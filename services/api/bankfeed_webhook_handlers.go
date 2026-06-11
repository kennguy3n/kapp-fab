package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/ledger/bankfeed"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// bankfeedWebhookHandlers backs POST
// /api/v1/finance/bank-feeds/webhooks/{provider}: the near-real-time
// counterpart to the hourly scheduled sync. A provider (Plaid, GoCardless)
// posts a notification here when a connection has new data; the handler
// verifies the provider's signature, resolves the affected connection and
// runs the same SyncOne pipeline the scheduler drives.
//
// Unlike the rest of the bank-feeds subtree this route runs OUTSIDE the
// tenant chain: an inbound provider notification carries no Kapp session
// and no tenant header, so it cannot pass tenantChain/authz. Authentication
// is therefore the per-provider HMAC signature (verified by the provider's
// WebhookVerifier) and the resolution of the notification's external id to
// a stored connection — never a forge-able tenant header. The route is
// IP-rate-limited (publicWebhookIPLimit) so an unverified flood cannot
// amplify into provider calls.
type bankfeedWebhookHandlers struct {
	registry *bankfeed.Registry
	conns    *bankfeed.ConnectionStore
	sync     *bankfeed.SyncHandler
}

// maxWebhookBytes caps the notification body. Provider webhooks are tiny
// JSON envelopes (an item id + event type); 1 MiB is orders of magnitude
// of headroom and stops a hostile sender from streaming an unbounded body
// into memory before the signature is even checked.
const maxWebhookBytes = 1 << 20

// webhookSyncTimeout bounds the detached sync triggered by a verified
// webhook so a slow provider fetch cannot pin a goroutine indefinitely.
const webhookSyncTimeout = 2 * time.Minute

// receive verifies an inbound provider webhook and, when it signals new
// data, triggers a sync of the affected connection(s). It always responds
// quickly: the (potentially slow) provider fetch + ingest runs in a
// detached goroutine so the provider's webhook delivery is acknowledged
// well within its retry timeout. The response body is deliberately
// content-free — the caller is a provider, not a tenant, and must learn
// nothing about which connections exist beyond the signature it already
// holds.
func (h *bankfeedWebhookHandlers) receive(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.registry == nil || h.conns == nil || h.sync == nil {
		http.Error(w, "bank-feed webhook ingress not wired", http.StatusServiceUnavailable)
		return
	}
	providerName := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	log := platform.LoggerFromContext(r.Context())

	provider, err := h.registry.Get(providerName)
	if err != nil {
		// Unknown / unconfigured provider. Provider identity is not a
		// secret (the in-tenant /providers route advertises it), so a 404
		// is the honest answer and leaks nothing sensitive.
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}
	verifier, ok := provider.(bankfeed.WebhookVerifier)
	if !ok {
		// A configured provider with no push channel (CSV). 404 so a
		// misdirected notification is not mistaken for an outage.
		http.Error(w, "provider does not support webhooks", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}

	event, err := verifier.VerifyAndParse(body, r.Header)
	if err != nil {
		switch {
		case errors.Is(err, bankfeed.ErrWebhookSignature):
			// Forged or misconfigured-secret notification. 401 and never
			// any sync — a payload we cannot authenticate must not drive a
			// provider call.
			http.Error(w, "signature verification failed", http.StatusUnauthorized)
		default:
			// Verified-but-unparseable (or a provider quirk). The body may
			// echo provider internals, so do not surface its detail.
			log.Warn("bankfeed: webhook parse", "provider", providerName, "error", err)
			http.Error(w, "could not parse webhook", http.StatusBadRequest)
		}
		return
	}

	// A verified event that does not warrant a sync (an informational /
	// error notification) is acknowledged with no work so the provider
	// stops retrying.
	if !event.TriggerSync || event.ExternalID == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	conns, err := h.conns.ResolveActiveByExternalID(r.Context(), providerName, event.ExternalID)
	if err != nil {
		if errors.Is(err, bankfeed.ErrNotFound) {
			// Verified, but no active connection matches the handle (already
			// disconnected, or never ours). Ack so the provider stops
			// retrying; nothing to sync.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// A missing admin pool / non-bypass role is an operational
		// misconfiguration, not a client error: 503 so the provider retries
		// once the deployment is fixed.
		log.Error("bankfeed: webhook resolve connection", "provider", providerName, "error", err)
		http.Error(w, "could not resolve connection", http.StatusServiceUnavailable)
		return
	}

	// Acknowledge first, sync after: detach from the request context so the
	// provider fetch + ingest survives the response being written, bounded
	// by webhookSyncTimeout. SyncOne sets its own per-tenant RLS context
	// inside WithTenantTx, so the detached context needs no tenant value.
	toSync := append([]bankfeed.Connection(nil), conns...)
	go h.runSync(context.WithoutCancel(r.Context()), providerName, event.Kind, toSync)

	w.WriteHeader(http.StatusAccepted)
}

// runSync triggers SyncOne for each resolved connection in the background.
// Errors are logged (sanitized — SyncOne already keeps credentials out of
// its errors) and never block siblings: one connection's failure must not
// strand the others sharing a GoCardless requisition.
func (h *bankfeedWebhookHandlers) runSync(ctx context.Context, providerName, kind string, conns []bankfeed.Connection) {
	ctx, cancel := context.WithTimeout(ctx, webhookSyncTimeout)
	defer cancel()
	log := platform.LoggerFromContext(ctx)
	for i := range conns {
		conn := conns[i]
		if _, err := h.sync.SyncOne(ctx, conn.TenantID, &conn); err != nil {
			log.Error("bankfeed: webhook-triggered sync",
				"provider", providerName, "kind", kind,
				"tenant", conn.TenantID, "connection", conn.ID, "error", err)
		}
	}
}
