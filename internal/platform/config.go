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
		return errors.New("cache sizes (KAPP_KTYPE_CACHE_SIZE / KAPP_AUTHZ_CACHE_SIZE / KAPP_TENANT_CACHE_SIZE) must be positive; getenvInt fallback failed")
	}
	if c.LogFormat != "" && c.LogFormat != "json" && c.LogFormat != "text" {
		return fmt.Errorf("KAPP_LOG_FORMAT=%q is not a recognised value; expected one of: json, text", c.LogFormat)
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

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
