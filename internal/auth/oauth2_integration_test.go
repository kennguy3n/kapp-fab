package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// mockIAMCore is a minimal stand-in for iam-core's OAuth2/OIDC surface
// that enforces the parts of the protocol the kapp-fab client relies
// on: PKCE S256 binding between authorize and token, one-time
// authorization codes, refresh-token rotation, and JWKS-published
// signing keys. It lets the integration test drive the real
// OAuth2Client + JWKSValidator through a complete login → callback →
// refresh sequence without a live iam-core.
type mockIAMCore struct {
	t        *testing.T
	srv      *httptest.Server
	issuer   string
	clientID string
	kid      string
	priv     *rsa.PrivateKey

	mu       sync.Mutex
	codes    map[string]codeGrant // code -> grant
	refresh  map[string]bool      // currently-valid refresh tokens
	tenantID uuid.UUID
	userID   uuid.UUID
}

type codeGrant struct {
	challenge string
	nonce     string
	redirect  string
}

func newMockIAMCore(t *testing.T, clientID string) *mockIAMCore {
	t.Helper()
	m := &mockIAMCore{
		t:        t,
		clientID: clientID,
		kid:      "mock-iam-kid",
		priv:     testRSAKey(t),
		codes:    map[string]codeGrant{},
		refresh:  map[string]bool{},
		tenantID: uuid.New(),
		userID:   uuid.New(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", m.handleJWKS)
	mux.HandleFunc("/oauth2/token", m.handleToken)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m.issuer = srv.URL
	m.srv = srv
	return m
}

func (m *mockIAMCore) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write(jwksJSON(m.t, m.kid, &m.priv.PublicKey))
}

// issueCode simulates the user authenticating at iam-core's authorize
// endpoint: it records the PKCE challenge + nonce bound to a fresh
// authorization code, exactly as a real AS would after login.
func (m *mockIAMCore) issueCode(challenge, nonce, redirect string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	code := "code-" + uuid.NewString()
	m.codes[code] = codeGrant{challenge: challenge, nonce: nonce, redirect: redirect}
	return code
}

func (m *mockIAMCore) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		m.tokenFromCode(w, r)
	case "refresh_token":
		m.tokenFromRefresh(w, r)
	default:
		writeOAuthErr(w, "unsupported_grant_type")
	}
}

func (m *mockIAMCore) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	verifier := r.PostForm.Get("code_verifier")
	m.mu.Lock()
	grant, ok := m.codes[code]
	if ok {
		delete(m.codes, code) // codes are single-use
	}
	m.mu.Unlock()
	if !ok {
		writeOAuthErr(w, "invalid_grant") // unknown or replayed code
		return
	}
	// Enforce PKCE: BASE64URL(SHA256(verifier)) must equal the stored
	// challenge. This is the crux of the integration contract.
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
		writeOAuthErr(w, "invalid_grant")
		return
	}
	m.writeTokens(w, grant.nonce)
}

func (m *mockIAMCore) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	rt := r.PostForm.Get("refresh_token")
	m.mu.Lock()
	valid := m.refresh[rt]
	if valid {
		delete(m.refresh, rt) // rotation: old token is invalidated
	}
	m.mu.Unlock()
	if !valid {
		writeOAuthErr(w, "invalid_grant") // reuse detection
		return
	}
	m.writeTokens(w, "")
}

func (m *mockIAMCore) writeTokens(w http.ResponseWriter, nonce string) {
	now := time.Now()
	idClaims := map[string]any{
		"iss":            m.issuer,
		"aud":            m.clientID,
		"sub":            m.userID.String(),
		"email":          "user@example.com",
		"kapp_tenant_id": m.tenantID.String(),
		"kapp_user_id":   m.userID.String(),
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	if nonce != "" {
		idClaims["nonce"] = nonce
	}
	idTok := mintRS256(m.t, m.priv, m.kid, idClaims)
	accessTok := mintRS256(m.t, m.priv, m.kid, accessClaims(m.issuer, m.tenantID, m.userID))

	rt := "rt-" + uuid.NewString()
	m.mu.Lock()
	m.refresh[rt] = true
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  accessTok,
		"token_type":    "Bearer",
		"expires_in":    3600,
		"refresh_token": rt,
		"id_token":      idTok,
		"scope":         "openid profile email",
	})
}

