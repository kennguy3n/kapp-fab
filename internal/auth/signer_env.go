package auth

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// DevPlaceholderJWTSecret is the literal dev-only KAPP_JWT_SECRET
// shipped in .env.example so `make dev` / docker-compose boot
// without manual setup. It is intentionally a recognisable string
// (not a high-entropy random value) so it stands out in log review.
//
// Any deployment that runs with this exact value AND has not opted
// in via KAPP_ALLOW_DEV_JWT_SECRET=1 is almost certainly a
// misconfiguration — somebody copied .env.example to production
// without rotating the secret. SignerFromEnv refuses to boot in
// that state so the misconfiguration surfaces immediately instead
// of after the first forged admin JWT shows up in the audit log.
//
// Keep this constant in sync with the KAPP_JWT_SECRET line in
// .env.example. The string is consumed by services/api,
// services/importer, and services/agent-tools as the lookup key
// for the dev-mode gate.
const DevPlaceholderJWTSecret = "dev-only-kapp-jwt-secret-do-not-use-outside-localhost-fLPXuVqo9wKn"

// SignerFromEnv builds a *Signer from the standard KAPP_JWT_*
// environment variables every Kapp service shares:
//
//	KAPP_JWT_SECRET            (required; HMAC secret)
//	KAPP_ALLOW_DEV_JWT_SECRET  (required IFF secret == DevPlaceholderJWTSecret)
//	KAPP_JWT_ACCESS_TTL        (optional duration; default 15m)
//	KAPP_JWT_REFRESH_TTL       (optional duration; default 24h)
//	KAPP_JWT_ISSUER            (optional; default "kapp")
//	KAPP_JWT_AUDIENCE          (optional; default "kapp")
//
// Returns (nil, err) when KAPP_JWT_SECRET is unset OR is the
// recognised dev placeholder without the opt-in flag. Callers
// (services/api, services/importer, services/agent-tools) use the
// (nil, err) signal to either refuse to mount JWT-gated routes
// (api: returns 503 on those routes) or to fall back to the
// legacy header-based middleware with a loud WARN log
// (importer / agent-tools: legacy TenantMiddleware).
//
// Why one helper across services. Before Phase 5 this logic lived
// only in services/api/auth.go. Migrating importer + agent-tools
// to JWT-derived tenant scoping (closing the X-Tenant-ID
// impersonation gap) required the same parsing + same
// dev-placeholder guard in two more places, and copy-pasting the
// implementation would have created three drift surfaces for the
// secret-rotation check (the part most likely to regress). The
// helper centralises the contract; the test on services/api
// continues to pin the surface from the gateway's perspective.
func SignerFromEnv() (*Signer, error) {
	secret := os.Getenv("KAPP_JWT_SECRET")
	if secret == "" {
		return nil, errors.New("KAPP_JWT_SECRET unset")
	}
	if secret == DevPlaceholderJWTSecret && os.Getenv("KAPP_ALLOW_DEV_JWT_SECRET") != "1" {
		return nil, errors.New(
			"KAPP_JWT_SECRET is the literal dev-only placeholder from .env.example; " +
				"rotate it to a freshly generated value (e.g. `openssl rand -base64 48`) " +
				"or, for local development against the dev compose stack, explicitly " +
				"opt in by setting KAPP_ALLOW_DEV_JWT_SECRET=1 — the placeholder is the " +
				"same in every checkout of the repository, so anyone with a copy of " +
				".env.example can mint admin-looking tokens against this deployment",
		)
	}
	access := durationEnv("KAPP_JWT_ACCESS_TTL", 15*time.Minute)
	refresh := durationEnv("KAPP_JWT_REFRESH_TTL", 24*time.Hour)
	issuer := stringEnv("KAPP_JWT_ISSUER", "kapp")
	audience := stringEnv("KAPP_JWT_AUDIENCE", "kapp")
	return NewSigner(SignerConfig{
		Algorithm:  AlgHS256,
		HMACKey:    []byte(secret),
		Issuer:     issuer,
		Audience:   audience,
		AccessTTL:  access,
		RefreshTTL: refresh,
		Leeway:     30 * time.Second,
	})
}

// durationEnv parses the named env var as a time.Duration. Empty,
// missing, or unparseable values fall back to def AND emit a single
// stderr line so an operator who fat-fingered "15min" instead of
// "15m" sees the rejection at boot.
func durationEnv(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth: %s=%q not parseable as duration; using %s\n", key, raw, def)
		return def
	}
	return d
}

// stringEnv returns the named env var or the supplied default when
// the var is unset or empty.
func stringEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// boolEnv parses a string env var as a boolean with the same
// conservative semantics as platform.getenvBool — only the
// explicitly-listed canonical truthy / falsy values are recognised;
// anything else returns the fallback. Reused by sidecar services
// that need to opt into "JWT required" mode without falling through
// the platform package's getenv-set (which lives in a different
// package).
func boolEnv(key string, def bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "true", "TRUE", "True":
		return true
	case "0", "false", "FALSE", "False":
		return false
	default:
		return def
	}
}

// RequireJWT returns true when the operator has explicitly opted
// the sidecar service into the JWT-required mode via
// KAPP_REQUIRE_JWT=1. When unset, sidecars fall back to the
// legacy header-based middleware so local development continues
// to work. Production deployments SHOULD set this so a missing
// secret fails the boot loudly instead of silently exposing a
// cross-tenant impersonation path.
//
// Mirrors KAPP_REQUIRE_REDIS in spirit — "loud-fail vs silent-
// degrade" gate the operator can flip in deployments where the
// strict invariant is non-negotiable.
func RequireJWT() bool {
	return boolEnv("KAPP_REQUIRE_JWT", false)
}

