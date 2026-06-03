package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// trialGrace is the window after a trial's end during which a tenant
// keeps access before EnforceTrialExpiry suspends it. The spec calls
// for a 7-day grace period so a tenant whose card fails on the trial-
// to-paid transition isn't locked out instantly.
const trialGrace = 7 * 24 * time.Hour

// TenantController is the slice of tenant.PGStore the billing service
// drives. Declaring it as an interface keeps the service unit-
// testable with an in-memory fake.
type TenantController interface {
	Get(ctx context.Context, id uuid.UUID) (*tenant.Tenant, error)
	UpdatePlan(ctx context.Context, id uuid.UUID, plan string, quota []byte) error
	Suspend(ctx context.Context, id uuid.UUID) error
	Activate(ctx context.Context, id uuid.UUID) error
}

// PlanResolver resolves a plan definition by name
// (satisfied by *tenant.PlanStore).
type PlanResolver interface {
	Get(ctx context.Context, name string) (*tenant.Plan, error)
}

// FeatureSetter resets a tenant's feature flags to a plan's defaults
// (satisfied by *tenant.FeatureStore).
type FeatureSetter interface {
	SetFeatures(ctx context.Context, tenantID uuid.UUID, features map[string]bool) error
}

// UsageReader reads a tenant's current-period metering counters
// (satisfied by *tenant.MeteringStore).
type UsageReader interface {
	GetUsage(ctx context.Context, tenantID uuid.UUID, periodStart time.Time, metric string) (int64, error)
	CurrentPeriod() time.Time
}

// Service is the billing orchestration layer. It is the only place
// the Stripe client, the billing store, and the tenant control-plane
// stores meet. Handlers and the worker job call into it; it never
// imports the HTTP layer.
type Service struct {
	cfg      Config
	stripe   StripeAPI
	store    *Store
	tenants  TenantController
	plans    PlanResolver
	features FeatureSetter
	usage    UsageReader
	logger   *slog.Logger
	now      func() time.Time
}

// ServiceDeps bundles the collaborators NewService needs. Grouping
// them in a struct keeps the constructor readable as the dependency
// set grows.
type ServiceDeps struct {
	Config   Config
	Stripe   StripeAPI
	Store    *Store
	Tenants  TenantController
	Plans    PlanResolver
	Features FeatureSetter
	Usage    UsageReader
	Logger   *slog.Logger
}

