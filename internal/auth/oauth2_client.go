package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth2 endpoint paths under the iam-core issuer. Kept as constants
// so the derived URLs are auditable in one place and match iam-core's
// router exactly.
const (
	oauth2AuthorizePath = "/oauth2/authorize"
	oauth2TokenPath     = "/oauth2/token" //nolint:gosec // G101 false positive: this is an OAuth2 endpoint URL path, not a credential.
	oauth2UserInfoPath  = "/oauth2/userinfo"
	oauth2RevokePath    = "/oauth2/revoke"
	oauth2LogoutPath    = "/oauth2/logout"
)

// defaultOIDCScopes is the scope set requested for an interactive
// login when the caller does not override it. `openid` is mandatory
// for OIDC (it triggers id_token issuance); profile + email populate
// the user record on first login.
var defaultOIDCScopes = []string{"openid", "profile", "email"}

// OAuth2Config configures the authorization-code client that brokers
// interactive logins between a Kapp web/API client and iam-core.
type OAuth2Config struct {
	// Issuer is the iam-core base URL (no trailing slash).
	Issuer string
	// ClientID is Kapp's confidential OAuth2 client registered in
	// iam-core.
	ClientID string
	// ClientSecret authenticates the confidential client at the
	// token endpoint. Sent via client_secret_post.
	ClientSecret string
	// RedirectURI is the registered callback. It must match what is
	// sent on the authorize request and what iam-core has on file, or
	// the token exchange is rejected.
	RedirectURI string
	// Audience requests an access token minted for a specific API
	// (iam-core's `audience` parameter). Optional; when set it is
	// forwarded on both authorize and token requests.
	Audience string
	// Scopes overrides defaultOIDCScopes when non-empty.
	Scopes []string
	// HTTPClient performs token/userinfo/revoke calls. Zero selects a
	// client with a 10s timeout.
	HTTPClient *http.Client
}

// OAuth2Client implements the OAuth2 Authorization Code flow with
// PKCE (S256) against iam-core. It validates received id_tokens
// through the shared JWKSValidator so signature/issuer/nonce checks
// use the same cached key set as the API middleware.
//
// The client is stateless: per-login transient state (PKCE verifier,
// state, nonce) is produced by NewAuthRequest and must be persisted by
// the caller (encrypted cookie / server-side session) until the
// callback. The client never stores it.
type OAuth2Client struct {
	cfg        OAuth2Config
	validator  *JWKSValidator
	httpClient *http.Client
	scopes     []string
}

// NewOAuth2Client wires the client. validator is required: it is what
// the callback uses to validate the id_token. A missing issuer,
// client id, or redirect URI is a configuration error.
func NewOAuth2Client(cfg OAuth2Config, validator *JWKSValidator) (*OAuth2Client, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("auth: oauth2 client requires an issuer")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("auth: oauth2 client requires a client id")
	}
	if strings.TrimSpace(cfg.RedirectURI) == "" {
		return nil, errors.New("auth: oauth2 client requires a redirect uri")
	}
	if validator == nil {
		return nil, errors.New("auth: oauth2 client requires a jwks validator")
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultOIDCScopes
	}
	return &OAuth2Client{
		cfg:        cfg,
		validator:  validator,
		httpClient: httpClient,
		scopes:     scopes,
	}, nil
}

// AuthRequest is the transient per-login state the caller must stash
// (e.g. in a signed, short-lived cookie) between the authorize
// redirect and the callback. State defeats CSRF on the callback;
// Nonce binds the eventual id_token to this exact request; Verifier is
// the PKCE secret proving the callback came from the same client that
// initiated the flow.
type AuthRequest struct {
	URL      string // the full iam-core authorize URL to redirect to
	State    string
	Nonce    string
	Verifier string // PKCE code_verifier — keep secret, never logged
}

// NewAuthRequest builds an authorization-code request with PKCE. It
// generates a fresh state, nonce, and PKCE verifier/challenge, and
// returns the authorize URL plus the secrets the caller must persist
// for the callback. extraScopes are appended to the configured scope
// set (deduped), letting a caller request e.g. `offline_access` to get
// a refresh token without changing global config.
func (c *OAuth2Client) NewAuthRequest(extraScopes ...string) (*AuthRequest, error) {
	state, err := randomURLToken(32)
	if err != nil {
		return nil, fmt.Errorf("auth: generate state: %w", err)
	}
	nonce, err := randomURLToken(32)
	if err != nil {
		return nil, fmt.Errorf("auth: generate nonce: %w", err)
	}
	verifier, err := randomURLToken(48) // 64 chars base64url — within RFC 7636's 43..128
	if err != nil {
		return nil, fmt.Errorf("auth: generate pkce verifier: %w", err)
	}
	challenge := pkceChallengeS256(verifier)

	scopes := dedupeScopes(c.scopes, extraScopes)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if c.cfg.Audience != "" {
		q.Set("audience", c.cfg.Audience)
	}
	return &AuthRequest{
		URL:      c.cfg.Issuer + oauth2AuthorizePath + "?" + q.Encode(),
		State:    state,
		Nonce:    nonce,
		Verifier: verifier,
	}, nil
}

// TokenResponse is the iam-core token-endpoint reply (RFC 6749 §5.1
// plus OIDC id_token). Scope echoes the granted scopes, which may be
// narrower than requested.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
}

