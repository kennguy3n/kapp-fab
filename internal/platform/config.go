// Package platform wires infrastructure primitives (database, config) used
// across Kapp services. It exposes a small set of helpers that services can
// compose without introducing framework-level coupling.
package platform

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds runtime configuration values shared by the API and worker
// services. Fields are populated from environment variables by LoadConfig.
type Config struct {
	// DatabaseURL is a libpq-style connection string for PostgreSQL. In
	// production this points at the non-superuser `kapp_app` role so that
	// RLS is enforced on the data plane (see migrations).
	DatabaseURL string
	// AdminDatabaseURL optionally points at a BYPASSRLS role (kapp_admin)
	// used only for the narrow set of control-plane reads that legitimately
	// span tenants — notably the user→tenants lookup used by login. Empty
	// is allowed; those reads then fall back to the main pool and return
	// no rows under the default RLS policy.
	AdminDatabaseURL string
	// ListenAddr is the host:port the HTTP server binds to (API only).
	ListenAddr string
	// S3Endpoint is the object-store endpoint (S3 or MinIO compatible).
	S3Endpoint string
	// S3Bucket is the bucket used for Kapp file attachments.
	S3Bucket string
	// S3AccessKey is the object-store access key ID.
	S3AccessKey string
	// S3SecretKey is the object-store secret access key.
	S3SecretKey string
	// EventBusURL is the NATS/Kafka/etc. URL for the event bus.
	EventBusURL string
	// SMTPHost/Port/User/Password/From configure the outbound mail
	// adapter used by the worker for `notification.channel=email`.
	// All five are optional; when SMTPHost is empty the worker falls
	// back to logging the notice instead of dialing an MTA.
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string

	// LRU cache bounds for the three hot-path lookup paths shared by
	// the API binary. Defaults match the constants that used to live
	// inline in services/api/main.go (1024 / 512 / 256) and are
	// overridable from operator-facing env vars so a deployment with
	// a large tenant fleet or a particularly authz-heavy workload can
	// trade memory for hit rate without a rebuild.
	//
	//   KAPP_KTYPE_CACHE_SIZE  - records-handler ktype-by-name lookup.
	//                           300+ KTypes is the practical upper bound;
	//                           default 1024 keeps the whole catalogue resident.
	//   KAPP_AUTHZ_CACHE_SIZE  - per-(user,resource) decision cache.
	//                           Default 512 with a 30s TTL bounds staleness
	//                           after a role grant/revoke; raise it when
	//                           the active-user count is high.
	//   KAPP_TENANT_CACHE_SIZE - tenant row lookup keyed by both id and
	//                           slug. Tenant rows are small (<1 KB) so
	//                           the default 256 fits the typical multi-
	//                           tenant fleet without trading much memory.
	//
	// All three are clamped to a positive lower bound by NewLRUCache
	// (it defaults to 1024 if a non-positive value is passed), so an
	// operator who accidentally sets KAPP_TENANT_CACHE_SIZE=0 ends up
	// with the LRUCache default rather than an unbounded map.
	KTypeCacheSize  int
	AuthzCacheSize  int
	TenantCacheSize int

	// RedisURL is the connection string for the shared Redis instance
	// that backs the distributed tenant + IP rate limiters (Phase 1)
	// and any future cross-replica coordination primitives. Empty is
	// permitted in local-dev (`make dev`, single-process docker-compose)
	// — both limiters then fall back to in-process maps which enforce
	// limits per-pod rather than globally.
	//
	// Phase 3 introduces RequireRedis as a hard gate so production
	// deployments cannot accidentally fall back to the in-process
	// limiter. When RequireRedis is true and RedisURL is empty,
	// LoadConfig returns an error rather than booting the API with
	// non-distributed rate-limiting; this matches the existing pattern
	// for DB_URL and avoids the "silent dev-mode in production"
	// failure class.
	RedisURL string

	// RequireRedis (sourced from KAPP_REQUIRE_REDIS) opts a deployment
	// into the strict Redis-required mode. Production deployments
	// SHOULD set KAPP_REQUIRE_REDIS=1 (or =true) so a misconfigured
	// REDIS_URL fails the boot loudly instead of silently degrading
	// to per-pod rate limiting. Default false so local dev continues
	// to boot without Redis.
	RequireRedis bool

	// Env is the operator-supplied deployment marker emitted into
	// every structured log line ("dev", "staging", "production").
	// Sourced from KAPP_ENV. Empty values default to "dev" so a
	// local boot still produces useful log attribution.
	Env string

	// LogFormat selects the slog handler: "json" (machine-parseable,
	// expected in production) or "text" (human-readable, default).
	// Sourced from KAPP_LOG_FORMAT.
	LogFormat string

	// LogLevel is the minimum severity emitted. Accepted values:
	// "debug", "info", "warn", "error". Sourced from KAPP_LOG_LEVEL.
	// Unknown values default to info.
	LogLevel string

	// MetricsAddr is the host:port the Prometheus /metrics endpoint
	// listens on. When empty the endpoint is mounted on the main
	// API router instead (legacy behaviour); when set, a dedicated
	// http.Server listens on the supplied address so scrapers can
	// hit /metrics without contending with user-facing latency.
	// Sourced from KAPP_METRICS_ADDR. Production deployments SHOULD
	// set this to a separate port (e.g. ":9090") behind an internal-
	// only network policy.
	MetricsAddr string

	// SSEAddr is the host:port the Server-Sent-Events listener
	// binds to. When empty, /api/v1/events/stream stays mounted on
	// the main API router (legacy behaviour) and the main
	// http.Server keeps WriteTimeout=0 so the stream is not killed
	// mid-flight. When set to a dedicated address (e.g. ":8081"),
	// the SSE route is split off onto its own http.Server with
	// LongStreamTimeouts (Write=0); the main API listener then
	// adopts DefaultHTTPTimeouts (Write=120s) so every non-streaming
	// route gets the strict slow-write defense too. Sourced from
	// KAPP_SSE_ADDR. Production deployments SHOULD set this so the
	// main API does not carry the SSE-shaped WriteTimeout=0 surface
	// across every other route.
	SSEAddr string

	// GRPCAddr is the host:port the gRPC server (Pillar A5) binds
	// to. When empty, the api binary serves HTTP only — the gRPC
	// surface stays dark. When set (e.g. ":9090") the api binary
	// stands up an additional grpc.Server with the same dependency
	// wiring as the HTTP gateway (signer, tenant resolver, session
	// store, ktype registry) so a gRPC client and a REST client
	// hitting the same deployment see byte-for-byte identical
	// behaviour. Production deployments SHOULD set this to an
	// internal-only port so the SDK can dial the typed surface
	// without re-translating through REST. Sourced from
	// KAPP_GRPC_ADDR.
	GRPCAddr string

	// GRPCReflection controls whether the gRPC server advertises
	// the grpc.reflection.v1alpha service so off-the-shelf tooling
	// (grpcurl, BloomRPC, Postman) can list and invoke RPCs without
	// a copy of the .proto files. Defaults off — production should
	// keep reflection disabled so the on-the-wire surface is not
	// self-describing. Sourced from KAPP_GRPC_REFLECTION.
	GRPCReflection bool

	// GatewayMount, when non-empty, mounts the grpc-gateway HTTP
	// reverse-proxy on the main API router at the supplied path
	// prefix (e.g. "/api/v2"). The gateway translates JSON/HTTP
	// requests under that prefix to gRPC calls against the local
	// in-process gRPC server, so an SDK using REST still goes
	// through the same handler chain as a typed gRPC client.
	// Empty disables the mount and the main router serves only
	// the legacy /api/v1 handlers. Sourced from
	// KAPP_GRPC_GATEWAY_MOUNT.
	GatewayMount string

	// CaptchaProvider selects the bot-resistance backend wired in
	// front of unauthenticated public POST endpoints (form submit,
	// portal magic-link request, SSO bootstrap). One of:
	//
	//   - ""           same as "disabled" — captcha is a no-op
	//                  pass-through; used for local dev.
	//   - "disabled"   explicit no-op; logged at boot.
	//   - "turnstile"  Cloudflare Turnstile siteverify.
	//   - "hcaptcha"   hCaptcha siteverify.
	//   - "recaptcha_v3" Google reCAPTCHA v3 siteverify.
	//   - "pow"        Internal Hashcash-style proof-of-work.
	//
	// Production deployments MUST set this to a non-disabled value.
	// Sourced from KAPP_CAPTCHA_PROVIDER.
	CaptchaProvider string

	// CaptchaSecret is the server-side secret used by the
	// siteverify-style providers (Turnstile, hCaptcha, reCAPTCHA
	// v3). The client-side site key is rendered in the frontend
	// independently and is NOT read by the API binary. Sourced
	// from KAPP_CAPTCHA_SECRET.
	CaptchaSecret string

	// CaptchaMinScore is the lower bound on reCAPTCHA v3's score
	// (0.0 bot ... 1.0 human) below which the verifier denies.
	// 0 falls back to 0.5 (Google's recommended default). Ignored
	// by Turnstile and hCaptcha which return a binary outcome.
	// Sourced from KAPP_CAPTCHA_MIN_SCORE.
	CaptchaMinScore float64

	// CaptchaExpectedHostname optionally pins the hostname the
	// provider reports as the token's origin. Empty disables
	// the check; production deployments SHOULD set it to the
	// site-facing hostname (e.g. "kapp.example.com") so a token
	// minted on a different site key is rejected. Sourced from
	// KAPP_CAPTCHA_EXPECTED_HOSTNAME.
	CaptchaExpectedHostname string

	// PoWHMACKey is the symmetric key used to sign Hashcash-style
	// PoW challenge envelopes. Required when CaptchaProvider="pow"
	// and ignored otherwise. MUST be at least 32 bytes (256 bits)
	// — the factory rejects shorter keys at boot. Sourced from
	// KAPP_POW_HMAC_KEY (raw bytes; not base64).
	PoWHMACKey string

	// PoWDifficulty is the number of leading zero bits required
	// in a PoW solution hash. 0 → default 16 (~100ms of JS work
	// per solve). Tune upward for high-value endpoints, downward
	// for low-value endpoints where user friction matters more.
	// Sourced from KAPP_POW_DIFFICULTY.
	PoWDifficulty int

	// CSRFAllowedOrigins is the allowlist of origins (scheme://
	// host[:port]) the CSRF middleware accepts for mutating
	// requests. Comma-separated in the env var. Production
	// deployments MUST set at least one entry; empty disables
	// the Origin-allowlist defence (still safe for bearer-token
	// auth but unsafe for any future cookie-auth surface). Sourced
	// from KAPP_CSRF_ALLOWED_ORIGINS.
	CSRFAllowedOrigins []string

	// CSRFCookieName names the double-submit cookie. Empty
	// disables the double-submit check entirely (Origin allowlist
	// still applies). Production deployments using cookie auth
	// SHOULD set "__Host-kapp-csrf" so the cookie is bound to the
	// exact origin. Sourced from KAPP_CSRF_COOKIE_NAME.
	CSRFCookieName string

	// CSRFCookieSecure controls the Secure flag on the issued
	// CSRF cookie. SHOULD be true in production (HTTPS); false
	// only for local-dev HTTP. Sourced from KAPP_CSRF_COOKIE_SECURE.
	CSRFCookieSecure bool
}

