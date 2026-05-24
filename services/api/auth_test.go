package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/auth"
)

// TestNewAuthSigner_RejectsDevPlaceholderWithoutOptIn verifies the
// defence-in-depth guard against operators copying .env.example to a
// real deployment without rotating KAPP_JWT_SECRET. The literal dev
// secret is recognised by string-equality with the same constant the
// .env.example file ships; without KAPP_ALLOW_DEV_JWT_SECRET=1, the
// signer constructor refuses to build a Signer keyed on that value
// so the API refuses to boot.
//
// The test sets KAPP_JWT_SECRET via t.Setenv and explicitly unsets
// the opt-in flag so it exercises the rejection path regardless of
// what the surrounding process environment looks like.
func TestNewAuthSigner_RejectsDevPlaceholderWithoutOptIn(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", auth.DevPlaceholderJWTSecret)
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "")
	signer, err := newAuthSigner(context.TODO(), nil, auth.SignerProviderOptions{})
	if err == nil {
		t.Fatalf("newAuthSigner returned a signer with the dev placeholder + no opt-in; want refusal so misconfigured deployments fail loudly. got=%v", signer)
	}
	if !strings.Contains(err.Error(), "dev-only placeholder") {
		t.Errorf("error message does not name the dev placeholder; operators rely on this string to identify the misconfiguration. err=%v", err)
	}
	if !strings.Contains(err.Error(), "KAPP_ALLOW_DEV_JWT_SECRET") {
		t.Errorf("error message does not point at the opt-in flag operators must set; this is the actionable remediation hint. err=%v", err)
	}
}

// TestNewAuthSigner_AcceptsDevPlaceholderWithOptIn verifies the
// happy path for the dev compose stack: when KAPP_ALLOW_DEV_JWT_SECRET=1
// is set alongside the placeholder, newAuthSigner builds the signer
// without complaint. The .env.example bundles both env vars together
// so `make dev` continues to boot.
func TestNewAuthSigner_AcceptsDevPlaceholderWithOptIn(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", auth.DevPlaceholderJWTSecret)
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "1")
	signer, err := newAuthSigner(context.TODO(), nil, auth.SignerProviderOptions{})
	if err != nil {
		t.Fatalf("newAuthSigner refused the dev placeholder with opt-in set; want success so `make dev` boots. err=%v", err)
	}
	if signer == nil {
		t.Fatal("newAuthSigner returned nil signer without an error; want a usable signer")
	}
}

// TestNewAuthSigner_AcceptsRotatedSecret verifies the production path:
// any secret that is NOT the literal dev placeholder passes through
// without needing the opt-in flag. This is the path real deployments
// take after rotating the secret to a freshly generated value.
func TestNewAuthSigner_AcceptsRotatedSecret(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", "this-is-a-rotated-secret-not-the-placeholder-12345678")
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "")
	signer, err := newAuthSigner(context.TODO(), nil, auth.SignerProviderOptions{})
	if err != nil {
		t.Fatalf("newAuthSigner rejected a rotated secret; the dev-placeholder check should only fire on string equality. err=%v", err)
	}
	if signer == nil {
		t.Fatal("newAuthSigner returned nil signer without an error; want a usable signer for the rotated-secret path")
	}
}
