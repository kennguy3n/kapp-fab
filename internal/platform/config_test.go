package platform

import (
	"os"
	"testing"
)

func TestLoadConfig_CacheSizeDefaults(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	// Unset all cache-size env vars to verify defaults.
	os.Unsetenv("KAPP_KTYPE_CACHE_SIZE")
	os.Unsetenv("KAPP_AUTHZ_CACHE_SIZE")
	os.Unsetenv("KAPP_TENANT_CACHE_SIZE")

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
