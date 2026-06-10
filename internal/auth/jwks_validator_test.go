package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- shared RS256 / JWKS test fixtures (package-internal) ------------
//
// These helpers mint iam-core-shaped RS256 tokens and stand up a JWKS
// endpoint backed by an in-test RSA key, so the validator/client/
// middleware tests exercise the real signature + JWKS-fetch paths
// without a live iam-core. They are deliberately dependency-free
// (manual JWS assembly) so they match exactly what the validator
// verifies.

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

// jwksJSON renders the public half of key as a single-key JWKS
// document with the given kid.
func jwksJSON(t *testing.T, kid string, pub *rsa.PublicKey) []byte {
	t.Helper()
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	doc := map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(eBytes),
		}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return b
}

// newJWKSServer serves the key's JWKS at /.well-known/jwks.json and
// counts how many times it was fetched (to assert caching). The
// returned server's URL is a usable issuer base.
func newJWKSServer(t *testing.T, kid string, pub *rsa.PublicKey, hits *int64) *httptest.Server {
	t.Helper()
	body := jwksJSON(t, kid, pub)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mintRS256 assembles a compact JWS signed with RS256 over the given
// claims, using kid in the header. A blank kid omits the header field.
func mintRS256(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	return mintSigned(t, priv, header, claims)
}

func mintSigned(t *testing.T, priv *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// accessClaims builds a minimal-but-valid iam-core access-token claim
// set for issuer iss, tenant tid, user uid, expiring in one hour.
func accessClaims(iss string, tid, uid uuid.UUID) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":            iss,
		"sub":            uid.String(),
		"kapp_tenant_id": tid.String(),
		"kapp_user_id":   uid.String(),
		"iat":            now.Unix(),
		"nbf":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(time.Hour).Unix(),
		"jti":            "jti-" + uid.String(),
	}
}

func newValidatorForServer(t *testing.T, issuer, audience string) *JWKSValidator {
	t.Helper()
	v, err := NewJWKSValidator(JWKSValidatorConfig{
		Issuer:   issuer,
		Audience: audience,
	})
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}
	return v
}

// --- tests -----------------------------------------------------------

func TestJWKSValidator_VerifyValidToken(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)

	v := newValidatorForServer(t, srv.URL, "")
	tid, uid := uuid.New(), uuid.New()
	tok := mintRS256(t, priv, kid, accessClaims(srv.URL, tid, uid))

	claims, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.TenantID != tid {
		t.Errorf("TenantID = %s, want %s", claims.TenantID, tid)
	}
	if claims.UserID != uid {
		t.Errorf("UserID = %s, want %s", claims.UserID, uid)
	}
	if claims.Issuer != srv.URL {
		t.Errorf("Issuer = %s, want %s", claims.Issuer, srv.URL)
	}
}

func TestJWKSValidator_RejectsHS256AlgConfusion(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	// Forge an HS256 token using the RSA public modulus bytes as the
	// HMAC secret — the classic alg-confusion attack. The validator
	// must refuse to even consider an HS* alg.
	header := map[string]any{"alg": "HS256", "typ": "JWT", "kid": kid}
	claims := accessClaims(srv.URL, uuid.New(), uuid.New())
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(cb)
	// (signature content is irrelevant — rejection happens at alg gate)
	tok := signingInput + "." + base64.RawURLEncoding.EncodeToString([]byte("x"))

	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("Verify err = %v, want ErrTokenSignature", err)
	}
}

func TestJWKSValidator_RejectsWrongIssuer(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	// Signature verifies (correct key) but `iss` is a different
	// issuer — must be rejected.
	claims := accessClaims("https://evil.example.com", uuid.New(), uuid.New())
	tok := mintRS256(t, priv, kid, claims)
	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify err = %v, want ErrTokenInvalid", err)
	}
}

