package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVaultProvider_GetSecret_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/data/jwt/primary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Vault-Token") != "test-token" {
			t.Errorf("missing/wrong vault token: %s", r.Header.Get("X-Vault-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"value":"supersecret"},"metadata":{"version":3}}}`))
	}))
	defer srv.Close()

	p, err := NewVaultProvider(VaultProviderConfig{
		Addr:  srv.URL,
		Token: "test-token",
	})
	if err != nil {
		t.Fatalf("NewVaultProvider: %v", err)
	}
	v, err := p.GetSecret(context.Background(), "jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "supersecret" {
		t.Fatalf("got %q want supersecret", string(v.Bytes))
	}
	if v.Version != "3" {
		t.Fatalf("got version %q want 3", v.Version)
	}
}

func TestVaultProvider_GetSecret_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p, _ := NewVaultProvider(VaultProviderConfig{Addr: srv.URL, Token: "t"})
	_, err := p.GetSecret(context.Background(), "missing")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestVaultProvider_GetSecret_5xx_Transient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p, _ := NewVaultProvider(VaultProviderConfig{Addr: srv.URL, Token: "t"})
	_, err := p.GetSecret(context.Background(), "key")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got %v", err)
	}
}

// TestVaultProvider_GetSecret_PermissionDenied pins the
// 401/403 → ErrProviderUnavailable contract introduced to
// align Vault's error classification with the GCP provider
// (gcp.go:translateGCPError maps codes.PermissionDenied /
// Unauthenticated the same way). Without this alignment,
// future callers using errors.Is to distinguish "credential
// is bad" from "everything is fine" would silently fail to
// match on the Vault backend. The bot's round-7 finding
// raised this as a cross-provider inconsistency — pinning it
// in a test prevents the wrap from being silently removed in
// a future refactor.
func TestVaultProvider_GetSecret_PermissionDenied(t *testing.T) {
	for _, sc := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(sc), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(sc)
				_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
			}))
			defer srv.Close()
			p, _ := NewVaultProvider(VaultProviderConfig{Addr: srv.URL, Token: "t"})
			_, err := p.GetSecret(context.Background(), "jwt/primary")
			if !errors.Is(err, ErrProviderUnavailable) {
				t.Fatalf("expected ErrProviderUnavailable for %d, got %v", sc, err)
			}
			if !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("expected error to surface IAM hint, got %v", err)
			}
		})
	}
}

func TestVaultProvider_GetSecret_MissingValueKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"data":{"other":"x"},"metadata":{"version":1}}}`))
	}))
	defer srv.Close()
	p, _ := NewVaultProvider(VaultProviderConfig{Addr: srv.URL, Token: "t"})
	_, err := p.GetSecret(context.Background(), "key")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound (missing 'value' key), got %v", err)
	}
}

func TestVaultProvider_GetSecret_CustomMountAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/kv/data/jwt/primary" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"pemKey":"---PEM---"},"metadata":{"version":7}}}`))
	}))
	defer srv.Close()
	p, _ := NewVaultProvider(VaultProviderConfig{
		Addr:      srv.URL,
		Token:     "t",
		MountPath: "kv",
		SecretKey: "pemKey",
	})
	v, err := p.GetSecret(context.Background(), "jwt/primary")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(v.Bytes) != "---PEM---" {
		t.Fatalf("got %q want ---PEM---", string(v.Bytes))
	}
}

func TestVaultProvider_RejectsEmptyAddr(t *testing.T) {
	_, err := NewVaultProvider(VaultProviderConfig{Token: "t"})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured, got %v", err)
	}
}

func TestVaultProvider_RejectsEmptyToken(t *testing.T) {
	_, err := NewVaultProvider(VaultProviderConfig{Addr: "https://x"})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured, got %v", err)
	}
}

// TestVaultProvider_EncodesPathSpecialChars verifies that
// percent-special characters in the secret key are URL-encoded
// before the GET so the request URL parses cleanly. Without the
// per-segment escape, a key containing '%', '#', '?', or space
// would be interpreted as URL syntax — the Vault server would
// see a different path than the operator configured, and either
// 404 or accidentally route to a different secret namespace.
func TestVaultProvider_EncodesPathSpecialChars(t *testing.T) {
	t.Parallel()
	var observedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"data":{"data":{"value":"x"},"metadata":{"version":1}}}`))
	}))
	defer srv.Close()
	p, _ := NewVaultProvider(VaultProviderConfig{Addr: srv.URL, Token: "t"})
	if _, err := p.GetSecret(context.Background(), "secret/with space and?ampersand"); err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	// Expect each segment escaped individually; the "/"
	// between segments is preserved.
	want := "/v1/secret/data/secret/with%20space%20and%3Fampersand"
	if observedPath != want {
		t.Fatalf("vault saw path %q, want %q", observedPath, want)
	}
}

// TestVaultProvider_PreservesPathStructure verifies that the
// nested-namespace path separator ("/") is preserved as a
// structural delimiter; only special chars within each segment
// are escaped. Vault KV v2 conventionally uses "/" to namespace
// secrets ("jwt/primary", "platform/db/password") and that
// hierarchy must survive encoding.
func TestVaultProvider_PreservesPathStructure(t *testing.T) {
	t.Parallel()
	var observedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"data":{"data":{"value":"x"},"metadata":{"version":1}}}`))
	}))
	defer srv.Close()
	p, _ := NewVaultProvider(VaultProviderConfig{Addr: srv.URL, Token: "t"})
	if _, err := p.GetSecret(context.Background(), "platform/jwt/primary"); err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	want := "/v1/secret/data/platform/jwt/primary"
	if observedPath != want {
		t.Fatalf("vault saw path %q, want %q (separators must survive encoding)", observedPath, want)
	}
}
