package billing

import (
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// LoadConfig reads the price-id map from the environment, so the
// config tests set the vars via t.Setenv and round-trip through
// LoadConfig rather than constructing the unexported priceIDs map
// directly.
func loadConfigWithPrices(t *testing.T) Config {
	t.Helper()
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_123")
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_123")
	t.Setenv("STRIPE_PRICE_ID_STARTER", "price_starter")
	t.Setenv("STRIPE_PRICE_ID_BUSINESS", "price_business")
	// Enterprise intentionally left unset to exercise the
	// "paid plan with no configured price" path.
	t.Setenv("STRIPE_PRICE_ID_ENTERPRISE", "")
	return LoadConfig()
}

func TestConfigEnabled(t *testing.T) {
	var zero Config
	if zero.Enabled() {
		t.Fatal("zero-value Config must report disabled")
	}
	cfg := loadConfigWithPrices(t)
	if !cfg.Enabled() {
		t.Fatal("config with a secret key must report enabled")
	}
}

func TestPriceIDForPlan(t *testing.T) {
	cfg := loadConfigWithPrices(t)
	if got := cfg.PriceIDForPlan(tenant.PlanStarter); got != "price_starter" {
		t.Fatalf("starter price = %q, want price_starter", got)
	}
	if got := cfg.PriceIDForPlan(tenant.PlanFree); got != "" {
		t.Fatalf("free plan must have no price id, got %q", got)
	}
	if got := cfg.PriceIDForPlan("nonsense"); got != "" {
		t.Fatalf("unknown plan must have no price id, got %q", got)
	}
}

func TestPlanForPriceID(t *testing.T) {
	cfg := loadConfigWithPrices(t)
	if got := cfg.PlanForPriceID("price_business"); got != tenant.PlanBusiness {
		t.Fatalf("price_business mapped to %q, want business", got)
	}
	// The empty price id (and any id unknown to this environment)
	// must not resolve to a plan — otherwise an unconfigured
	// enterprise price would silently collide.
	if got := cfg.PlanForPriceID(""); got != "" {
		t.Fatalf("empty price id resolved to %q, want empty", got)
	}
	if got := cfg.PlanForPriceID("price_unknown"); got != "" {
		t.Fatalf("unknown price id resolved to %q, want empty", got)
	}
}

func TestRequiresPayment(t *testing.T) {
	cfg := loadConfigWithPrices(t)
	cases := map[string]bool{
		tenant.PlanFree:       false, // free is always free
		tenant.PlanStarter:    true,  // has a configured price
		tenant.PlanBusiness:   true,
		tenant.PlanEnterprise: false, // paid plan but no price configured -> cannot charge
	}
	for plan, want := range cases {
		if got := cfg.RequiresPayment(plan); got != want {
			t.Errorf("RequiresPayment(%q) = %v, want %v", plan, got, want)
		}
	}
}
