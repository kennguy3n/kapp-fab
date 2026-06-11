package platform

import (
	"strings"
	"testing"
)

func TestPlaidConfigured(t *testing.T) {
	cases := []struct {
		id, secret string
		want       bool
	}{
		{"", "", false},
		{"id", "", false},
		{"", "sec", false},
		{"id", "sec", true},
	}
	for _, tc := range cases {
		c := &Config{PlaidClientID: tc.id, PlaidSecret: tc.secret}
		if got := c.PlaidConfigured(); got != tc.want {
			t.Errorf("PlaidConfigured(%q,%q) = %v; want %v", tc.id, tc.secret, got, tc.want)
		}
	}
}

func TestGoCardlessConfigured(t *testing.T) {
	cases := []struct {
		id, key string
		want    bool
	}{
		{"", "", false},
		{"id", "", false},
		{"", "key", false},
		{"id", "key", true},
	}
	for _, tc := range cases {
		c := &Config{GoCardlessSecretID: tc.id, GoCardlessSecretKey: tc.key}
		if got := c.GoCardlessConfigured(); got != tc.want {
			t.Errorf("GoCardlessConfigured(%q,%q) = %v; want %v", tc.id, tc.key, got, tc.want)
		}
	}
}

// prodValidConfig returns a hand-built Config that passes every
// production gate in Validate EXCEPT the bank-feed checks, so each test
// can toggle just the bank-feed fields it cares about.
func prodValidConfig() *Config {
	return &Config{
		DatabaseURL:      "postgres://localhost/test",
		Env:              "production",
		JWTSecretPresent: true,
		MasterKeyPresent: true,
		RedisURL:         "redis://localhost:6379",
		RequireRedis:     true,
		KTypeCacheSize:   16,
		AuthzCacheSize:   16,
		TenantCacheSize:  16,
	}
}

func TestValidateProductionBaselinePasses(t *testing.T) {
	if err := prodValidConfig().Validate(); err != nil {
		t.Fatalf("baseline production config should validate: %v", err)
	}
}

func TestValidateProductionPlaidHalfConfiguredFailsClosed(t *testing.T) {
	c := prodValidConfig()
	c.PlaidClientID = "id" // secret intentionally missing
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "PLAID") {
		t.Fatalf("err = %v; want a Plaid fail-closed error", err)
	}

	c2 := prodValidConfig()
	c2.PlaidSecret = "sec" // id intentionally missing
	if err := c2.Validate(); err == nil {
		t.Fatal("secret without client id should fail closed in production")
	}
}

func TestValidateProductionPlaidFullyConfiguredPasses(t *testing.T) {
	c := prodValidConfig()
	c.PlaidClientID = "id"
	c.PlaidSecret = "sec"
	c.PlaidEnv = "production"
	if err := c.Validate(); err != nil {
		t.Fatalf("fully-configured Plaid should validate: %v", err)
	}
}

func TestValidateProductionPlaidBadEnvFails(t *testing.T) {
	c := prodValidConfig()
	c.PlaidClientID = "id"
	c.PlaidSecret = "sec"
	c.PlaidEnv = "staging" // not a valid Plaid host
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PLAID_ENV") {
		t.Fatalf("err = %v; want PLAID_ENV validation error", err)
	}
}

func TestValidateProductionGoCardlessHalfConfiguredFailsClosed(t *testing.T) {
	c := prodValidConfig()
	c.GoCardlessSecretID = "id" // key missing
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "GOCARDLESS") {
		t.Fatalf("err = %v; want GoCardless fail-closed error", err)
	}
}

func TestValidateNonProductionDoesNotFailOnHalfConfig(t *testing.T) {
	c := prodValidConfig()
	c.Env = "staging"
	c.PlaidClientID = "id" // half-configured, but non-production
	if err := c.Validate(); err != nil {
		t.Fatalf("non-production half-config must not fail Validate: %v", err)
	}
}

func TestWarningsFlagHalfConfiguredFeedsOutsideProduction(t *testing.T) {
	c := prodValidConfig()
	c.Env = "staging"
	c.CSRFCookieSecure = true // silence the unrelated CSRF warning
	c.PlaidClientID = "id"
	c.GoCardlessSecretKey = "key"
	ws := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(ws, "KAPP_PLAID") {
		t.Errorf("expected a Plaid half-config warning; got %q", ws)
	}
	if !strings.Contains(ws, "KAPP_GOCARDLESS") {
		t.Errorf("expected a GoCardless half-config warning; got %q", ws)
	}
}

func TestWarningsSilentWhenFeedsFullyConfigured(t *testing.T) {
	c := prodValidConfig()
	c.Env = "staging"
	c.CSRFCookieSecure = true
	c.PlaidClientID = "id"
	c.PlaidSecret = "sec"
	for _, w := range c.Warnings() {
		if strings.Contains(w, "KAPP_PLAID") {
			t.Errorf("did not expect a Plaid warning when fully configured; got %q", w)
		}
	}
}