// NewService wires a billing Service. Logger defaults to
// slog.Default() when nil.
func NewService(d ServiceDeps) *Service {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:      d.Config,
		stripe:   d.Stripe,
		store:    d.Store,
		tenants:  d.Tenants,
		plans:    d.Plans,
		features: d.Features,
		usage:    d.Usage,
		logger:   logger,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SubscribeResult is returned by Subscribe. For a paid plan
// CheckoutURL is the hosted Stripe Checkout page the caller redirects
// the browser to; the plan only actually switches once Stripe
// confirms payment via webhook. For the free plan the switch is
// applied immediately and CheckoutURL is empty.
type SubscribeResult struct {
	Plan            string `json:"plan"`
	RequiresPayment bool   `json:"requires_payment"`
	CheckoutURL     string `json:"checkout_url,omitempty"`
}

// Subscribe moves a tenant toward the named plan.
//
//   - Free plan: applied immediately (UpdatePlan + feature reset) and
//     any prior subscription is marked canceled. No Stripe call.
//   - Paid plan: requires Stripe to be configured and a price id for
//     the plan. Ensures a Stripe customer exists, opens a Checkout
//     session (carrying the plan's trial days), records an
//     "incomplete" subscription row, and returns the Checkout URL.
//     The plan/feature switch is deferred to the webhook that fires
//     when payment succeeds — this is what enforces "downgrades take
//     effect at the next billing cycle" and "paid plans require
//     checkout".
func (s *Service) Subscribe(ctx context.Context, tenantID uuid.UUID, planName, successURL, cancelURL string) (*SubscribeResult, error) {
	tn, err := s.tenants.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	plan, err := s.plans.Get(ctx, planName)
	if err != nil {
		return nil, err
	}

	if !s.cfg.RequiresPayment(plan.Name) {
		// Free (or unpriced) plan — apply immediately, no Stripe.
		if err := s.applyPlan(ctx, tenantID, plan); err != nil {
			return nil, err
		}
		if _, err := s.store.UpsertSubscription(ctx, Subscription{
			TenantID: tenantID,
			Plan:     plan.Name,
			Status:   SubStatusActive,
		}); err != nil {
			return nil, err
		}
		return &SubscribeResult{Plan: plan.Name, RequiresPayment: false}, nil
	}

	if !s.cfg.Enabled() {
		return nil, ErrBillingDisabled
	}
	priceID := s.cfg.PriceIDForPlan(plan.Name)
	if priceID == "" {
		return nil, ErrPlanNotPayable
	}

	// Reuse the tenant's existing Stripe customer if it already has a
	// subscription row (e.g. an upgrade from a prior paid plan) so we
	// don't orphan customers on every plan change.
	customerID := ""
	if existing, gerr := s.store.GetSubscriptionByTenant(ctx, tenantID); gerr == nil {
		customerID = existing.StripeCustomerID
	} else if !errors.Is(gerr, ErrNoSubscription) {
		return nil, gerr
	}
	if customerID == "" {
		customerID, err = s.stripe.CreateCustomer(ctx, CustomerParams{
			Name:     tn.Name,
			TenantID: tenantID.String(),
		})
		if err != nil {
			return nil, err
		}
	}

	session, err := s.stripe.CreateCheckoutSession(ctx, CheckoutParams{
		CustomerID:      customerID,
		PriceID:         priceID,
		TrialPeriodDays: plan.TrialDays,
		SuccessURL:      s.sanitizeReturnURL(successURL),
		CancelURL:       s.sanitizeReturnURL(cancelURL),
		TenantID:        tenantID.String(),
	})
	if err != nil {
		return nil, err
	}

	// Record the pending subscription so a webhook arriving before
	// the browser returns can resolve the tenant by customer id.
	if _, err := s.store.UpsertSubscription(ctx, Subscription{
		TenantID:         tenantID,
		Plan:             plan.Name,
		Status:           SubStatusIncomplete,
		StripeCustomerID: customerID,
	}); err != nil {
		return nil, err
	}

	return &SubscribeResult{
		Plan:            plan.Name,
		RequiresPayment: true,
		CheckoutURL:     session.URL,
	}, nil
}

// sanitizeReturnURL guards the post-checkout redirect targets. Stripe
// sends the user's browser to SuccessURL / CancelURL after checkout, so
// a caller who replayed a stolen tenant JWT could otherwise steer the
// victim to an attacker-controlled origin (a post-payment phishing
// page). A client value is accepted only when it is an absolute http(s)
// URL whose scheme+host match the operator-configured ReturnURL;
// anything else falls back to ReturnURL (which may be empty, in which
// case Stripe uses the account's default return and no client value is
// honored). This keeps redirects pinned to the app's own origin while
// still letting the frontend choose a same-origin landing path.
func (s *Service) sanitizeReturnURL(candidate string) string {
	if candidate == "" {
		return s.cfg.ReturnURL
	}
	if s.cfg.ReturnURL == "" {
		// No trusted base to compare against — refuse the client value.
		return ""
	}
	base, err := url.Parse(s.cfg.ReturnURL)
	if err != nil {
		return s.cfg.ReturnURL
	}
	u, err := url.Parse(candidate)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return s.cfg.ReturnURL
	}
	if !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return s.cfg.ReturnURL
	}
	return candidate
}