func TestJWKSValidator_RejectsWrongAudience(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "kapp-api")

	claims := accessClaims(srv.URL, uuid.New(), uuid.New())
	claims["aud"] = "some-other-api"
	tok := mintRS256(t, priv, kid, claims)
	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify err = %v, want ErrTokenInvalid", err)
	}

	// Correct audience passes.
	claims["aud"] = "kapp-api"
	tok = mintRS256(t, priv, kid, claims)
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("Verify (correct aud): %v", err)
	}
}

func TestJWKSValidator_RejectsExpiredToken(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	claims := accessClaims(srv.URL, uuid.New(), uuid.New())
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	tok := mintRS256(t, priv, kid, claims)
	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("Verify err = %v, want ErrTokenExpired", err)
	}
}

func TestJWKSValidator_RejectsMissingTenantClaim(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	claims := accessClaims(srv.URL, uuid.New(), uuid.New())
	delete(claims, "kapp_tenant_id")
	tok := mintRS256(t, priv, kid, claims)
	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Verify err = %v, want ErrTokenInvalid", err)
	}
}

func TestJWKSValidator_RejectsUnknownKID(t *testing.T) {
	priv := testRSAKey(t)
	srv := newJWKSServer(t, "served-kid", &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	// Token references a kid the JWKS does not publish.
	tok := mintRS256(t, priv, "unknown-kid", accessClaims(srv.URL, uuid.New(), uuid.New()))
	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("Verify err = %v, want ErrTokenSignature", err)
	}
}

func TestJWKSValidator_RejectsBadSignature(t *testing.T) {
	priv := testRSAKey(t)
	other := testRSAKey(t)
	kid := "test-kid-1"
	// Serve priv's public key but sign with a different key.
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	tok := mintRS256(t, other, kid, accessClaims(srv.URL, uuid.New(), uuid.New()))
	if _, err := v.Verify(tok); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("Verify err = %v, want ErrTokenSignature", err)
	}
}

// TestJWKSValidator_CachesKeys asserts the steady-state hot path does
// not hit the network on every Verify: after the first fetch, repeated
// verifications are served from cache.
func TestJWKSValidator_CachesKeys(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	var hits int64
	srv := newJWKSServer(t, kid, &priv.PublicKey, &hits)
	v := newValidatorForServer(t, srv.URL, "")

	for i := 0; i < 5; i++ {
		tok := mintRS256(t, priv, kid, accessClaims(srv.URL, uuid.New(), uuid.New()))
		if _, err := v.Verify(tok); err != nil {
			t.Fatalf("Verify[%d]: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("jwks fetches = %d, want 1 (cache should serve subsequent verifies)", got)
	}
}

func TestJWKSValidator_VerifyIDTokenNonceBinding(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	now := time.Now()
	const clientID = "kapp-web"
	idClaims := map[string]any{
		"iss":   srv.URL,
		"aud":   clientID,
		"sub":   uuid.New().String(),
		"nonce": "nonce-abc",
		"email": "user@example.com",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	tok := mintRS256(t, priv, kid, idClaims)

	if _, err := v.VerifyIDToken(tok, clientID, "nonce-abc"); err != nil {
		t.Fatalf("VerifyIDToken (matching nonce): %v", err)
	}
	if _, err := v.VerifyIDToken(tok, clientID, "wrong-nonce"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyIDToken (wrong nonce) err = %v, want ErrTokenInvalid", err)
	}
	if _, err := v.VerifyIDToken(tok, "wrong-client", "nonce-abc"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyIDToken (wrong aud) err = %v, want ErrTokenInvalid", err)
	}
}

func TestJWKSValidator_RefreshPicksUpRotatedKey(t *testing.T) {
	priv := testRSAKey(t)
	kid := "rotated-kid"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	v := newValidatorForServer(t, srv.URL, "")

	if err := v.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	tok := mintRS256(t, priv, kid, accessClaims(srv.URL, uuid.New(), uuid.New()))
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("Verify after explicit refresh: %v", err)
	}
}
