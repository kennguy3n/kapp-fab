package main

import (
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// TestSidecarRequiresJWTAtBoot pins the boot-time JWT gate: outside a
// development environment the sidecar must refuse to boot without a
// signer (default true), while development and an explicit
// KAPP_REQUIRE_JWT=0 opt-out keep the legacy header bridge available.
func TestSidecarRequiresJWTAtBoot(t *testing.T) {
	cases := []struct {
		name       string
		env        string
		requireJWT bool
		want       bool
	}{
		{"empty env (dev) does not require", "", true, false},
		{"development does not require", "development", true, false},
		{"dev alias does not require", "dev", true, false},
		{"test env does not require", "test", true, false},
		{"production default requires", "production", true, true},
		{"prod alias default requires", "prod", true, true},
		{"staging default requires", "staging", true, true},
		{"production opt-out does not require", "production", false, false},
		{"staging opt-out does not require", "staging", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &platform.Config{Env: tc.env, RequireJWT: tc.requireJWT}
			if got := sidecarRequiresJWTAtBoot(cfg); got != tc.want {
				t.Errorf("sidecarRequiresJWTAtBoot(env=%q, requireJWT=%v) = %v; want %v",
					tc.env, tc.requireJWT, got, tc.want)
			}
		})
	}
}

// TestSidecarRequiresJWTAtBoot_ViaLoadConfig exercises the real
// platform.LoadConfig → sidecarRequiresJWTAtBoot path (rather than a
// hand-built Config), so the gate is pinned against LoadConfig's actual
// defaulting: KAPP_REQUIRE_JWT defaults true, and KAPP_ENV drives
// IsNonDev. REDIS_URL is supplied for the staging cases because
// KAPP_REQUIRE_REDIS also defaults true outside development.
func TestSidecarRequiresJWTAtBoot_ViaLoadConfig(t *testing.T) {
	cases := []struct {
		name       string
		env        string
		requireJWT string // raw KAPP_REQUIRE_JWT ("" = unset → defaults true)
		redisURL   string
		want       bool
	}{
		{"unset env defaults to dev → no boot gate", "", "", "", false},
		{"staging default requires JWT at boot", "staging", "", "redis://localhost:6379", true},
		{"staging opt-out disables boot gate", "staging", "0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DB_URL", "postgres://localhost/test")
			t.Setenv("KAPP_ENV", tc.env)
			t.Setenv("KAPP_REQUIRE_JWT", tc.requireJWT)
			t.Setenv("REDIS_URL", tc.redisURL)
			if tc.redisURL == "" {
				t.Setenv("KAPP_REQUIRE_REDIS", "0")
			} else {
				t.Setenv("KAPP_REQUIRE_REDIS", "")
			}

			cfg, err := platform.LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got := sidecarRequiresJWTAtBoot(cfg); got != tc.want {
				t.Errorf("sidecarRequiresJWTAtBoot via LoadConfig (env=%q, requireJWT=%q) = %v; want %v",
					tc.env, tc.requireJWT, got, tc.want)
			}
		})
	}
}
