// Package billing is the Workstream 1 SaaS control-plane billing
// layer. It wraps Stripe (a thin form-encoded HTTP client — no vendor
// SDK, matching the convention in internal/captcha and
// internal/auth), persists the subscription / invoice / event tables
// added by migrations/000078_billing.sql, and exposes a service the
// API handlers and the worker usage-sync job drive.
//
// The package depends on internal/tenant (plans, metering, status
// transitions) but tenant never imports billing — keeping the
// dependency edge one-directional avoids the import cycle the wider
// codebase is careful about (see internal/dbutil's package doc).
package billing

import (
	"os"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// defaultAPIBase is Stripe's REST origin. Overridable via
// STRIPE_API_BASE so tests (and any future Stripe-compatible mock)
// can point the client at a local server.
const defaultAPIBase = "https://api.stripe.com"

// Config holds the Stripe credentials and the per-plan price-id map
// the billing layer needs. It is loaded from the environment by
// LoadConfig; the zero value is a safe "billing disabled" config
// (Enabled reports false) so local dev and tests that never set the
// Stripe vars degrade to the free-plan-only path rather than erroring
// at boot.
type Config struct {
	// SecretKey is the Stripe secret API key (sk_test_… / sk_live_…)
	// used as the HTTP basic-auth username on every Stripe call.
	// Empty disables billing. Sourced from STRIPE_SECRET_KEY.
	SecretKey string

	// WebhookSecret is the signing secret (whsec_…) Stripe uses to
	// HMAC each webhook body. The webhook handler verifies the
	// Stripe-Signature header against it; an empty secret means the
	// handler rejects every webhook (fail-closed) rather than
	// trusting unauthenticated POSTs. Sourced from
	// STRIPE_WEBHOOK_SECRET.
	WebhookSecret string

	// APIBase is the Stripe REST origin. Defaults to
	// defaultAPIBase; overridable via STRIPE_API_BASE.
	APIBase string

	// ReturnURL is where Stripe redirects the browser back to after
	// a Checkout session or Billing Portal session completes. The
	// billing handlers append the Stripe-required {CHECKOUT_SESSION_ID}
	// only where Checkout needs it. Sourced from STRIPE_BILLING_RETURN_URL.
	ReturnURL string

	// priceIDs maps a canonical plan name (tenant.PlanStarter, …) to
	// its Stripe Price id. Only paid plans appear; the free plan has
	// no price and never reaches Stripe. Populated from
	// STRIPE_PRICE_ID_STARTER / _BUSINESS / _ENTERPRISE.
	priceIDs map[string]string
}

// LoadConfig reads the billing configuration from the environment.
// It never fails: missing Stripe vars simply yield a disabled config
// (Enabled() == false), which the service treats as "free plan only,
// no Stripe calls". Production deployments that want paid plans must
// set STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET, and the per-plan
// STRIPE_PRICE_ID_* vars; the fail-closed production guard for that
// lives in the API boot path, not here, so unit tests can build a
// service without a full Stripe setup.
func LoadConfig() Config {
	apiBase := strings.TrimRight(os.Getenv("STRIPE_API_BASE"), "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	return Config{
		SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		APIBase:       apiBase,
		ReturnURL:     os.Getenv("STRIPE_BILLING_RETURN_URL"),
		priceIDs: map[string]string{
			tenant.PlanStarter:    os.Getenv("STRIPE_PRICE_ID_STARTER"),
			tenant.PlanBusiness:   os.Getenv("STRIPE_PRICE_ID_BUSINESS"),
			tenant.PlanEnterprise: os.Getenv("STRIPE_PRICE_ID_ENTERPRISE"),
		},
	}
}

// Enabled reports whether Stripe is wired. When false the service
// short-circuits every Stripe call: free signups still work, paid
// subscribe attempts return ErrBillingDisabled, and the usage-sync
// job is a no-op.
func (c Config) Enabled() bool {
	return c.SecretKey != ""
}

// PriceIDForPlan returns the Stripe Price id configured for the named
// plan, or "" when the plan is free or no price is configured. A
// non-empty result is the signal that a plan requires Stripe
// checkout.
func (c Config) PriceIDForPlan(plan string) string {
	if c.priceIDs == nil {
		return ""
	}
	return c.priceIDs[plan]
}

// PlanForPriceID is the reverse of PriceIDForPlan: given a Stripe
// Price id (as it appears on a subscription item in a webhook
// payload) it returns the canonical plan name, or "" when the price
// id is unknown to this environment. Used by the webhook handler to
// translate a Stripe subscription back into a Kapp plan.
func (c Config) PlanForPriceID(priceID string) string {
	if priceID == "" {
		return ""
	}
	for plan, id := range c.priceIDs {
		if id != "" && id == priceID {
			return plan
		}
	}
	return ""
}

// RequiresPayment reports whether moving a tenant onto the named plan
// needs a Stripe checkout. The free plan (and any plan without a
// configured price id) is free; everything else requires payment.
func (c Config) RequiresPayment(plan string) bool {
	return plan != tenant.PlanFree && c.PriceIDForPlan(plan) != ""
}
