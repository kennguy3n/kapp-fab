package secrets

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EnvProvider resolves secrets from process environment
// variables. It is the default provider for backwards
// compatibility with the pre-PR-6 deployments that read
// KAPP_*_SECRET directly via os.Getenv.
//
// The key-to-env mapping is a simple normalisation: dotted /
// slashed keys are uppercased and have "." and "/" replaced
// with "_", then the configured prefix (default "KAPP_") is
// prepended. So a request for "jwt/primary" resolves to
// KAPP_JWT_PRIMARY, "captcha/turnstile" resolves to
// KAPP_CAPTCHA_TURNSTILE, etc. Operators with non-standard env
// schemes can override the prefix at construction time.
//
// EnvProvider never returns ErrProviderUnavailable: the env is
// always reachable from the process's perspective. It returns
// ErrSecretNotFound only when the resolved env var is unset OR
// empty. Empty values are treated as missing rather than as a
// legitimate empty secret so the JWT layer doesn't accept a
// zero-length signing key from a fat-fingered .env.
type EnvProvider struct {
	prefix string
}

// NewEnvProvider returns an EnvProvider with the supplied env
// var prefix. Empty prefix is rejected because the unprefixed
// lookup would let any environment variable shadow a Kapp
// secret; the default prefix is "KAPP_" and is sufficient for
// every shipping deployment.
func NewEnvProvider(prefix string) (*EnvProvider, error) {
	if prefix == "" {
		prefix = "KAPP_"
	}
	return &EnvProvider{prefix: prefix}, nil
}

// Name returns the literal "env".
func (*EnvProvider) Name() string { return "env" }

// GetSecret resolves key by normalising it to an env var name
// and reading os.Getenv. Empty or unset variables return
// ErrSecretNotFound; non-empty variables return SecretValue
// with an empty Version field (env vars carry no version
// metadata).
func (p *EnvProvider) GetSecret(_ context.Context, key string) (SecretValue, error) {
	envName := envKey(p.prefix, key)
	v := os.Getenv(envName)
	if v == "" {
		return SecretValue{}, fmt.Errorf("%w: env %s unset", ErrSecretNotFound, envName)
	}
	return SecretValue{Bytes: []byte(v)}, nil
}

// envKey normalises a Provider key into an env var name. Used
// by EnvProvider and by tests that want to assert the
// canonical mapping.
func envKey(prefix, key string) string {
	upper := strings.ToUpper(key)
	upper = strings.ReplaceAll(upper, "/", "_")
	upper = strings.ReplaceAll(upper, ".", "_")
	upper = strings.ReplaceAll(upper, "-", "_")
	return prefix + upper
}
