package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestEnvProvider_GetSecret_Found(t *testing.T) {
	t.Setenv("KAPP_TEST_JWT_PRIMARY", "abc123")
	p, err := NewEnvProvider("")
	if err != nil {
		t.Fatalf("NewEnvProvider: %v", err)
	}
	v, err := p.GetSecret(context.Background(), "test/jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "abc123" {
		t.Fatalf("got %q want abc123", string(v.Bytes))
	}
	if v.Version != "" {
		t.Fatalf("env provider should leave Version empty; got %q", v.Version)
	}
}

func TestEnvProvider_GetSecret_Missing(t *testing.T) {
	p, _ := NewEnvProvider("")
	_, err := p.GetSecret(context.Background(), "test/missing/key")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestEnvProvider_GetSecret_Empty(t *testing.T) {
	t.Setenv("KAPP_TEST_EMPTY", "")
	p, _ := NewEnvProvider("")
	_, err := p.GetSecret(context.Background(), "test/empty")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound for empty env, got %v", err)
	}
}

func TestEnvProvider_CustomPrefix(t *testing.T) {
	t.Setenv("MYAPP_JWT_PRIMARY", "xyz")
	p, _ := NewEnvProvider("MYAPP_")
	v, err := p.GetSecret(context.Background(), "jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "xyz" {
		t.Fatalf("got %q want xyz", string(v.Bytes))
	}
}

func TestEnvKey_Normalisation(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"jwt/primary", "KAPP_JWT_PRIMARY"},
		{"jwt.primary", "KAPP_JWT_PRIMARY"},
		{"jwt-primary", "KAPP_JWT_PRIMARY"},
		{"captcha/turnstile", "KAPP_CAPTCHA_TURNSTILE"},
		{"already_upper", "KAPP_ALREADY_UPPER"},
	}
	for _, tc := range cases {
		got := envKey("KAPP_", tc.key)
		if got != tc.want {
			t.Errorf("envKey(%q) = %q want %q", tc.key, got, tc.want)
		}
	}
}