func writeOAuthErr(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// TestOAuth2_FullAuthCodeFlow_WithMockIAMCore exercises the entire
// interactive login the auth routes drive: build a PKCE auth request,
// simulate the user authenticating at iam-core (which binds the
// challenge to a code), exchange the code + verifier for tokens,
// validate the id_token's nonce, validate the access token against
// JWKS, then rotate the refresh token and confirm the old one is
// rejected (reuse detection).
func TestOAuth2_FullAuthCodeFlow_WithMockIAMCore(t *testing.T) {
	const clientID = "kapp-web"
	m := newMockIAMCore(t, clientID)

	validator := newValidatorForServer(t, m.issuer, "")
	client, err := NewOAuth2Client(OAuth2Config{
		Issuer:       m.issuer,
		ClientID:     clientID,
		ClientSecret: "s3cret",
		RedirectURI:  "https://kapp.example.com/api/v1/auth/callback",
		HTTPClient:   m.srv.Client(),
	}, validator)
	if err != nil {
		t.Fatalf("NewOAuth2Client: %v", err)
	}

	// 1. /login: build the PKCE authorization request.
	authReq, err := client.NewAuthRequest("offline_access")
	if err != nil {
		t.Fatalf("NewAuthRequest: %v", err)
	}
	u, _ := url.Parse(authReq.URL)
	challenge := u.Query().Get("code_challenge")

	// 2. User authenticates at iam-core, which issues a code bound to
	//    the challenge + nonce.
	code := m.issueCode(challenge, authReq.Nonce, client.cfg.RedirectURI)

	// 3. /callback: exchange code + verifier for tokens.
	tokens, err := client.Exchange(context.Background(), code, authReq.Verifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// 4. Validate the id_token nonce binding (defeats injection).
	idClaims, err := client.VerifyIDToken(tokens.IDToken, authReq.Nonce)
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if idClaims.Email != "user@example.com" {
		t.Errorf("id_token email = %q", idClaims.Email)
	}

	// 5. The access token validates against JWKS and maps to the
	//    Kapp tenant/user (what the API middleware will do per
	//    request).
	apiClaims, err := validator.Verify(tokens.AccessToken)
	if err != nil {
		t.Fatalf("validator.Verify(access): %v", err)
	}
	if apiClaims.TenantID != m.tenantID || apiClaims.UserID != m.userID {
		t.Fatalf("access claims mismatch: tid=%s uid=%s", apiClaims.TenantID, apiClaims.UserID)
	}

	// 6. Replaying the same code must fail (single-use).
	if _, err := client.Exchange(context.Background(), code, authReq.Verifier); err == nil {
		t.Fatal("code replay accepted; want invalid_grant")
	}

	// 7. Refresh rotates the token; the new one works and the old one
	//    is now rejected by reuse detection.
	rotated, err := client.Refresh(context.Background(), tokens.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rotated.RefreshToken == tokens.RefreshToken || rotated.RefreshToken == "" {
		t.Fatalf("refresh token not rotated: old=%q new=%q", tokens.RefreshToken, rotated.RefreshToken)
	}
	if _, err := client.Refresh(context.Background(), tokens.RefreshToken); err == nil {
		t.Fatal("reused (old) refresh token accepted; want invalid_grant")
	}
}

// TestOAuth2_FullFlow_WrongVerifierRejected proves PKCE actually
// protects the exchange: a code obtained for one challenge cannot be
// redeemed with a different verifier (the stolen-code scenario).
func TestOAuth2_FullFlow_WrongVerifierRejected(t *testing.T) {
	const clientID = "kapp-web"
	m := newMockIAMCore(t, clientID)
	validator := newValidatorForServer(t, m.issuer, "")
	client, err := NewOAuth2Client(OAuth2Config{
		Issuer:      m.issuer,
		ClientID:    clientID,
		RedirectURI: "https://kapp.example.com/api/v1/auth/callback",
		HTTPClient:  m.srv.Client(),
	}, validator)
	if err != nil {
		t.Fatalf("NewOAuth2Client: %v", err)
	}
	authReq, err := client.NewAuthRequest()
	if err != nil {
		t.Fatalf("NewAuthRequest: %v", err)
	}
	u, _ := url.Parse(authReq.URL)
	code := m.issueCode(u.Query().Get("code_challenge"), authReq.Nonce, client.cfg.RedirectURI)

	if _, err := client.Exchange(context.Background(), code, "attacker-supplied-verifier"); err == nil {
		t.Fatal("exchange with wrong PKCE verifier accepted; want invalid_grant")
	}
}