// LoadConfig reads configuration from environment variables and returns a
// validated Config. It returns an error if a required value is missing.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		DatabaseURL:      os.Getenv("DB_URL"),
		AdminDatabaseURL: os.Getenv("ADMIN_DB_URL"),
		ListenAddr:       getenv("LISTEN_ADDR", ":8080"),
		S3Endpoint:       os.Getenv("S3_ENDPOINT"),
		S3Bucket:         os.Getenv("S3_BUCKET"),
		S3AccessKey:      os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:      os.Getenv("S3_SECRET_KEY"),
		EventBusURL:      os.Getenv("NATS_URL"),
		SMTPHost:         os.Getenv("SMTP_HOST"),
		SMTPPort:         os.Getenv("SMTP_PORT"),
		SMTPUser:         os.Getenv("SMTP_USER"),
		SMTPPassword:     os.Getenv("SMTP_PASS"),
		SMTPFrom:         os.Getenv("SMTP_FROM"),
		KTypeCacheSize:   getenvInt("KAPP_KTYPE_CACHE_SIZE", 1024),
		AuthzCacheSize:   getenvInt("KAPP_AUTHZ_CACHE_SIZE", 512),
		TenantCacheSize:  getenvInt("KAPP_TENANT_CACHE_SIZE", 256),
		RedisURL:         os.Getenv("REDIS_URL"),
		RequireRedis:     getenvBool("KAPP_REQUIRE_REDIS", false),
		Env:              getenv("KAPP_ENV", "dev"),
		LogFormat:        os.Getenv("KAPP_LOG_FORMAT"),
		LogLevel:         os.Getenv("KAPP_LOG_LEVEL"),
		MetricsAddr:      os.Getenv("KAPP_METRICS_ADDR"),
		SSEAddr:          os.Getenv("KAPP_SSE_ADDR"),
		GRPCAddr:         os.Getenv("KAPP_GRPC_ADDR"),
		GRPCReflection:   getenvBool("KAPP_GRPC_REFLECTION", false),
		GatewayMount:     os.Getenv("KAPP_GRPC_GATEWAY_MOUNT"),

		CaptchaProvider:         os.Getenv("KAPP_CAPTCHA_PROVIDER"),
		CaptchaSecret:           os.Getenv("KAPP_CAPTCHA_SECRET"),
		CaptchaMinScore:         getenvFloat("KAPP_CAPTCHA_MIN_SCORE", 0),
		CaptchaExpectedHostname: os.Getenv("KAPP_CAPTCHA_EXPECTED_HOSTNAME"),
		PoWHMACKey:              os.Getenv("KAPP_POW_HMAC_KEY"),
		PoWDifficulty:           getenvInt("KAPP_POW_DIFFICULTY", 0),

		CSRFAllowedOrigins: splitCSV(os.Getenv("KAPP_CSRF_ALLOWED_ORIGINS")),
		CSRFCookieName:     os.Getenv("KAPP_CSRF_COOKIE_NAME"),
		CSRFCookieSecure:   getenvBool("KAPP_CSRF_COOKIE_SECURE", false),
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks the loaded configuration for structural correctness.
// Called from LoadConfig; exported so tests and the upcoming config-
// validation CLI tool can re-run validation on an already-populated
// Config. Each rule's purpose:
//
//  1. DB_URL is the only universally-required env var (every service
//     opens a pgx pool). Missing → hard boot failure with a clear
//     message.
//  2. RequireRedis + empty REDIS_URL is the "loud-fail vs silent-
//     degrade" gate introduced in Phase 3.
//  3. Cache sizes must be positive when explicitly set (zero / negative
//     would create either a no-op cache or a cache that LRU evicts on
//     every write — both are silent perf footguns). getenvInt already
//     falls back to the default on non-positive values, so this check
//     is primarily a doc-comment for operators reading the source.
//  4. LogFormat / LogLevel typos already fall back to safe defaults in
//     parseLevel / NewLogger; we surface the warning via a structured
//     return value so an operator can see what was actually accepted.
func (c *Config) Validate() error {
	if c.DatabaseURL == "" {
		return errors.New("DB_URL is required")
	}
	if c.RequireRedis && c.RedisURL == "" {
		return errors.New("KAPP_REQUIRE_REDIS=1 but REDIS_URL is empty; set REDIS_URL or unset KAPP_REQUIRE_REDIS to permit in-process fallback")
	}
	if c.KTypeCacheSize <= 0 || c.AuthzCacheSize <= 0 || c.TenantCacheSize <= 0 {
		// LoadConfig() routes every KAPP_*_CACHE_SIZE through
		// getenvInt which falls back to a positive default on
		// missing or non-positive input, so this branch can only
		// fire when Validate() is invoked on a Config struct
		// constructed by hand (e.g. tests, future config-from-
		// YAML loaders). Defense-in-depth against a caller that
		// bypasses LoadConfig.
		return fmt.Errorf("cache sizes must be positive; got KTypeCacheSize=%d AuthzCacheSize=%d TenantCacheSize=%d (likely set via hand-constructed Config rather than LoadConfig)", c.KTypeCacheSize, c.AuthzCacheSize, c.TenantCacheSize)
	}
	if c.LogFormat != "" {
		switch strings.ToLower(c.LogFormat) {
		case "json", "text":
		default:
			return fmt.Errorf("KAPP_LOG_FORMAT=%q is not a recognised value; expected one of: json, text", c.LogFormat)
		}
	}
	if c.LogLevel != "" {
		switch strings.ToLower(c.LogLevel) {
		case "debug", "info", "warn", "warning", "error", "err":
		default:
			return fmt.Errorf("KAPP_LOG_LEVEL=%q is not a recognised value; expected one of: debug, info, warn, error", c.LogLevel)
		}
	}
	return nil
}

