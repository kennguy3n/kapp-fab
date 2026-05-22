package platform

import (
	"testing"
)

func TestLoadConfig_CacheSizeDefaults(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	// Clear cache-size env vars to verify defaults. t.Setenv to ""
	// is functionally equivalent to Unsetenv for the getenvInt
	// fallback path (empty string == use default) AND registers
	// cleanup with the test framework so any caller env value is
	// restored after the test.
	t.Setenv("KAPP_KTYPE_CACHE_SIZE", "")
	t.Setenv("KAPP_AUTHZ_CACHE_SIZE", "")
	t.Setenv("KAPP_TENANT_CACHE_SIZE", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KTypeCacheSize != 1024 {
		t.Errorf("KTypeCacheSize = %d, want 1024", cfg.KTypeCacheSize)
	}
	if cfg.AuthzCacheSize != 512 {
		t.Errorf("AuthzCacheSize = %d, want 512", cfg.AuthzCacheSize)
	}
	if cfg.TenantCacheSize != 256 {
		t.Errorf("TenantCacheSize = %d, want 256", cfg.TenantCacheSize)
	}
}

func TestLoadConfig_CacheSizeFromEnv(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_KTYPE_CACHE_SIZE", "2048")
	t.Setenv("KAPP_AUTHZ_CACHE_SIZE", "1024")
	t.Setenv("KAPP_TENANT_CACHE_SIZE", "512")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KTypeCacheSize != 2048 {
		t.Errorf("KTypeCacheSize = %d, want 2048", cfg.KTypeCacheSize)
	}
	if cfg.AuthzCacheSize != 1024 {
		t.Errorf("AuthzCacheSize = %d, want 1024", cfg.AuthzCacheSize)
	}
	if cfg.TenantCacheSize != 512 {
		t.Errorf("TenantCacheSize = %d, want 512", cfg.TenantCacheSize)
	}
}

func TestLoadConfig_CacheSizeInvalidFallsBackToDefault(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_KTYPE_CACHE_SIZE", "not-a-number")
	t.Setenv("KAPP_AUTHZ_CACHE_SIZE", "0")
	t.Setenv("KAPP_TENANT_CACHE_SIZE", "-1")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KTypeCacheSize != 1024 {
		t.Errorf("KTypeCacheSize = %d, want 1024 (fallback)", cfg.KTypeCacheSize)
	}
	if cfg.AuthzCacheSize != 512 {
		t.Errorf("AuthzCacheSize = %d, want 512 (fallback for 0)", cfg.AuthzCacheSize)
	}
	if cfg.TenantCacheSize != 256 {
		t.Errorf("TenantCacheSize = %d, want 256 (fallback for -1)", cfg.TenantCacheSize)
	}
}

// TestLoadConfig_RequireRedisGate locks in the Phase 3 hardening:
// KAPP_REQUIRE_REDIS=1 with no REDIS_URL must fail boot loudly rather
// than silently degrading to per-pod in-process rate limiting. The
// inverse (REDIS_URL set OR KAPP_REQUIRE_REDIS unset) must boot
// cleanly so local dev and production deployments are both well-
// behaved without configuration gymnastics.
//
// This matters because the previous behaviour — log a warning and
// fall back — was indistinguishable in logs from a Redis outage and
// could leave a production deploy running with per-pod rate limits
// indefinitely. The gate forces an explicit operator choice.
func TestLoadConfig_RequireRedisGate(t *testing.T) {
	cases := []struct {
		name         string
		requireRedis string
		redisURL     string
		wantErr      bool
	}{
		{"unset and no redis url - dev default", "", "", false},
		{"unset and redis url present", "", "redis://localhost:6379", false},
		{"required and redis url present", "1", "redis://localhost:6379", false},
		{"required and no redis url", "1", "", true},
		{"required=true alias", "true", "", true},
		{"required=TRUE alias", "TRUE", "", true},
		{"required=0 explicit opt-out", "0", "", false},
		{"required=false explicit opt-out", "false", "", false},
		{"required=unrecognised falls back to default false", "yes", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DB_URL", "postgres://localhost/test")
			t.Setenv("KAPP_REQUIRE_REDIS", tc.requireRedis)
			t.Setenv("REDIS_URL", tc.redisURL)
			cfg, err := LoadConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error from LoadConfig but got cfg=%+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if cfg.RedisURL != tc.redisURL {
				t.Errorf("RedisURL = %q, want %q", cfg.RedisURL, tc.redisURL)
			}
		})
	}
}

