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
