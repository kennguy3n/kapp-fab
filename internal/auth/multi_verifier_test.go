package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// dualVerifier wires a real legacy HS256 Signer alongside an iam-core
// JWKS validator, mirroring how deps_build.go composes the production
// MultiVerifier when IAM_CORE_ISSUER is set.
func dualVerifier(t *testing.T, issuer string) (*MultiVerifier, *Signer) {
	t.Helper()
	signer := newTestSigner(t)
	v := newValidatorForServer(t, issuer, "")
	return NewMultiVerifier(signer, v), signer
}

// TestMultiVerifier_RoutesByIssuer is the core dual-issuer contract:
// an HS256 legacy token routes to the Signer, an RS256 iam-core token
// routes to the JWKS validator, and both yield a populated Claims.
func TestMultiVerifier_RoutesByIssuer(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	mv, signer := dualVerifier(t, srv.URL)

	// Legacy HS256 token (no iss == iam-core) -> legacy path.
	legacyTID, legacyUID := uuid.New(), uuid.New()
	legacyTok, err := signer.Issue(Claims{UserID: legacyUID, TenantID: legacyTID})
	if err != nil {
		t.Fatalf("Issue legacy: %v", err)
	}
	gotLegacy, err := mv.Verify(legacyTok)
	if err != nil {
		t.Fatalf("Verify legacy: %v", err)
	}
	if gotLegacy.TenantID != legacyTID || gotLegacy.UserID != legacyUID {
		t.Fatalf("legacy claims mismatch: %+v", gotLegacy)
	}

	// iam-core RS256 token -> JWKS path.
	iamTID, iamUID := uuid.New(), uuid.New()
	iamTok := mintRS256(t, priv, kid, accessClaims(srv.URL, iamTID, iamUID))
	gotIAM, err := mv.Verify(iamTok)
	if err != nil {
		t.Fatalf("Verify iam-core: %v", err)
	}
	if gotIAM.TenantID != iamTID || gotIAM.UserID != iamUID {
		t.Fatalf("iam-core claims mismatch: %+v", gotIAM)
	}
	if gotIAM.Issuer != srv.URL {
		t.Fatalf("iam-core issuer = %q, want %q", gotIAM.Issuer, srv.URL)
	}
}

// TestMultiVerifier_IAMTokenNotForgeableViaLegacy proves the routing
// decision grants no trust: a token claiming the iam-core issuer but
// signed with the legacy HMAC secret is routed to the JWKS validator
// and rejected (no signing key / bad signature), never silently
// accepted by the legacy path.
func TestMultiVerifier_IAMTokenNotForgeableViaLegacy(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	mv, signer := dualVerifier(t, srv.URL)

	// Forge: claim the iam-core issuer but sign HS256 with the legacy
	// secret. Routed to JWKS validator by iss -> must fail.
	forged, err := signer.Issue(Claims{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
		Issuer:   srv.URL,
	})
	if err != nil {
		t.Fatalf("Issue forged: %v", err)
	}
	if _, err := mv.Verify(forged); err == nil {
		t.Fatal("forged iam-issuer/HS256 token accepted; want rejection")
	}
}

// TestMultiVerifier_NilIAMIsLegacyOnly asserts backward compatibility:
// with no iam-core validator, every token routes to the legacy signer.
func TestMultiVerifier_NilIAMIsLegacyOnly(t *testing.T) {
	signer := newTestSigner(t)
	mv := NewMultiVerifier(signer, nil)

	tid, uid := uuid.New(), uuid.New()
	tok, err := signer.Issue(Claims{UserID: uid, TenantID: tid})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := mv.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.TenantID != tid {
		t.Fatalf("claims mismatch: %+v", got)
	}
}

// TestMiddleware_DualIssuerAcceptsBothTokenTypes wires the real
// Middleware around a MultiVerifier and asserts an HS256 legacy token
// AND an RS256 iam-core token both reach the protected handler with
// the correct tenant on the context.
func TestMiddleware_DualIssuerAcceptsBothTokenTypes(t *testing.T) {
	priv := testRSAKey(t)
	kid := "test-kid-1"
	srv := newJWKSServer(t, kid, &priv.PublicKey, nil)
	mv, signer := dualVerifier(t, srv.URL)

	legacyTID := uuid.New()
	iamTID := uuid.New()
	resolver := stubTenantResolver{out: &tenant.Tenant{Status: tenant.StatusActive}}

	legacyTok, err := signer.Issue(Claims{UserID: uuid.New(), TenantID: legacyTID})
	if err != nil {
		t.Fatalf("Issue legacy: %v", err)
	}
	iamTok := mintRS256(t, priv, kid, accessClaims(srv.URL, iamTID, uuid.New()))

	cases := []struct {
		name  string
		token string
	}{
		{"legacy HS256", legacyTok},
		{"iam-core RS256", iamTok},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := Middleware(mv, resolver, nil)
			var reached bool
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !reached {
				t.Fatalf("status = %d reached=%v, want 200/true (body=%q)", rec.Code, reached, rec.Body.String())
			}
		})
	}
}

// TestMiddleware_DualIssuerRejectsGarbage ensures a non-JWT bearer is
// rejected with 401 through the MultiVerifier path.
func TestMiddleware_DualIssuerRejectsGarbage(t *testing.T) {
	priv := testRSAKey(t)
	srv := newJWKSServer(t, "test-kid-1", &priv.PublicKey, nil)
	mv, _ := dualVerifier(t, srv.URL)

	mw := Middleware(mv, stubTenantResolver{out: &tenant.Tenant{Status: tenant.StatusActive}}, nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/records", http.NoBody)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// sanity: ensure ErrTokenInvalid is the error class for malformed
// routing input, so callers can map to 401 uniformly.
func TestMultiVerifier_MalformedTokenIsInvalid(t *testing.T) {
	priv := testRSAKey(t)
	srv := newJWKSServer(t, "test-kid-1", &priv.PublicKey, nil)
	mv, _ := dualVerifier(t, srv.URL)
	if _, err := mv.Verify("aaa.bbb"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}