// TestLoadConfig_EnvAndLogDefaults verifies that the new Phase 4
// observability config keys default safely when unset.
func TestLoadConfig_EnvAndLogDefaults(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_ENV", "")
	t.Setenv("KAPP_LOG_FORMAT", "")
	t.Setenv("KAPP_LOG_LEVEL", "")
	t.Setenv("KAPP_METRICS_ADDR", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Env != "dev" {
		t.Errorf("Env: want dev, got %q", cfg.Env)
	}
	if cfg.LogFormat != "" {
		t.Errorf("LogFormat: want empty (NewLogger picks default), got %q", cfg.LogFormat)
	}
	if cfg.LogLevel != "" {
		t.Errorf("LogLevel: want empty (parseLevel picks info), got %q", cfg.LogLevel)
	}
	if cfg.MetricsAddr != "" {
		t.Errorf("MetricsAddr: want empty (legacy in-router mount), got %q", cfg.MetricsAddr)
	}
}

// TestLoadConfig_ValidateLogFormat verifies typo'd KAPP_LOG_FORMAT
// values fail the boot loudly. The slog NewLogger function silently
// falls back to text on unknown values, which would mask a production
// misconfiguration (operator sets KAPP_LOG_FORMAT=jsom and gets text
// output for weeks before noticing). LoadConfig surfaces it at boot.
func TestLoadConfig_ValidateLogFormat(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")

	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty", "", false},
		{"json", "json", false},
		{"text", "text", false},
		{"typo-jsom", "jsom", true},
		// Uppercase is accepted (case-insensitive) so an operator
		// setting KAPP_LOG_FORMAT=JSON doesn't get a boot failure
		// while KAPP_LOG_LEVEL=INFO would be accepted (which would
		// be a surprising inconsistency). NewLogger likewise
		// normalises via strings.ToLower.
		{"uppercase-JSON", "JSON", false},
		{"uppercase-Text", "Text", false},
		{"typo-syslog", "syslog", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAPP_LOG_FORMAT", tc.value)
			_, err := LoadConfig()
			if (err != nil) != tc.wantErr {
				t.Errorf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestLoadConfig_ValidateLogLevel verifies typo'd KAPP_LOG_LEVEL
// values fail the boot loudly. Same rationale as LogFormat: silent
// fallback to info would mask a debug-mode-in-production attempt.
func TestLoadConfig_ValidateLogLevel(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")

	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty", "", false},
		{"debug", "debug", false},
		{"info", "info", false},
		{"warn", "warn", false},
		{"warning_alias", "warning", false},
		{"error", "error", false},
		{"err_alias", "err", false},
		{"uppercase", "INFO", false},
		{"typo-verbose", "verbose", true},
		{"typo-trace", "trace", true},
		{"typo-fatal", "fatal", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KAPP_LOG_LEVEL", tc.value)
			_, err := LoadConfig()
			if (err != nil) != tc.wantErr {
				t.Errorf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestLoadConfig_ValidateCachePositive verifies that explicit zero or
// negative cache sizes fail boot. getenvInt already falls back to the
// default on invalid input, so this test catches a future regression
// where a refactor of getenvInt accidentally accepts zero.
func TestLoadConfig_ValidateCachePositive(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	// All cache-size env vars cleared so they fall back to safe
	// defaults; Validate should pass cleanly. Use t.Setenv to
	// register cleanup with the test framework rather than
	// os.Unsetenv (which would leak across tests).
	t.Setenv("KAPP_KTYPE_CACHE_SIZE", "")
	t.Setenv("KAPP_AUTHZ_CACHE_SIZE", "")
	t.Setenv("KAPP_TENANT_CACHE_SIZE", "")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with defaults: %v", err)
	}
	if cfg.KTypeCacheSize <= 0 || cfg.AuthzCacheSize <= 0 || cfg.TenantCacheSize <= 0 {
		t.Errorf("default cache sizes should be positive; got %+v", cfg)
	}

	// Force a zero into the struct (simulating a future getenvInt
	// regression) and verify Validate rejects it.
	cfg.TenantCacheSize = 0
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject zero TenantCacheSize")
	}
}
