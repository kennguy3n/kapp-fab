package secrets

import (
	"context"
	"fmt"
	"strings"
)

// Config is the operator-supplied selector for the active
// provider. The Backend field switches on the provider name;
// the embedded backend-specific configs are passed through to
// the matching constructor. Operators set this from environment
// variables (see platform.Config.LoadSecretConfig).
//
// One-of semantics: the factory consults exactly the field
// matching Backend. The others are ignored, so e.g. setting
// File.RootDir while Backend="env" does not affect anything.
type Config struct {
	// Backend is the provider name: "env", "file", "aws",
	// "vault", "gcp". Empty defaults to "env" for backwards
	// compatibility with pre-PR-6 deployments.
	Backend string
	// EnvPrefix overrides the env-var prefix when Backend ==
	// "env". Default "KAPP_".
	EnvPrefix string
	// File holds the file-provider config.
	File FileProviderConfig
	// AWS holds the AWS Secrets Manager config.
	AWS AWSProviderConfig
	// Vault holds the HashiCorp Vault config.
	Vault VaultProviderConfig
}

// FileProviderConfig is the file-backend selector. It is its
// own named struct (rather than an anonymous one inside Config)
// so callers can build one in isolation, e.g. tests that exercise
// just the file backend without filling in unrelated AWS/Vault
// fields.
type FileProviderConfig struct {
	RootDir string
}

// NewFromConfig dispatches on cfg.Backend and returns the
// matching Provider. Returns ErrProviderNotConfigured for
// unknown backend names so operator typos surface at boot
// rather than silently falling through to env.
//
// Pass a request-scoped context that the constructor can use
// for any boot-time HTTP / IMDS calls the backend needs (the
// AWS provider for example dispatches one STS call to load the
// default credential chain).
func NewFromConfig(ctx context.Context, cfg Config) (Provider, error) {
	backend := strings.ToLower(strings.TrimSpace(cfg.Backend))
	if backend == "" {
		backend = "env"
	}
	switch backend {
	case "env":
		return NewEnvProvider(cfg.EnvPrefix)
	case "file":
		return NewFileProvider(cfg.File.RootDir)
	case "aws":
		return NewAWSProvider(ctx, cfg.AWS)
	case "vault":
		return NewVaultProvider(cfg.Vault)
	case "gcp":
		return NewGCPProvider(ctx)
	default:
		return nil, fmt.Errorf("%w: unknown backend %q", ErrProviderNotConfigured, cfg.Backend)
	}
}
