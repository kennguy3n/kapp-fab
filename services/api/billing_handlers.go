package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/kennguy3n/kapp-fab/internal/billing"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// maxWebhookBody caps the Stripe webhook body we will read into
// memory before verifying its signature. Stripe events are a few KB;
// 1 MiB is a generous ceiling that still prevents an attacker from
// exhausting memory by streaming an unbounded body to the public
// (unauthenticated) webhook endpoint.
const maxWebhookBody = 1 << 20

// billingHandlers backs the /api/v1/billing/* surface. The
// subscribe / usage / portal-session routes are tenant-scoped (the
// tenant is taken from the JWT-stamped request context, never a body
// field, so a caller can only ever act on their own tenant). The
// webhook route is public — it is authenticated by the Stripe
// signature, not a Kapp JWT.
//
// metering + plans are referenced directly (rather than through the
// billing service) for the read-only usage endpoint so the response
// mirrors the existing control-plane /tenants/{id}/usage payload the
// UI already knows how to render, without widening the billing
// service's surface with metering concerns.
type billingHandlers struct {
	svc      *billing.Service
	metering *tenant.MeteringStore
	plans    *tenant.PlanStore
	logger   *slog.Logger
}

type subscribeRequest struct {
	Plan       string `json:"plan"`
	SuccessURL string `json:"success_url"`
	CancelURL  string `json:"cancel_url"`
}

// subscribe starts (or, for the free plan, immediately applies) a
// plan change for the calling tenant. For paid plans the response
// carries the Stripe Checkout URL the frontend redirects to; the
// plan only switches once Stripe confirms payment via webhook.
func (h *billingHandlers) subscribe(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req subscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Plan == "" {
		http.Error(w, "plan required", http.StatusBadRequest)
		return
	}
	res, err := h.svc.Subscribe(r.Context(), t.ID, req.Plan, req.SuccessURL, req.CancelURL)
	if err != nil {
		h.writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// portalSession opens a Stripe Billing Portal session so the tenant
// can manage their payment method / cancel from Stripe's hosted UI.
func (h *billingHandlers) portalSession(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	url, err := h.svc.PortalSession(r.Context(), t.ID)
	if err != nil {
		h.writeBillingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// billingUsageResponse is the tenant-scoped billing dashboard
// payload: the current subscription (may be nil for a tenant that
// never subscribed), the current-period metered counters rolled up
// against the plan's limits, and the invoice history.
type billingUsageResponse struct {
	Plan         string                `json:"plan"`
	Usage        map[string]int64      `json:"usage"`
	Limits       tenant.PlanLimits     `json:"limits"`
	Subscription *billing.Subscription `json:"subscription"`
	Invoices     []billing.Invoice     `json:"invoices"`
}

// usage returns the calling tenant's billing dashboard data.
func (h *billingHandlers) usage(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rows, err := h.metering.GetAllMetrics(r.Context(), t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	usage := map[string]int64{
		tenant.MetricAPICalls:     0,
		tenant.MetricStorageBytes: 0,
		tenant.MetricKRecordCount: 0,
		tenant.MetricUserSeats:    0,
	}
	for _, row := range rows {
		usage[row.Metric] = row.Value
	}

	limits := tenant.PlanLimits{}
	if plan, perr := h.plans.Get(r.Context(), t.Plan); perr == nil {
		limits = plan.Limits
	}

	// A tenant that has never subscribed has no row — that is a
	// normal state (free plan), not an error, so collapse
	// ErrNoSubscription to a nil subscription rather than a 404.
	var sub *billing.Subscription
	if s, serr := h.svc.GetSubscription(r.Context(), t.ID); serr == nil {
		sub = s
	} else if !errors.Is(serr, billing.ErrNoSubscription) {
		http.Error(w, serr.Error(), http.StatusInternalServerError)
		return
	}

	invoices, err := h.svc.ListInvoices(r.Context(), t.ID, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, billingUsageResponse{
		Plan:         t.Plan,
		Usage:        usage,
		Limits:       limits,
		Subscription: sub,
		Invoices:     invoices,
	})
}

// webhook is the public Stripe webhook sink. It is NOT behind the
// JWT gate: authenticity is established by the Stripe-Signature HMAC,
// which the service verifies before trusting a single byte of the
// body. The body is read under a size cap and the raw bytes (not a
// re-encoded form) are handed to the verifier because the HMAC is
// computed over the exact payload Stripe sent.
func (h *billingHandlers) webhook(w http.ResponseWriter, r *http.Request) {
	// Read one byte past the cap so an over-limit body is detected
	// explicitly: a silently truncated body would still fail the HMAC
	// (the signature is over the full payload), but it would surface as
	// an opaque "invalid signature" rather than the true cause.
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	if int64(len(payload)) > maxWebhookBody {
		http.Error(w, "webhook body too large", http.StatusRequestEntityTooLarge)
		return
	}
	sig := r.Header.Get("Stripe-Signature")
	if err := h.svc.HandleWebhook(r.Context(), payload, sig); err != nil {
		switch {
		case errors.Is(err, billing.ErrInvalidSignature):
			// Untrusted body — answer 400 without leaking detail.
			http.Error(w, "invalid signature", http.StatusBadRequest)
		case errors.Is(err, billing.ErrUnknownCustomer):
			// Event we can't attribute to a tenant. Acknowledge with
			// 200 so Stripe stops retrying — a retry will never
			// succeed — but log it for investigation.
			if h.logger != nil {
				h.logger.Warn("billing: webhook for unknown customer", slog.String("error", err.Error()))
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		default:
			// Transient/internal failure — 500 so Stripe retries.
			if h.logger != nil {
				h.logger.Error("billing: webhook processing failed", slog.String("error", err.Error()))
			}
			http.Error(w, "webhook processing failed", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeBillingError maps the billing package's sentinel errors onto
// HTTP status codes. Unknown errors collapse to 500.
func (h *billingHandlers) writeBillingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrBillingDisabled):
		http.Error(w, "billing is not configured", http.StatusServiceUnavailable)
	case errors.Is(err, billing.ErrPlanNotPayable):
		http.Error(w, "plan does not require payment", http.StatusBadRequest)
	case errors.Is(err, billing.ErrNoSubscription):
		http.Error(w, "no subscription for tenant", http.StatusNotFound)
	case errors.Is(err, tenant.ErrPlanNotFound):
		http.Error(w, "plan not found", http.StatusBadRequest)
	case errors.Is(err, tenant.ErrNotFound):
		http.Error(w, "tenant not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
