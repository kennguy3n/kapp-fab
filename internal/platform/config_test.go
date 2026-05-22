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
