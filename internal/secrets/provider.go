// Package secrets is Kapp's secret-resolution layer. Every
// security-sensitive value an operator can configure -- the JWT
// signing key, the captcha provider secret, the SSO client
// secret, the helpdesk inbound shared token, the per-tenant
// portal HMAC, and so on -- flows through this package on its
// way from "operator-supplied string" to "in-memory byte slice
// used by a constructor".
//
// # Why an indirection layer
//
// Pre-PR-6 every service read its secrets directly via
// os.Getenv("KAPP_JWT_SECRET") and friends. That worked for a
// laptop / Docker Compose deployment but had three problems for
// any deployment past that:
//
//   - Production deployments on AWS / GCP / Kubernetes / Vault
//     don't materialise secrets as environment variables; they
//     mount them at a path or fetch them from a managed secret
//     store on demand. Hard-coding os.Getenv against fields named
//     KAPP_*_SECRET means the operator must either copy each
//     secret into the boot environment (defeating the point of
//     the managed store) or hand-write glue code in every
//     wrapper.
//
//   - Secret rotation is impossible without a restart when the
//     secret only exists in the environment. A managed secret
//     store gives the operator a "next version" they can stage
//     before the old one is revoked; we can't honour that
//     contract from os.Getenv.
//
//   - Audit logs that record the rotation epoch (e.g. "secret
//     version 3 was active when token X was minted") need a
//     consistent shape across providers. The interface here
//     surfaces a Version() so the rotation telemetry is uniform.
//
// # Resolver shape
//
// Provider exposes a single GetSecret(ctx, key) call that
// returns a SecretValue carrying the resolved bytes and a
// version string supplied by the backend (the AWS Secrets
// Manager version-id, the Vault KV-v2 metadata version, the file
// mtime when nothing more structured is available). Versions are
// opaque to the caller -- the only contract is that distinct
// versions of the same secret have distinct Version strings, so
// the JWT keyring refresh path can detect "the operator rotated
// the upstream secret; bring up a new signer pointing at the new
// material".
//
// # Provider selection
//
// The factory at internal/secrets/factory.go reads
// KAPP_SECRET_PROVIDER and dispatches to the matching backend.
// Default is "env", which preserves the pre-rotation behaviour
// exactly: every existing deployment keeps booting against
// os.Getenv without any code change. Operators opt into one of:
//
//   - env (default): os.Getenv lookup, no version (always "").
//   - file:          read from a path; version = mtime in unix-nanos.
//   - aws:           AWS Secrets Manager; version = AWS version-id.
//   - vault:         HashiCorp Vault KV v2; version = KV metadata version.
//   - gcp:           Google Secret Manager. Stubbed in PR-6 with a
//                    "not configured" error pending the heavier
//                    cloud.google.com/go/secretmanager dependency.
package secrets

import (
	"context"
	"errors"
)

// SecretValue is the resolved-secret return shape. Bytes is the
// secret material (typed as []byte so the caller can zero it
// after use); Version is the backend-supplied revision tag
// (opaque to callers, used by rotation detection).
type SecretValue struct {
	Bytes   []byte
	Version string
}

// Provider is the abstract source of secrets. Implementations
// resolve a logical key (e.g. "jwt/primary", "captcha/turnstile")
// to the current SecretValue. Implementations must be safe for
// concurrent use and must NOT cache values themselves -- the
// keyring refresh path expects a fresh resolution per call so a
// rotated upstream secret is picked up on the next refresh tick.
//
// Names are dotted / slashed paths chosen by the operator's
// secret-store layout; this package is intentionally
// path-agnostic so the same Provider works against e.g. a Vault
// mount at /secret/data/kapp/jwt-primary and an AWS prefix at
// arn:aws:secretsmanager:eu-west-1:1234:secret:kapp/jwt-primary.
type Provider interface {
	// Name returns a short identifier for boot logging and
	// audit events ("env", "file", "aws", "vault", "gcp"). It
	// is the same string the factory dispatches on.
	Name() string
	// GetSecret resolves the named secret to its current
	// value. Returns ErrSecretNotFound when the backend
	// confirms the key does not exist (distinct from a network
	// or auth error); the caller may use this to fall through
	// to a default. Returns a non-sentinel error for transient
	// failures (network, throttling, bad auth) so the keyring
	// refresh path can retry rather than fall over.
	GetSecret(ctx context.Context, key string) (SecretValue, error)
}

// Sentinel errors. Implementations should wrap
// ErrSecretNotFound with %w when the backend confirms the key
// does not exist, and ErrProviderUnavailable when the backend
// is transient-broken (so the caller can decide between
// fall-through and retry).
var (
	// ErrSecretNotFound indicates the backend returned a
	// definitive "no such key" response. Distinct from
	// ErrProviderUnavailable so the caller can decide between
	// fall-through (e.g. use the env default) and retry.
	ErrSecretNotFound = errors.New("secrets: not found")
	// ErrProviderUnavailable indicates the backend was reachable
	// but returned a transient error (5xx, throttling, network).
	// Callers should retry on a backoff rather than fall through
	// to a default.
	ErrProviderUnavailable = errors.New("secrets: provider unavailable")
	// ErrProviderNotConfigured indicates the operator selected
	// this provider via KAPP_SECRET_PROVIDER but did not supply
	// the configuration required to talk to the backend (e.g.
	// KAPP_SECRETS_VAULT_ADDR for the vault provider). The
	// factory returns this so the boot fails loudly instead of
	// silently falling through to env.
	ErrProviderNotConfigured = errors.New("secrets: provider not configured")
)
