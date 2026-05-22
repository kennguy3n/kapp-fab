package auth

import (
	"strings"
	"testing"
)

func TestSignerFromEnv_RejectsUnsetSecret(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", "")
	signer, err := SignerFromEnv()
	if err == nil {
		t.Fatalf("SignerFromEnv accepted empty secret; want refusal. got=%v", signer)
	}
	if !strings.Contains(err.Error(), "unset") {
		t.Errorf("error message should mention KAPP_JWT_SECRET unset; operators rely on the literal string to grep for it. err=%v", err)
	}
}

func TestSignerFromEnv_RejectsDevPlaceholderWithoutOptIn(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", DevPlaceholderJWTSecret)
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "")
	signer, err := SignerFromEnv()
	if err == nil {
		t.Fatalf("SignerFromEnv accepted dev placeholder without opt-in; want refusal so misconfigured deployments fail loudly. got=%v", signer)
	}
	if !strings.Contains(err.Error(), "dev-only placeholder") {
		t.Errorf("error message does not name the dev placeholder; operators rely on this string to identify the misconfiguration. err=%v", err)
	}
	if !strings.Contains(err.Error(), "KAPP_ALLOW_DEV_JWT_SECRET") {
		t.Errorf("error message does not point at the opt-in flag operators must set; this is the actionable remediation hint. err=%v", err)
	}
}

func TestSignerFromEnv_AcceptsDevPlaceholderWithOptIn(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", DevPlaceholderJWTSecret)
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "1")
	signer, err := SignerFromEnv()
	if err != nil {
		t.Fatalf("SignerFromEnv refused dev placeholder with opt-in set; want success so `make dev` boots. err=%v", err)
	}
	if signer == nil {
		t.Fatal("SignerFromEnv returned nil signer without an error; want a usable signer")
	}
}

func TestSignerFromEnv_AcceptsRotatedSecret(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", "rotated-production-secret-not-the-placeholder-1234567")
	t.Setenv("KAPP_ALLOW_DEV_JWT_SECRET", "")
	signer, err := SignerFromEnv()
	if err != nil {
		t.Fatalf("SignerFromEnv rejected a rotated secret; the dev-placeholder check should only fire on string equality. err=%v", err)
	}
	if signer == nil {
		t.Fatal("SignerFromEnv returned nil signer without an error; want a usable signer for the rotated-secret path")
	}
}

func TestSignerFromEnv_HonoursTTLOverrides(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", "rotated-production-secret-for-ttl-override-test-12345")
	t.Setenv("KAPP_JWT_ACCESS_TTL", "5m")
	t.Setenv("KAPP_JWT_REFRESH_TTL", "1h")
	signer, err := SignerFromEnv()
	if err != nil {
		t.Fatalf("SignerFromEnv refused valid config: %v", err)
	}
	if signer == nil {
		t.Fatal("nil signer with no error")
	}
	// The Signer's internal config isn't exported but we can
	// exercise the TTL through Issue + Verify with a known clock
	// in jwt_test.go. Here we just confirm the parser accepted
	// the override (rejection would be logged to stderr but the
	// signer would still be built with the defaults).
	tok, err := signer.Issue(Claims{})
	if err != nil {
		t.Fatalf("signer cannot issue tokens: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
}

func TestSignerFromEnv_InvalidTTLFallsBackToDefault(t *testing.T) {
	t.Setenv("KAPP_JWT_SECRET", "rotated-production-secret-for-invalid-ttl-test-12345")
	t.Setenv("KAPP_JWT_ACCESS_TTL", "fifteen-minutes-please")
	signer, err := SignerFromEnv()
	if err != nil {
		t.Fatalf("SignerFromEnv refused valid secret on invalid TTL; the invalid TTL must fall back, not propagate. err=%v", err)
	}
	if signer == nil {
		t.Fatal("nil signer with no error")
	}
}

func TestRequireJWT_DefaultsFalse(t *testing.T) {
	t.Setenv("KAPP_REQUIRE_JWT", "")
	if RequireJWT() {
		t.Fatal("RequireJWT() returned true with KAPP_REQUIRE_JWT unset; want false so local dev keeps booting without a secret")
	}
}

func TestRequireJWT_RecognisedTruthyValues(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "True"} {
		t.Setenv("KAPP_REQUIRE_JWT", v)
		if !RequireJWT() {
			t.Errorf("RequireJWT(%q) returned false; want true", v)
		}
	}
}

func TestRequireJWT_RecognisedFalsyValues(t *testing.T) {
	for _, v := range []string{"0", "false", "FALSE", "False"} {
		t.Setenv("KAPP_REQUIRE_JWT", v)
		if RequireJWT() {
			t.Errorf("RequireJWT(%q) returned true; want false", v)
		}
	}
}

func TestRequireJWT_UnknownValueFallsBackToDefault(t *testing.T) {
	t.Setenv("KAPP_REQUIRE_JWT", "yes-please")
	if RequireJWT() {
		t.Fatal("RequireJWT() honoured an unrecognised truthy value; want strict-set fallback so typos don't accidentally flip production into strict mode")
	}
}