// getenvBool parses a string env var as a boolean. Accepts the strings
// "1", "true", "TRUE", "True" (true) and "0", "false", "FALSE", "False"
// (false); anything else returns the fallback so a typo doesn't silently
// flip the value. Centralising the parser ensures every boolean env var
// in this package uses the same predictable semantics (notably, raw
// presence does NOT imply truthiness — value is always inspected).
func getenvBool(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "TRUE", "True":
		return true
	case "0", "false", "FALSE", "False":
		return false
	default:
		return fallback
	}
}

// getenvInt returns the integer value of the named environment variable,
// or fallback if the variable is unset, empty, or not a valid integer.
// Non-positive values fall back to the caller's default so an operator
// who clears a cache-size env var to opt out of a tunable gets the
// builtin default rather than a silently-disabled cache (which would
// degrade performance without surfacing as an obvious misconfiguration).
func getenvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// getenvFloat returns the float64 value of the named environment
// variable, or fallback if the variable is unset, empty, or not a
// valid float. Used by KAPP_CAPTCHA_MIN_SCORE which expects a
// decimal in [0.0, 1.0]; out-of-range values are accepted at this
// layer and rejected by the captcha verifier's own bounds check.
func getenvFloat(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return f
}

// splitCSV parses a comma-separated env var into a non-nil slice
// of trimmed non-empty strings. Empty input returns nil so callers
// can length-check the result without separate "is it set" logic.
// Used by KAPP_CSRF_ALLOWED_ORIGINS where each entry is an
// origin (scheme://host[:port]).
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
