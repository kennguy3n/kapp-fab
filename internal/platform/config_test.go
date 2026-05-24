package platform

import (
	"testing"
	"time"
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

func TestLoadConfig_SecretsAndJWTDefaults(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	for _, k := range []string{
		"KAPP_SECRET_PROVIDER",
		"KAPP_SECRETS_ENV_PREFIX",
		"KAPP_SECRETS_FILE_ROOT_DIR",
		"KAPP_SECRETS_AWS_REGION",
		"KAPP_SECRETS_VAULT_ADDR",
		"KAPP_JWT_PRIMARY_REF",
		"KAPP_JWT_VERIFY_REFS",
		"KAPP_JWT_ALGORITHM",
		"KAPP_JWT_ISSUER",
		"KAPP_JWT_AUDIENCE",
		"KAPP_JWT_ACCESS_TTL",
		"KAPP_JWT_REFRESH_TTL",
		"KAPP_JWT_LEEWAY",
		"KAPP_JWT_KEYRING_REFRESH_INTERVAL",
	} {
		t.Setenv(k, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SecretProvider != "" {
		t.Errorf("SecretProvider default should be empty, got %q", cfg.SecretProvider)
	}
	if cfg.SecretsEnvPrefix != "KAPP_" {
		t.Errorf("SecretsEnvPrefix = %q want KAPP_", cfg.SecretsEnvPrefix)
	}
	if cfg.JWTPrimaryRef != "jwt/primary" {
		t.Errorf("JWTPrimaryRef = %q want jwt/primary", cfg.JWTPrimaryRef)
	}
	if cfg.JWTAlgorithm != "HS256" {
		t.Errorf("JWTAlgorithm = %q want HS256", cfg.JWTAlgorithm)
	}
	if cfg.JWTIssuer != "kapp" {
		t.Errorf("JWTIssuer = %q want kapp", cfg.JWTIssuer)
	}
	if cfg.JWTAccessTTL.String() != "15m0s" {
		t.Errorf("JWTAccessTTL default = %s want 15m0s", cfg.JWTAccessTTL)
	}
	if cfg.JWTRefreshTTL.String() != "24h0m0s" {
		t.Errorf("JWTRefreshTTL default = %s want 24h0m0s", cfg.JWTRefreshTTL)
	}
	if cfg.JWTLeeway.String() != "30s" {
		t.Errorf("JWTLeeway default = %s want 30s", cfg.JWTLeeway)
	}
	if cfg.JWTKeyringRefreshInterval.String() != "1m0s" {
		t.Errorf("JWTKeyringRefreshInterval default = %s want 1m0s", cfg.JWTKeyringRefreshInterval)
	}
	if len(cfg.JWTVerifyRefs) != 0 {
		t.Errorf("JWTVerifyRefs default should be empty, got %v", cfg.JWTVerifyRefs)
	}
}

func TestLoadConfig_SecretsAndJWTFromEnv(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_SECRET_PROVIDER", "vault")
	t.Setenv("KAPP_SECRETS_VAULT_ADDR", "https://vault.example.com")
	t.Setenv("KAPP_SECRETS_VAULT_TOKEN", "test-token")
	t.Setenv("KAPP_SECRETS_VAULT_MOUNT_PATH", "kv")
	t.Setenv("KAPP_JWT_PRIMARY_REF", "secret/jwt/active")
	t.Setenv("KAPP_JWT_VERIFY_REFS", "secret/jwt/prev , secret/jwt/older")
	t.Setenv("KAPP_JWT_ALGORITHM", "RS256")
	t.Setenv("KAPP_JWT_ACCESS_TTL", "5m")
	t.Setenv("KAPP_JWT_REFRESH_TTL", "12h")
	t.Setenv("KAPP_JWT_KEYRING_REFRESH_INTERVAL", "5s")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SecretProvider != "vault" {
		t.Errorf("SecretProvider = %q want vault", cfg.SecretProvider)
	}
	if cfg.SecretsVaultAddr != "https://vault.example.com" {
		t.Errorf("SecretsVaultAddr = %q", cfg.SecretsVaultAddr)
	}
	if cfg.JWTAlgorithm != "RS256" {
		t.Errorf("JWTAlgorithm = %q want RS256", cfg.JWTAlgorithm)
	}
	if cfg.JWTAccessTTL.String() != "5m0s" {
		t.Errorf("JWTAccessTTL = %s want 5m0s", cfg.JWTAccessTTL)
	}
	if len(cfg.JWTVerifyRefs) != 2 || cfg.JWTVerifyRefs[0] != "secret/jwt/prev" || cfg.JWTVerifyRefs[1] != "secret/jwt/older" {
		t.Errorf("JWTVerifyRefs trimmed split mismatch: %v", cfg.JWTVerifyRefs)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{",,,", nil},
		{"a", []string{"a"}},
		{" a , b , c ", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitCSV(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitCSV(%q) = %v want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestGetenvDuration(t *testing.T) {
	t.Setenv("KAPP_TEST_DUR", "5m")
	if got := getenvDuration("KAPP_TEST_DUR", time.Second); got.String() != "5m0s" {
		t.Errorf("getenvDuration = %s want 5m0s", got)
	}
	t.Setenv("KAPP_TEST_DUR_BAD", "not-a-duration")
	if got := getenvDuration("KAPP_TEST_DUR_BAD", 7*time.Second); got != 7*time.Second {
		t.Errorf("invalid value should fall back to default, got %s", got)
	}
	t.Setenv("KAPP_TEST_DUR_ZERO", "0s")
	if got := getenvDuration("KAPP_TEST_DUR_ZERO", 7*time.Second); got != 7*time.Second {
		t.Errorf("zero value should fall back to default, got %s", got)
	}
}

// TestGetenvDurationAllowZero pins the contract that explicit
// zero values are HONOURED rather than treated as "unset". The
// canonical use case is KAPP_JWT_LEEWAY=0s, which per
// SignerConfig.Leeway docs disables the clock-skew grace
// window — strict-clock-skew is rare but real for audit-bound
// deployments. Without this variant, operators who set
// LEEWAY=0s would be silently upgraded to the 30s default.
//
// Negative values stay rejected (malformed input).
func TestGetenvDurationAllowZero(t *testing.T) {
	t.Setenv("KAPP_TEST_DUR_AZ_VALID", "5m")
	if got := getenvDurationAllowZero("KAPP_TEST_DUR_AZ_VALID", time.Second); got.String() != "5m0s" {
		t.Errorf("getenvDurationAllowZero = %s want 5m0s", got)
	}
	t.Setenv("KAPP_TEST_DUR_AZ_ZERO", "0s")
	if got := getenvDurationAllowZero("KAPP_TEST_DUR_AZ_ZERO", 7*time.Second); got != 0 {
		t.Errorf("explicit zero should be honoured, got %s", got)
	}
	t.Setenv("KAPP_TEST_DUR_AZ_BAD", "not-a-duration")
	if got := getenvDurationAllowZero("KAPP_TEST_DUR_AZ_BAD", 7*time.Second); got != 7*time.Second {
		t.Errorf("invalid value should fall back to default, got %s", got)
	}
	t.Setenv("KAPP_TEST_DUR_AZ_NEG", "-1s")
	if got := getenvDurationAllowZero("KAPP_TEST_DUR_AZ_NEG", 7*time.Second); got != 7*time.Second {
		t.Errorf("negative value should fall back to default, got %s", got)
	}
	// Unset → fallback (don't t.Setenv anything for this key).
	if got := getenvDurationAllowZero("KAPP_TEST_DUR_AZ_UNSET", 7*time.Second); got != 7*time.Second {
		t.Errorf("unset value should fall back to default, got %s", got)
	}
}

// TestLoadConfig_JWTLeewayZeroHonoured pins the end-to-end
// behaviour: KAPP_JWT_LEEWAY=0s makes it through LoadConfig
// as zero (not the 30s default). Without the dedicated
// allow-zero helper this would silently upgrade.
func TestLoadConfig_JWTLeewayZeroHonoured(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	t.Setenv("KAPP_JWT_LEEWAY", "0s")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.JWTLeeway != 0 {
		t.Errorf("KAPP_JWT_LEEWAY=0s should resolve to 0, got %s", cfg.JWTLeeway)
	}
}
