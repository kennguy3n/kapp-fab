package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestNewFromConfig_DefaultsToEnv(t *testing.T) {
	t.Setenv("KAPP_FACTORY_DEFAULT", "v")
	p, err := NewFromConfig(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if p.Name() != "env" {
		t.Fatalf("expected env provider, got %s", p.Name())
	}
	sec, err := p.GetSecret(context.Background(), "factory/default")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if string(sec.Bytes) != "v" {
		t.Fatalf("got %q want v", string(sec.Bytes))
	}
}

func TestNewFromConfig_File(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Backend: "file"}
	cfg.File.RootDir = dir
	p, err := NewFromConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if p.Name() != "file" {
		t.Fatalf("expected file provider, got %s", p.Name())
	}
}

func TestNewFromConfig_Vault_RequiresAddr(t *testing.T) {
	_, err := NewFromConfig(context.Background(), Config{Backend: "vault"})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured, got %v", err)
	}
}

func TestNewFromConfig_UnknownBackend(t *testing.T) {
	_, err := NewFromConfig(context.Background(), Config{Backend: "nonexistent"})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured for unknown backend, got %v", err)
	}
}

func TestNewFromConfig_GCP_StubReturnsNotConfigured(t *testing.T) {
	_, err := NewFromConfig(context.Background(), Config{Backend: "gcp"})
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured for gcp stub, got %v", err)
	}
}

func TestNewFromConfig_CaseAndTrimNormalisation(t *testing.T) {
	t.Setenv("KAPP_X", "1")
	p, err := NewFromConfig(context.Background(), Config{Backend: "  ENV  "})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	if p.Name() != "env" {
		t.Fatalf("expected env after case+trim, got %s", p.Name())
	}
}