// PortalSession opens a Stripe Billing Portal session for the
// tenant's customer and returns the hosted URL. Returns
// ErrNoSubscription when the tenant has never subscribed (no Stripe
// customer to manage).
func (s *Service) PortalSession(ctx context.Context, tenantID uuid.UUID) (string, error) {
	if !s.cfg.Enabled() {
		return "", ErrBillingDisabled
	}
	sub, err := s.store.GetSubscriptionByTenant(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if sub.StripeCustomerID == "" {
		return "", ErrNoSubscription
	}
	return s.stripe.CreatePortalSession(ctx, sub.StripeCustomerID, s.cfg.ReturnURL)
}

// GetSubscription returns the tenant's subscription row, or
// ErrNoSubscription.
func (s *Service) GetSubscription(ctx context.Context, tenantID uuid.UUID) (*Subscription, error) {
	return s.store.GetSubscriptionByTenant(ctx, tenantID)
}

// ListInvoices returns the tenant's invoice history (newest-first).
func (s *Service) ListInvoices(ctx context.Context, tenantID uuid.UUID, limit int) ([]Invoice, error) {
	return s.store.ListInvoices(ctx, tenantID, limit)
}

// HandleWebhook verifies, records (idempotently), and applies a
// Stripe webhook. A redelivered event is recorded once and skipped on
// replay. Signature failures return ErrInvalidSignature so the
// handler can answer 400 without trusting the body.
func (s *Service) HandleWebhook(ctx context.Context, payload []byte, sigHeader string) error {
	event, err := ConstructEvent(payload, sigHeader, s.cfg.WebhookSecret, s.now())
	if err != nil {
		return err
	}

	tenantID, err := s.resolveTenant(ctx, event)
	if err != nil {
		// An event type we don't act on (Stripe sends dozens beyond the
		// four we handle). The signature already verified, so the
		// delivery is authentic — acknowledge it with success so Stripe
		// doesn't enter a 72h retry storm over events we intentionally
		// ignore. Only genuine processing failures should surface as an
		// error (→ non-2xx → Stripe retry).
		if errors.Is(err, errUnhandledEvent) {
			return nil
		}
		return err
	}

	firstTime, err := s.store.RecordEvent(ctx, tenantID, event.ID, event.Type, payload)
	if err != nil {
		return err
	}
	if !firstTime {
		// Stripe redelivered an event we already applied.
		return nil
	}

	if err := s.applyEvent(ctx, tenantID, event); err != nil {
		return err
	}
	return s.store.MarkEventProcessed(ctx, tenantID, event.ID)
}

// resolveTenant maps a webhook event onto its owning tenant. It tries
// the object's tenant_id metadata first (set on every Checkout
// subscription we create), then falls back to a reverse lookup by
// Stripe customer / subscription id against billing_subscriptions.
func (s *Service) resolveTenant(ctx context.Context, event Event) (uuid.UUID, error) {
	switch event.Type {
	case EventSubscriptionUpdated, EventSubscriptionDeleted:
		obj, err := parseSubscriptionObject(event.Data)
		if err != nil {
			return uuid.Nil, fmt.Errorf("billing: parse subscription object: %w", err)
		}
		if id := tenantIDFromMetadata(obj.Metadata); id != uuid.Nil {
			return id, nil
		}
		if obj.Customer != "" {
			if sub, err := s.store.GetSubscriptionByStripeCustomer(ctx, obj.Customer); err == nil {
				return sub.TenantID, nil
			}
		}
		if sub, err := s.store.GetSubscriptionByStripeSubID(ctx, obj.ID); err == nil {
			return sub.TenantID, nil
		}
		return uuid.Nil, ErrUnknownCustomer
	case EventInvoicePaid, EventInvoicePaymentFailed:
		obj, err := parseInvoiceObject(event.Data)
		if err != nil {
			return uuid.Nil, fmt.Errorf("billing: parse invoice object: %w", err)
		}
		if id := tenantIDFromMetadata(obj.Metadata); id != uuid.Nil {
			return id, nil
		}
		if obj.Customer != "" {
			if sub, err := s.store.GetSubscriptionByStripeCustomer(ctx, obj.Customer); err == nil {
				return sub.TenantID, nil
			}
		}
		if obj.Subscription != "" {
			if sub, err := s.store.GetSubscriptionByStripeSubID(ctx, obj.Subscription); err == nil {
				return sub.TenantID, nil
			}
		}
		return uuid.Nil, ErrUnknownCustomer
	default:
		// An event type we don't act on. Signal the caller to skip it
		// (HandleWebhook acknowledges with success rather than erroring,
		// so Stripe doesn't retry events we intentionally ignore).
		return uuid.Nil, errUnhandledEvent
	}
}

// errUnhandledEvent is an internal sentinel signalling resolveTenant
// could not (and need not) attribute an event type we don't act on.
var errUnhandledEvent = errors.New("billing: unhandled event type")

func tenantIDFromMetadata(md map[string]string) uuid.UUID {
	if md == nil {
		return uuid.Nil
	}
	if raw, ok := md["tenant_id"]; ok {
		if id, err := uuid.Parse(raw); err == nil {
			return id
		}
	}
	return uuid.Nil
}

// applyEvent dispatches a verified, first-seen event to its handler.
func (s *Service) applyEvent(ctx context.Context, tenantID uuid.UUID, event Event) error {
	switch event.Type {
	case EventInvoicePaid:
		return s.onInvoice(ctx, tenantID, event.Data, InvoiceStatusPaid)
	case EventInvoicePaymentFailed:
		return s.onInvoicePaymentFailed(ctx, tenantID, event.Data)
	case EventSubscriptionUpdated:
		return s.onSubscriptionUpdated(ctx, tenantID, event.Data)
	case EventSubscriptionDeleted:
		return s.onSubscriptionDeleted(ctx, tenantID, event.Data)
	default:
		return nil
	}
}

// onInvoice records the invoice and, for a paid invoice, makes sure
// the subscription is marked active and the tenant is un-suspended
// (a successful payment after a dunning suspension reinstates
// access).
func (s *Service) onInvoice(ctx context.Context, tenantID uuid.UUID, raw json.RawMessage, status string) error {
	obj, err := parseInvoiceObject(raw)
	if err != nil {
		return fmt.Errorf("billing: parse invoice object: %w", err)
	}
	if err := s.upsertInvoice(ctx, tenantID, obj, status); err != nil {
		return err
	}
	if status == InvoiceStatusPaid {
		if sub, err := s.store.GetSubscriptionByTenant(ctx, tenantID); err == nil {
			sub.Status = SubStatusActive
			if _, err := s.store.UpsertSubscription(ctx, *sub); err != nil {
				return err
			}
		}
		// Reinstate a tenant suspended during dunning. Activate only
		// transitions suspended → active, so an already-active tenant
		// is a harmless no-op handled below.
		if err := s.reactivateTenant(ctx, tenantID); err != nil {
			return err
		}
	}
	return nil
}

// onInvoicePaymentFailed records the failed invoice and marks the
// subscription past_due. We deliberately do NOT suspend on a single
// failure — Stripe's dunning retries and the trial-grace sweeper
// (EnforceTrialExpiry) own the eventual suspension so a transient
// card decline doesn't lock a paying tenant out mid-period.
func (s *Service) onInvoicePaymentFailed(ctx context.Context, tenantID uuid.UUID, raw json.RawMessage) error {
	obj, err := parseInvoiceObject(raw)
	if err != nil {
		return fmt.Errorf("billing: parse invoice object: %w", err)
	}
	if err := s.upsertInvoice(ctx, tenantID, obj, InvoiceStatusPaymentFailed); err != nil {
		return err
	}
	if sub, err := s.store.GetSubscriptionByTenant(ctx, tenantID); err == nil {
		sub.Status = SubStatusPastDue
		if _, err := s.store.UpsertSubscription(ctx, *sub); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) upsertInvoice(ctx context.Context, tenantID uuid.UUID, obj stripeInvoiceObject, status string) error {
	if obj.ID == "" {
		// Some invoice events (e.g. zero-amount trial invoices) can
		// arrive without an id we can key on; skip persisting rather
		// than violating the NOT NULL/UNIQUE constraint.
		return nil
	}
	if status == "" {
		status = obj.Status
	}
	return s.store.UpsertInvoice(ctx, Invoice{
		TenantID:         tenantID,
		StripeInvoiceID:  obj.ID,
		Status:           status,
		AmountDue:        obj.AmountDue,
		AmountPaid:       obj.AmountPaid,
		Currency:         obj.Currency,
		HostedInvoiceURL: obj.HostedInvoiceURL,
		PeriodStart:      epochPtr(obj.PeriodStart),
		PeriodEnd:        epochPtr(obj.PeriodEnd),
	})
}

// onSubscriptionUpdated syncs the local subscription row to the
// Stripe object and, when the subscription is in a paid-and-current
// state (active/trialing), applies the corresponding plan + feature
// set to the tenant. This is the single funnel through which a paid
// plan actually takes effect.
func (s *Service) onSubscriptionUpdated(ctx context.Context, tenantID uuid.UUID, raw json.RawMessage) error {
	obj, err := parseSubscriptionObject(raw)
	if err != nil {
		return fmt.Errorf("billing: parse subscription object: %w", err)
	}
	itemID, priceID := obj.firstItem()
	planName := s.cfg.PlanForPriceID(priceID)

	// Preserve the previously-recorded plan when Stripe sends a price
	// id this environment doesn't recognise (e.g. a legacy price), so
	// we never blank out a tenant's plan on an unrelated update.
	if planName == "" {
		if existing, gerr := s.store.GetSubscriptionByTenant(ctx, tenantID); gerr == nil {
			planName = existing.Plan
		}
	}

	if _, err := s.store.UpsertSubscription(ctx, Subscription{
		TenantID:                 tenantID,
		Plan:                     planName,
		Status:                   obj.Status,
		StripeCustomerID:         obj.Customer,
		StripeSubscriptionID:     obj.ID,
		StripeSubscriptionItemID: itemID,
		CancelAtPeriodEnd:        obj.CancelAtPeriodEnd,
		CurrentPeriodEnd:         epochPtr(obj.CurrentPeriodEnd),
		TrialEnd:                 epochPtr(obj.TrialEnd),
	}); err != nil {
		return err
	}

	// A subscription that is paid-and-current grants the plan. Other
	// statuses (past_due, unpaid, canceled, incomplete) leave the
	// tenant's current plan untouched — the dunning/trial sweeper and
	// the deleted handler own downgrades.
	if planName != "" && (obj.Status == SubStatusActive || obj.Status == SubStatusTrialing) {
		plan, err := s.plans.Get(ctx, planName)
		if err != nil {
			return err
		}
		if err := s.applyPlan(ctx, tenantID, plan); err != nil {
			return err
		}
		if err := s.reactivateTenant(ctx, tenantID); err != nil {
			return err
		}
	}
	return nil
}

// onSubscriptionDeleted marks the subscription canceled and downgrades
// the tenant to the free plan (resetting feature flags + quota). The
// tenant keeps free access rather than being suspended — losing a
// paid subscription is not a policy violation.
func (s *Service) onSubscriptionDeleted(ctx context.Context, tenantID uuid.UUID, raw json.RawMessage) error {
	obj, err := parseSubscriptionObject(raw)
	if err != nil {
		return fmt.Errorf("billing: parse subscription object: %w", err)
	}
	if sub, gerr := s.store.GetSubscriptionByTenant(ctx, tenantID); gerr == nil {
		sub.Status = SubStatusCanceled
		sub.Plan = tenant.PlanFree
		sub.CancelAtPeriodEnd = obj.CancelAtPeriodEnd
		if _, err := s.store.UpsertSubscription(ctx, *sub); err != nil {
			return err
		}
	}
	freePlan, err := s.plans.Get(ctx, tenant.PlanFree)
	if err != nil {
		return err
	}
	return s.applyPlan(ctx, tenantID, freePlan)
}

// applyPlan writes the plan name + quota onto the tenant and resets
// its feature flags to the plan's defaults — the same "reset to the
// new baseline" semantics the admin /tenants/{id}/plan endpoint uses.
func (s *Service) applyPlan(ctx context.Context, tenantID uuid.UUID, plan *tenant.Plan) error {
	quota, err := json.Marshal(plan.Limits)
	if err != nil {
		return fmt.Errorf("billing: marshal plan limits: %w", err)
	}
	if err := s.tenants.UpdatePlan(ctx, tenantID, plan.Name, quota); err != nil {
		return err
	}
	if err := s.features.SetFeatures(ctx, tenantID, plan.Features); err != nil {
		return err
	}
	return nil
}

// reactivateTenant transitions a suspended tenant back to active. An
// already-active tenant (or any non-suspended state) is left as-is;
// tenant.PGStore.Activate only matches suspended rows, so we swallow
// the ErrInvalidTransition that a non-suspended tenant produces.
func (s *Service) reactivateTenant(ctx context.Context, tenantID uuid.UUID) error {
	err := s.tenants.Activate(ctx, tenantID)
	if err == nil || errors.Is(err, tenant.ErrInvalidTransition) {
		return nil
	}
	return err
}

// SyncUsage reports the tenant's current-period metered usage to
// Stripe. Driven daily by the worker scheduled-action handler. It is
// a no-op (nil) when billing is disabled, the tenant has no
// subscription, or the subscription has no metered item — so the job
// is safe to register for every tenant regardless of plan.
func (s *Service) SyncUsage(ctx context.Context, tenantID uuid.UUID) error {
	if !s.cfg.Enabled() {
		return nil
	}
	sub, err := s.store.GetSubscriptionByTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNoSubscription) {
			return nil
		}
		return err
	}
	if sub.StripeSubscriptionItemID == "" {
		return nil
	}
	qty, err := s.usage.GetUsage(ctx, tenantID, s.usage.CurrentPeriod(), tenant.MetricAPICalls)
	if err != nil {
		return err
	}
	return s.stripe.RecordUsage(ctx, UsageParams{
		SubscriptionItemID: sub.StripeSubscriptionItemID,
		Quantity:           qty,
		Timestamp:          s.now(),
		Action:             "set",
	})
}

