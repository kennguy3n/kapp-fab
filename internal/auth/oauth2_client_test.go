package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestOAuth2Client(t *testing.T, issuer string, hc *http.Client) *OAuth2Client {
	t.Helper()
	v := newValidatorForServer(t, issuer, "")
	c, err := NewOAuth2Client(OAuth2Config{
		Issuer:       issuer,
		ClientID:     "kapp-web",
		ClientSecret: "s3cret",
		RedirectURI:  "https://kapp.example.com/api/v1/auth/callback",
		HTTPClient:   hc,
	}, v)
	if err != nil {
		t.Fatalf("NewOAuth2Client: %v", err)
	}
	return c
}

// TestOAuth2_NewAuthRequest_PKCE asserts the authorize URL carries a
// correct S256 PKCE challenge derived from the returned verifier,
// plus state/nonce and the mandatory OAuth2/OIDC params.
func TestOAuth2_NewAuthRequest_PKCE(t *testing.T) {
	c := newTestOAuth2Client(t, "https://auth.example.com", nil)
	req, err := c.NewAuthRequest()
	if err != nil {
		t.Fatalf("NewAuthRequest: %v", err)
	}
	if req.State == "" || req.Nonce == "" || req.Verifier == "" {
		t.Fatalf("expected non-empty state/nonce/verifier, got %+v", req)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := u.Query()
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q, want code", got)
	}
	if got := q.Get("client_id"); got != "kapp-web" {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if got := q.Get("state"); got != req.State {
		t.Errorf("state in URL = %q, want %q", got, req.State)
	}
	if got := q.Get("nonce"); got != req.Nonce {
		t.Errorf("nonce in URL = %q, want %q", got, req.Nonce)
	}
	// The challenge must equal BASE64URL(SHA256(verifier)).
	sum := sha256.Sum256([]byte(req.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := q.Get("code_challenge"); got != want {
		t.Errorf("code_challenge = %q, want %q", got, want)
	}
	// The verifier must never appear in the redirect URL.
	if strings.Contains(req.URL, req.Verifier) {
		t.Error("PKCE verifier leaked into authorize URL")
	}
}

func TestOAuth2_NewAuthRequest_ExtraScopesDeduped(t *testing.T) {
	c := newTestOAuth2Client(t, "https://auth.example.com", nil)
	req, err := c.NewAuthRequest("offline_access", "openid")
	if err != nil {
		t.Fatalf("NewAuthRequest: %v", err)
	}
	u, _ := url.Parse(req.URL)
	scope := u.Query().Get("scope")
	parts := strings.Fields(scope)
	seen := map[string]int{}
	for _, p := range parts {
		seen[p]++
	}
	if seen["openid"] != 1 {
		t.Errorf("openid appears %d times in scope %q, want 1", seen["openid"], scope)
	}
	if seen["offline_access"] != 1 {
		t.Errorf("offline_access missing/duplicated in scope %q", scope)
	}
}

// TestOAuth2_Exchange drives a full code→token exchange against a mock
// token endpoint, asserting the PKCE verifier and client credentials
// are posted and the parsed TokenResponse is returned.
func TestOAuth2_Exchange(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"

	var gotForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwksJSON(t, kid, &priv.PublicKey))
	})
	var issuer string
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		now := time.Now()
		idTok := mintRS256(t, priv, kid, map[string]any{
			"iss":   issuer,
			"aud":   "kapp-web",
			"sub":   uuid.New().String(),
			"nonce": "nonce-xyz",
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-123",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "rt-456",
			"id_token":      idTok,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL

	c := newTestOAuth2Client(t, issuer, srv.Client())
	tr, err := c.Exchange(context.Background(), "auth-code", "pkce-verifier")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tr.AccessToken != "at-123" || tr.RefreshToken != "rt-456" {
		t.Fatalf("unexpected token response %+v", tr)
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", gotForm.Get("grant_type"))
	}
	if gotForm.Get("code") != "auth-code" {
		t.Errorf("code = %q", gotForm.Get("code"))
	}
	if gotForm.Get("code_verifier") != "pkce-verifier" {
		t.Errorf("code_verifier = %q, want pkce-verifier", gotForm.Get("code_verifier"))
	}
	if gotForm.Get("client_id") != "kapp-web" || gotForm.Get("client_secret") != "s3cret" {
		t.Errorf("client auth not posted: id=%q secret=%q", gotForm.Get("client_id"), gotForm.Get("client_secret"))
	}

	// And the returned id_token validates against the mock JWKS.
	if _, err := c.VerifyIDToken(tr.IDToken, "nonce-xyz"); err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
}

func TestOAuth2_Exchange_PropagatesOAuth2Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "authorization code expired",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newTestOAuth2Client(t, srv.URL, srv.Client())
	_, err := c.Exchange(context.Background(), "stale-code", "verifier")
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("Exchange err = %v, want invalid_grant surfaced", err)
	}
}

func TestOAuth2_Refresh(t *testing.T) {
	var gotForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-new",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "rt-rotated",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newTestOAuth2Client(t, srv.URL, srv.Client())
	tr, err := c.Refresh(context.Background(), "rt-old")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if gotForm.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", gotForm.Get("grant_type"))
	}
	if gotForm.Get("refresh_token") != "rt-old" {
		t.Errorf("refresh_token sent = %q, want rt-old", gotForm.Get("refresh_token"))
	}
	if tr.RefreshToken != "rt-rotated" {
		t.Errorf("rotated refresh token = %q, want rt-rotated", tr.RefreshToken)
	}
}

func TestOAuth2_Exchange_RequiresCodeAndVerifier(t *testing.T) {
	c := newTestOAuth2Client(t, "https://auth.example.com", nil)
	if _, err := c.Exchange(context.Background(), "", "v"); err == nil {
		t.Error("expected error for empty code")
	}
	if _, err := c.Exchange(context.Background(), "code", ""); err == nil {
		t.Error("expected error for empty verifier")
	}
}

func TestOAuth2_LogoutURL(t *testing.T) {
	c := newTestOAuth2Client(t, "https://auth.example.com", nil)
	got := c.LogoutURL("https://kapp.example.com/login", "id-token-hint")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse logout url: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "kapp-web" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("post_logout_redirect_uri") != "https://kapp.example.com/login" {
		t.Errorf("post_logout_redirect_uri = %q", q.Get("post_logout_redirect_uri"))
	}
	if q.Get("id_token_hint") != "id-token-hint" {
		t.Errorf("id_token_hint = %q", q.Get("id_token_hint"))
	}
}

func TestOAuth2_NewClient_ValidatesConfig(t *testing.T) {
	v := newValidatorForServer(t, "https://auth.example.com", "")
	cases := []OAuth2Config{
		{ClientID: "c", RedirectURI: "r"},       // missing issuer
		{Issuer: "https://i", RedirectURI: "r"}, // missing client id
		{Issuer: "https://i", ClientID: "c"},    // missing redirect uri
	}
	for i, cfg := range cases {
		if _, err := NewOAuth2Client(cfg, v); err == nil {
			t.Errorf("case %d: expected config error, got nil", i)
		}
	}
	// nil validator is also an error.
	if _, err := NewOAuth2Client(OAuth2Config{Issuer: "https://i", ClientID: "c", RedirectURI: "r"}, nil); err == nil {
		t.Error("expected error for nil validator")
	}
}
