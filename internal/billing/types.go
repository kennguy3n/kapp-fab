package billing

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Subscription status values. These mirror the subset of Stripe
// subscription statuses the control plane acts on; we store Stripe's
// string verbatim so an unforeseen status (e.g. "paused") round-trips
// without data loss even if no Kapp logic keys on it yet.
const (
	SubStatusTrialing = "trialing"
	SubStatusActive   = "active"
	SubStatusPastDue  = "past_due"
	SubStatusCanceled = "canceled"
	SubStatusUnpaid   = "unpaid"
	// SubStatusIncomplete is the status a subscription holds between
	// creation and the first successful payment when Checkout has
	// not yet completed.
	SubStatusIncomplete = "incomplete"
)

// Invoice status values mirrored from Stripe.
const (
	InvoiceStatusPaid          = "paid"
	InvoiceStatusOpen          = "open"
	InvoiceStatusVoid          = "void"
	InvoiceStatusUncollectible = "uncollectible"
	InvoiceStatusPaymentFailed = "payment_failed"
)

// Subscription is one row of billing_subscriptions — a tenant's
// current Stripe subscription. Exactly one row exists per tenant
// (enforced by the UNIQUE(tenant_id) in migration 000078); the store
// upserts on tenant_id.
type Subscription struct {
	ID                       uuid.UUID  `json:"id"`
	TenantID                 uuid.UUID  `json:"tenant_id"`
	Plan                     string     `json:"plan"`
	Status                   string     `json:"status"`
	StripeCustomerID         string     `json:"stripe_customer_id"`
	StripeSubscriptionID     string     `json:"stripe_subscription_id"`
	StripeSubscriptionItemID string     `json:"stripe_subscription_item_id"`
	CancelAtPeriodEnd        bool       `json:"cancel_at_period_end"`
	CurrentPeriodEnd         *time.Time `json:"current_period_end,omitempty"`
	TrialEnd                 *time.Time `json:"trial_end,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

// Invoice is one row of billing_invoices.
type Invoice struct {
	ID               uuid.UUID  `json:"id"`
	TenantID         uuid.UUID  `json:"tenant_id"`
	StripeInvoiceID  string     `json:"stripe_invoice_id"`
	Status           string     `json:"status"`
	AmountDue        int64      `json:"amount_due"`
	AmountPaid       int64      `json:"amount_paid"`
	Currency         string     `json:"currency"`
	HostedInvoiceURL string     `json:"hosted_invoice_url,omitempty"`
	PeriodStart      *time.Time `json:"period_start,omitempty"`
	PeriodEnd        *time.Time `json:"period_end,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

// Sentinel errors surfaced by the billing layer. Handlers map these
// to HTTP status codes; keeping them typed (rather than ad-hoc
// strings) lets the API translate "disabled" to 503, "no
// subscription" to 404, etc.
var (
	// ErrBillingDisabled is returned when a paid-plan operation is
	// attempted but Stripe is not configured (Config.Enabled() is
	// false).
	ErrBillingDisabled = errors.New("billing: stripe is not configured")

	// ErrNoSubscription is returned when a tenant has no
	// billing_subscriptions row (e.g. a free-plan tenant asking for
	// a portal session).
	ErrNoSubscription = errors.New("billing: tenant has no subscription")

	// ErrPlanNotPayable is returned when Subscribe is called for the
	// free plan or a plan with no configured Stripe price id.
	ErrPlanNotPayable = errors.New("billing: plan does not require payment")

	// ErrInvalidSignature is returned when a webhook body fails the
	// Stripe-Signature HMAC check.
	ErrInvalidSignature = errors.New("billing: invalid webhook signature")

	// ErrUnknownCustomer is returned when a webhook references a
	// Stripe customer/subscription that maps to no local tenant.
	ErrUnknownCustomer = errors.New("billing: no tenant for stripe customer")
)