// Exchange swaps an authorization code for tokens, sending the PKCE
// verifier so iam-core can confirm the callback originated from the
// same client that began the flow. The redirect URI must match the
// authorize request exactly.
func (c *OAuth2Client) Exchange(ctx context.Context, code, codeVerifier string) (*TokenResponse, error) {
	if code == "" {
		return nil, errors.New("auth: authorization code required")
	}
	if codeVerifier == "" {
		return nil, errors.New("auth: pkce verifier required")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.cfg.RedirectURI)
	form.Set("code_verifier", codeVerifier)
	return c.tokenRequest(ctx, form)
}

// Refresh exchanges a refresh token for a fresh token set. iam-core
// rotates refresh tokens — the response's RefreshToken (when present)
// supersedes the presented one and the old token is invalidated
// server-side, so callers MUST persist the returned refresh token and
// discard the old one. Reusing a rotated token triggers iam-core's
// reuse detection and revokes the family.
func (c *OAuth2Client) Refresh(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	if refreshToken == "" {
		return nil, errors.New("auth: refresh token required")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	// Re-send scope so a narrowed grant is explicit; omit to inherit
	// the original grant. We inherit by leaving scope unset.
	return c.tokenRequest(ctx, form)
}

// VerifyIDToken validates an id_token from a TokenResponse against the
// expected nonce (the one stored in the AuthRequest). It is a thin
// pass-through to the shared validator with the client's client_id as
// the expected audience.
func (c *OAuth2Client) VerifyIDToken(idToken, expectedNonce string) (*IDTokenClaims, error) {
	if idToken == "" {
		return nil, errors.New("auth: id_token missing from token response")
	}
	return c.validator.VerifyIDToken(idToken, c.cfg.ClientID, expectedNonce)
}

// UserInfo calls the OIDC userinfo endpoint with the access token and
// returns the raw claim map. Used as an optional enrichment step or by
// the /userinfo proxy route.
func (c *OAuth2Client) UserInfo(ctx context.Context, accessToken string) (map[string]json.RawMessage, error) {
	if accessToken == "" {
		return nil, errors.New("auth: access token required for userinfo")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Issuer+oauth2UserInfoPath, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("auth: build userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: userinfo request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: userinfo status %d", resp.StatusCode)
	}
	var out map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("auth: decode userinfo: %w", err)
	}
	return out, nil
}

// Revoke best-effort revokes a token at iam-core's revocation
// endpoint (RFC 7009). tokenTypeHint is "refresh_token" or
// "access_token". A non-200 is returned as an error so callers can
// log it, but per the RFC the local session should still be cleared
// regardless of the revocation result.
func (c *OAuth2Client) Revoke(ctx context.Context, token, tokenTypeHint string) error {
	if token == "" {
		return errors.New("auth: token required for revoke")
	}
	form := url.Values{}
	form.Set("token", token)
	if tokenTypeHint != "" {
		form.Set("token_type_hint", tokenTypeHint)
	}
	form.Set("client_id", c.cfg.ClientID)
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Issuer+oauth2RevokePath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("auth: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("auth: revoke request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: revoke status %d", resp.StatusCode)
	}
	return nil
}

// LogoutURL builds iam-core's end-session URL. postLogoutRedirectURI,
// when non-empty, is where iam-core sends the browser after clearing
// its session; idTokenHint (the user's id_token) lets iam-core skip
// the logout confirmation prompt.
func (c *OAuth2Client) LogoutURL(postLogoutRedirectURI, idTokenHint string) string {
	q := url.Values{}
	q.Set("client_id", c.cfg.ClientID)
	if postLogoutRedirectURI != "" {
		q.Set("post_logout_redirect_uri", postLogoutRedirectURI)
	}
	if idTokenHint != "" {
		q.Set("id_token_hint", idTokenHint)
	}
	return c.cfg.Issuer + oauth2LogoutPath + "?" + q.Encode()
}

// tokenRequest posts a form to the token endpoint with
// client_secret_post auth and decodes the JSON token response. OAuth2
// error responses (RFC 6749 §5.2) are surfaced with their `error`
// code so callers can distinguish e.g. invalid_grant (expired/replayed
// code) from transport failures.
func (c *OAuth2Client) tokenRequest(ctx context.Context, form url.Values) (*TokenResponse, error) {
	form.Set("client_id", c.cfg.ClientID)
	if c.cfg.ClientSecret != "" {
		form.Set("client_secret", c.cfg.ClientSecret)
	}
	if c.cfg.Audience != "" && form.Get("audience") == "" {
		form.Set("audience", c.cfg.Audience)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Issuer+oauth2TokenPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("auth: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseOAuth2Error(resp.StatusCode, body)
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("auth: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("auth: token response missing access_token")
	}
	return &tr, nil
}

// parseOAuth2Error turns a non-200 token response into a descriptive
// error, extracting the RFC 6749 `error`/`error_description` fields
// when the body is the standard JSON error object.
func parseOAuth2Error(status int, body []byte) error {
	var oerr struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &oerr); err == nil && oerr.Error != "" {
		if oerr.ErrorDescription != "" {
			return fmt.Errorf("auth: token endpoint error %q: %s", oerr.Error, oerr.ErrorDescription)
		}
		return fmt.Errorf("auth: token endpoint error %q", oerr.Error)
	}
	return fmt.Errorf("auth: token endpoint status %d", status)
}

// pkceChallengeS256 derives the S256 PKCE code_challenge from a
// verifier: BASE64URL(SHA256(verifier)) with no padding (RFC 7636 §4.2).
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomURLToken returns nBytes of CSPRNG output base64url-encoded
// without padding, suitable for state/nonce/PKCE verifier values.
func randomURLToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func dedupeScopes(base, extra []string) []string {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, s := range append(append([]string{}, base...), extra...) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
