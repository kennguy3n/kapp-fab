package secrets

import (
	"context"
	"fmt"
)

// GCPProvider is a placeholder for Google Secret Manager support.
// PR-6 ships the interface and the AWS / Vault / file / env
// backends; the GCP backend is intentionally deferred because
// pulling in cloud.google.com/go/secretmanager adds ~30 indirect
// dependencies and the dispatch contract (key -> projects/<p>/
// secrets/<n>/versions/<v>) is non-trivial enough to warrant a
// dedicated implementation pass.
//
// Operators who select KAPP_SECRET_PROVIDER=gcp get a clear
// "not configured" error at boot so the choice is recorded but
// not silently ignored. The TODO marker lives here, not in
// factory.go, because the factory file is the smoke-test surface
// for "every advertised provider has a constructor".
type GCPProvider struct{}

// NewGCPProvider returns an error: GCP support is not yet wired
// in this build. Operators who need it should track the
// follow-up PR (KAPP-7xx) or use the Vault backend in the
// meantime against a Vault-on-GKE instance.
func NewGCPProvider(_ context.Context) (*GCPProvider, error) {
	return nil, fmt.Errorf("%w: gcp provider not wired in this build; track follow-up PR or use vault backend",
		ErrProviderNotConfigured)
}

// Name returns the literal "gcp".
func (*GCPProvider) Name() string { return "gcp" }

// GetSecret always returns ErrProviderNotConfigured. Defensive:
// nobody should be able to hold a *GCPProvider because the
// constructor refuses, but if a future caller bypasses the
// constructor (e.g. via &GCPProvider{}), they get the same
// error rather than a silent nil-deref.
func (*GCPProvider) GetSecret(_ context.Context, _ string) (SecretValue, error) {
	return SecretValue{}, fmt.Errorf("%w: gcp provider not wired", ErrProviderNotConfigured)
}
