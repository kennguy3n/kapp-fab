package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// m2mTokenRefreshSkew is how long before an M2M access token's actual
// expiry we treat it as stale and fetch a fresh one. It absorbs clock
// skew and request latency so a token never expires mid-flight on the
// Management API call it was fetched for.
const m2mTokenRefreshSkew = 30 * time.Second

// M2MConfig configures the client_credentials client used for
// server-to-server calls into iam-core's Management API (tenant/user
// provisioning, session revocation).
type M2MConfig struct {
	// Issuer is the iam-core base URL (no trailing slash).
	Issuer string
	// ClientID / ClientSecret are the dedicated M2M credentials —
	// distinct from the interactive OAuth2 client so the two can be
	// rotated and scoped independently.
	ClientID     string
	ClientSecret string
	// Audience identifies the Management API. iam-core mints the M2M
	// access token for this audience; it is required for the
	// client_credentials grant.
	Audience string
	// Scopes optionally narrows the M2M grant.
	Scopes []string
	// HTTPClient performs the token request. Zero selects a client
	// with a 10s timeout.
	HTTPClient *http.Client
	// now is a clock seam for tests. Zero selects time.Now.
	now func() time.Time
}

// M2MClient obtains and caches client_credentials access tokens for
// iam-core's Management API. A single cached token is shared across
// all callers and refreshed just before expiry; concurrent callers
// during a refresh coalesce onto one in-flight fetch rather than
// stampeding the token endpoint.
//
// Safe for concurrent use.
type M2MClient struct {
	cfg        M2MConfig
	httpClient *http.Client
	tokenURL   string
	now        func() time.Time

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewM2MClient wires the client. Issuer, ClientID, ClientSecret and
// Audience are all required — an M2M client with no audience cannot
// obtain a Management API token, so we fail fast rather than at first
// use.
func NewM2MClient(cfg M2MConfig) (*M2MClient, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("auth: m2m client requires an issuer")
	}
	if strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, errors.New("auth: m2m client requires client id and secret")
	}
	if strings.TrimSpace(cfg.Audience) == "" {
		return nil, errors.New("auth: m2m client requires an audience")
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	nowFn := cfg.now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &M2MClient{
		cfg:        cfg,
		httpClient: httpClient,
		tokenURL:   cfg.Issuer + oauth2TokenPath,
		now:        nowFn,
	}, nil
}

// Token returns a valid M2M access token, fetching a new one when the
// cache is empty or within m2mTokenRefreshSkew of expiry. The mutex is
// held across the network fetch so a burst of callers issues exactly
// one token request; this is the intended trade-off for a low-QPS
// control-plane credential (provisioning is rare relative to data-plane
// traffic).
func (c *M2MClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expiresAt.Add(-m2mTokenRefreshSkew)) {
		return c.token, nil
	}
	tok, expiresIn, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.token = tok
	c.expiresAt = c.now().Add(time.Duration(expiresIn) * time.Second)
	return tok, nil
}

// Invalidate clears the cached token so the next Token call fetches a
// fresh one. Call this after a Management API request fails with 401,
// in case iam-core rotated the signing key or revoked the grant before
// the cached token's nominal expiry.
func (c *M2MClient) Invalidate() {
	c.mu.Lock()
	c.token = ""
	c.expiresAt = time.Time{}
	c.mu.Unlock()
}

func (c *M2MClient) fetch(ctx context.Context) (token string, expiresIn int64, err error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.cfg.ClientID)
	form.Set("client_secret", c.cfg.ClientSecret)
	form.Set("audience", c.cfg.Audience)
	if len(c.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("auth: build m2m token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("auth: m2m token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("auth: read m2m token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, parseOAuth2Error(resp.StatusCode, body)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", 0, fmt.Errorf("auth: decode m2m token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", 0, errors.New("auth: m2m token response missing access_token")
	}
	if tr.ExpiresIn <= 0 {
		// Defend against an issuer that omits expires_in: cap at a
		// conservative 5 minutes so we re-fetch rather than caching a
		// token of unknown lifetime forever.
		tr.ExpiresIn = 300
	}
	return tr.AccessToken, tr.ExpiresIn, nil
}