// EnforceTrialExpiry suspends a tenant whose trial (or past-due
// subscription) has been unpaid past the trial-grace window. Run
// alongside SyncUsage by the daily worker job. A tenant that is
// active/paid, or whose grace has not elapsed, is left untouched.
func (s *Service) EnforceTrialExpiry(ctx context.Context, tenantID uuid.UUID) error {
	sub, err := s.store.GetSubscriptionByTenant(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrNoSubscription) {
			return nil
		}
		return err
	}
	if !graceExpired(sub, s.now()) {
		return nil
	}
	if err := s.tenants.Suspend(ctx, tenantID); err != nil {
		// Suspend only matches active tenants; an already-suspended
		// tenant yields ErrInvalidTransition which is benign here.
		if errors.Is(err, tenant.ErrInvalidTransition) {
			return nil
		}
		return err
	}
	s.logger.InfoContext(ctx, "billing: suspended tenant after trial grace expiry",
		slog.String("tenant_id", tenantID.String()),
		slog.String("subscription_status", sub.Status))
	return nil
}

// graceExpired reports whether an unpaid subscription has run past its
// grace window and is due for suspension. It is a pure function of the
// subscription and the current time so the policy is unit-testable
// without a store.
//
// Only trialing/past-due subscriptions are candidates (an active sub is
// paid; a canceled one already downgraded to free). The grace anchor is
// chosen by status: a trialing sub is measured from the end of its
// trial, while a past-due sub (payment failed after a paid period —
// possibly with no trial at all) is measured from the end of its
// current billing period. Anchoring solely on TrialEnd would let a
// past-due, never-trialed subscription (TrialEnd == nil) evade
// suspension forever.
func graceExpired(sub *Subscription, now time.Time) bool {
	if sub.Status != SubStatusTrialing && sub.Status != SubStatusPastDue {
		return false
	}
	var anchor *time.Time
	if sub.Status == SubStatusTrialing {
		anchor = sub.TrialEnd
	} else { // SubStatusPastDue
		anchor = sub.CurrentPeriodEnd
		if anchor == nil {
			anchor = sub.TrialEnd
		}
	}
	if anchor == nil {
		// No anchor to measure grace from — can't safely suspend yet.
		return false
	}
	return !now.Before(anchor.Add(trialGrace))
}
