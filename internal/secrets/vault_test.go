package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
