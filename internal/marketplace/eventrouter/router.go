// Package eventrouter is the B4 marketplace event fan-out engine.
//
// The router consumes outbox event batches (events.Event values
// drained from the per-tenant `events` partition by
// internal/events.PGPublisher.DrainBatch) and, for each event,
// looks up matching rows in marketplace_webhook_subscriptions
// (registered at install time by
// internal/marketplace/runtime/registrar.go for both
// `webhooks_consumed[]` and `posting_hooks[]` manifest entries).
// Each matching subscription is dispatched as a signed HTTPS
// POST via the shared B3 Transport + SignRequest infrastructure
// from internal/marketplace/runtime.
//
// B4 specifically does NOT duplicate B3's dispatch primitives —
// the audit log writer (writeDispatchLogStart /
// writeDispatchLogComplete), the HMAC signer (SignRequest), and
// the HTTP transport (Transport) are all reused. The only thing
// the router adds is:
//
//  1. The subscription-lookup query (event → list of installs
//     with their endpoints / signing secrets).
//  2. The per-subscription filter evaluator (filter.go).
//  3. The per-(tenant, extension) rate limiter (ratelimit.go).
//
// Dispatch failures for one subscription do NOT block others
// for the same event: each (event, subscription) pair is
// dispatched independently with its own retry budget. A slow or
// broken extension cannot stall the event bus for other
// extensions or for non-marketplace consumers (NATS / kchat-
// bridge run alongside the router in services/worker/main.go).
package eventrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
)

// Router is the B4 fan-out engine. Construct via NewRouter and
// invoke RouteBatch from the worker's deliver() callback.
type Router struct {
	pool      *pgxpool.Pool
	transport runtime.Transport
	encryptor runtime.Encryptor
	limiter   *Limiter
	now       func() time.Time

	// retry holds the per-dispatch retry policy. Event-
	// delivery dispatches are bounded at 2 attempts (1
	// initial + 1 retry on 5xx) — symmetric with the user's
	// B4 brief ("1 retry on 5xx"). The DB CHECK on
	// retry_max_attempts >= 1 is irrelevant here because the
	// router does not consult per-installation retry config;
	// every event delivery shares this single policy.
	retry *runtime.RetryPolicy

	// timeout is the per-attempt HTTP timeout for event
	// deliveries. 10s default matches the spec sketch in the
	// B4 brief; not configurable per subscription today.
	timeout time.Duration
}

// NewRouter constructs a Router. now=nil falls back to time.Now.
// limiter=nil disables rate limiting (the caller is expected to
// supply a non-nil limiter in production; nil is for tests that
// exercise the dispatch path without burning bucket state).
func NewRouter(
	pool *pgxpool.Pool,
	transport runtime.Transport,
	encryptor runtime.Encryptor,
	limiter *Limiter,
	now func() time.Time,
) *Router {
	if now == nil {
		now = time.Now
	}
	if encryptor == nil {
		// Mirror dispatcher.go: a nil encryptor in tests
		// means "plaintext on disk", which is correct for
		// fixtures that pre-date the encryption migration.
		encryptor = runtime.NoopEncryptor()
	}
	return &Router{
		pool:      pool,
		transport: transport,
		encryptor: encryptor,
		limiter:   limiter,
		now:       now,
		retry:     &runtime.RetryPolicy{MaxAttempts: 2, Backoff: "exponential"},
		timeout:   10 * time.Second,
	}
}

// RouteBatch is the worker's entry point. For each event in the
// batch, the router looks up matching subscriptions and fires
// signed dispatches. Per-subscription dispatch failures are logged
// but never bubble up — a slow or broken extension cannot starve
// its siblings. The dispatch_log table is the authoritative
// per-attempt record; the outbox events table only tracks "did
// this event reach the router".
//
// Returns the count of subscriptions actually dispatched (after
// filter + rate-limit), which the caller can observe for metrics.
//
// Error semantics for the caller (services/worker/main.go deliver()):
//
// Marketplace event delivery is **best-effort, side-effect-only**
// alongside NATS publish + kchat-bridge. A non-nil error here means
// subscription enumeration failed (typically a DB outage during the
// lookup tx) for at least one event in the batch. The worker
// LOGS the error and continues — it does NOT block the outbox row
// from being marked delivered, because doing so would make a
// transient marketplace DB issue stall every other event consumer
// (NATS, kchat-bridge) for the same drain batch. That trade-off is
// deliberate: extension event delivery has at-MOST-once semantics
// today; a follow-up (B6 / B7) can add a separate dead-letter table
// for failed marketplace routings if at-least-once is required.
func (r *Router) RouteBatch(ctx context.Context, batch []events.Event) (int, error) {
	if r == nil {
		return 0, errors.New("eventrouter: nil router")
	}
	if r.pool == nil {
		return 0, errors.New("eventrouter: nil pool")
	}
	if r.transport == nil {
		return 0, errors.New("eventrouter: nil transport")
	}
	dispatched := 0
	var errs []error
	for _, e := range batch {
		n, err := r.routeOne(ctx, e)
		if err != nil {
			// Subscription lookup failed (DB outage / RLS
			// hiccup) for this event. Per the doc-comment
			// above, marketplace event delivery is best-
			// effort: a single failing event must NOT
			// starve its siblings in the same batch. We
			// accumulate the error and continue so every
			// remaining event still gets its chance at
			// subscription enumeration + dispatch. The
			// caller (worker deliver()) logs the joined
			// error and acks the outbox row regardless.
			errs = append(errs, fmt.Errorf("eventrouter: tenant=%s event=%s: %w", e.TenantID, e.Type, err))
			continue
		}
		dispatched += n
	}
	if len(errs) > 0 {
		return dispatched, errors.Join(errs...)
	}
	return dispatched, nil
}

// subscription is the row shape pulled from
// marketplace_webhook_subscriptions joined onto
// marketplace_extension_installations (for the signing secret +
// status + extension/version id) and marketplace_extensions (for
// the rate_limit_rpm override).
type subscription struct {
	ID                 uuid.UUID
	InstallationID     uuid.UUID
	ExtensionID        uuid.UUID
	ExtensionVersionID uuid.UUID
	SigningSecret      string // ciphertext from disk; decrypt before use
	Endpoint           string
	Filter             json.RawMessage
	RateLimitRPM       int
}

// routeOne fans out one event to its matching subscriptions.
// Returns the number of subscriptions that completed dispatch
// (allowed by rate limit + filter + transport).
func (r *Router) routeOne(ctx context.Context, e events.Event) (int, error) {
	if e.TenantID == uuid.Nil || e.Type == "" {
		// Malformed outbox row — log and skip rather than
		// stalling the drain. The events table CHECK
		// constraints already reject these at INSERT, so
		// we should never observe one here in practice;
		// the guard is for defence against direct SQL.
		slog.Default().Warn("eventrouter: malformed outbox event",
			slog.String("tenant_id", e.TenantID.String()),
			slog.String("event_type", e.Type),
			slog.String("event_id", e.ID.String()),
		)
		return 0, nil
	}

	subs, err := r.lookupSubscriptions(ctx, e.TenantID, e.Type)
	if err != nil {
		return 0, err
	}
	if len(subs) == 0 {
		return 0, nil
	}

	dispatched := 0
	for i := range subs {
		sub := &subs[i]
		// 1. Filter eligibility: the subscription's filter
		//    must match the event payload (spec §7
		//    equality semantics — see filter.go).
		filterMap, err := decodeFilter(sub.Filter)
		if err != nil {
			slog.Default().Warn("eventrouter: malformed filter",
				slog.String("subscription_id", sub.ID.String()),
				slog.String("err", err.Error()),
			)
			continue
		}
		matched, err := filterMatches(filterMap, e.Payload)
		if err != nil {
			slog.Default().Warn("eventrouter: filter eval failed",
				slog.String("subscription_id", sub.ID.String()),
				slog.String("event_type", e.Type),
				slog.String("err", err.Error()),
			)
			continue
		}
		if !matched {
			continue
		}

		// 2. Rate-limit: per (tenant, extension). A
		//    rejected dispatch is dropped silently — the
		//    audit story is that no dispatch_log row is
		//    written, which mirrors how a never-dispatched
		//    subscription looks. A future iteration may
		//    write a "rate_limited" log row for forensic
		//    visibility; today we keep dispatch_log narrow.
		if r.limiter != nil {
			rpm := sub.RateLimitRPM
			if rpm < 1 {
				rpm = r.limiter.DefaultRPM()
			}
			if !r.limiter.Allow(e.TenantID, sub.ExtensionID, rpm) {
				slog.Default().Info("eventrouter: rate-limited",
					slog.String("tenant_id", e.TenantID.String()),
					slog.String("extension_id", sub.ExtensionID.String()),
					slog.String("event_type", e.Type),
					slog.Int("rpm", rpm),
				)
				continue
			}
		}

		// 3. Dispatch. Per-subscription failures are
		//    isolated — slog.Warn but keep iterating so a
		//    slow/broken extension doesn't starve its
		//    siblings.
		if err := r.dispatchOne(ctx, e, sub); err != nil {
			slog.Default().Warn("eventrouter: dispatch failed",
				slog.String("subscription_id", sub.ID.String()),
				slog.String("extension_id", sub.ExtensionID.String()),
				slog.String("event_type", e.Type),
				slog.String("err", err.Error()),
			)
			continue
		}
		dispatched++
	}
	return dispatched, nil
}

// lookupSubscriptions reads every active subscription that
// matches `(tenant, eventType)`. The join onto
// marketplace_extension_installations filters to active
// installs only (uninstalled / failed / disabled installs do
// not receive event deliveries — the subscription row is kept
// for audit but dispatch is silently skipped). marketplace_
// extensions supplies the rate_limit_rpm column.
//
// The query runs inside dbutil.WithTenantTx so RLS gates the
// subscription read alongside the explicit `WHERE tenant_id =
// $1` predicate (defence-in-depth — same pattern PR #128
// established for ListInstallationsForTenant).
func (r *Router) lookupSubscriptions(ctx context.Context, tenantID uuid.UUID, eventType string) ([]subscription, error) {
	var out []subscription
	err := dbutil.WithTenantTx(ctx, r.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT s.id,
			       s.installation_id,
			       i.extension_id,
			       i.extension_version_id,
			       i.signing_secret,
			       s.endpoint,
			       s.filter,
			       e.rate_limit_rpm
			  FROM marketplace_webhook_subscriptions AS s
			  JOIN marketplace_extension_installations AS i
			    ON i.tenant_id = s.tenant_id
			   AND i.id = s.installation_id
			  JOIN marketplace_extensions AS e
			    ON e.id = i.extension_id
			 WHERE s.tenant_id = $1
			   AND s.event = $2
			   AND i.status = 'active'`,
			tenantID, eventType,
		)
		if err != nil {
			return fmt.Errorf("query subscriptions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sub subscription
			if err := rows.Scan(
				&sub.ID,
				&sub.InstallationID,
				&sub.ExtensionID,
				&sub.ExtensionVersionID,
				&sub.SigningSecret,
				&sub.Endpoint,
				&sub.Filter,
				&sub.RateLimitRPM,
			); err != nil {
				return fmt.Errorf("scan subscription: %w", err)
			}
			out = append(out, sub)
		}
		return rows.Err()
	})
	return out, err
}

// dispatchOne signs and POSTs the event payload to the
// subscription's endpoint, writing per-attempt rows into
// marketplace_dispatch_log. Mirrors the structure of
// transportHooks.Dispatch (internal/marketplace/runtime/hooks.go)
// — open log row → send → close log row → classify response,
// with the simplified 2-attempt retry policy from r.retry.
func (r *Router) dispatchOne(ctx context.Context, e events.Event, sub *subscription) error {
	plain, err := r.encryptor.DecryptString(e.TenantID, sub.SigningSecret)
	if err != nil {
		return fmt.Errorf("decrypt signing secret: %w", err)
	}
	secret := runtime.SigningSecret(plain)

	body, err := json.Marshal(map[string]any{
		"event_id":     e.ID.String(),
		"event_type":   e.Type,
		"tenant_id":    e.TenantID.String(),
		"created_at":   e.CreatedAt.UTC().Format(time.RFC3339Nano),
		"installation": sub.InstallationID.String(),
		"payload":      json.RawMessage(payloadOrEmpty(e.Payload)),
	})
	if err != nil {
		return fmt.Errorf("marshal event delivery body: %w", err)
	}

	requestID := uuid.New()
	bodyHash := runtime.BodyHashHex(body)

	dispatchReq := &runtime.DispatchRequest{
		TenantID:           e.TenantID,
		InstallationID:     sub.InstallationID,
		ExtensionID:        sub.ExtensionID,
		ExtensionVersionID: sub.ExtensionVersionID,
		Kind:               runtime.KindEventDelivery,
		URL:                sub.Endpoint,
		Body:               body,
		Timeout:            r.timeout,
		Retry:              r.retry,
		SigningSecret:      secret,
		RequestID:          requestID,
	}

	for attempt := 1; attempt <= r.retry.MaxAttempts; attempt++ {
		if delay := r.retry.BackoffDelay(attempt); delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		ts := r.now()
		headers, _, signErr := runtime.SignRequest(dispatchReq, ts)
		if signErr != nil {
			return fmt.Errorf("sign: %w", signErr)
		}

		logRowID, logErr := runtime.WriteDispatchLogStart(ctx, r.pool, runtime.DispatchLogStart{
			TenantID:           e.TenantID,
			InstallationID:     sub.InstallationID,
			ExtensionID:        sub.ExtensionID,
			ExtensionVersionID: sub.ExtensionVersionID,
			Kind:               runtime.KindEventDelivery,
			Endpoint:           sub.Endpoint,
			RequestID:          requestID,
			Attempt:            attempt,
			BodySHA256:         bodyHash,
			Signature:          headers[runtime.SignatureHeaderName],
			StartedAt:          ts,
		})
		if logErr != nil {
			// Mirror lifecycle hooks: a missing audit row
			// is degraded-mode, not fatal. Event delivery
			// is fire-and-forget so completing the
			// dispatch is more important than the audit
			// trail; the operator-visible outcome (event
			// delivered to extension) is preserved.
			slog.Default().Warn("eventrouter: write dispatch log start",
				slog.String("subscription_id", sub.ID.String()),
				slog.String("err", logErr.Error()),
			)
		}

		started := time.Now()
		resp, sendErr := r.transport.Send(ctx, sub.Endpoint, body, headers, r.timeout)
		latency := time.Since(started)

		if sendErr != nil {
			if logRowID != uuid.Nil {
				if logErr := runtime.WriteDispatchLogComplete(ctx, r.pool, dispatchReq.TenantID, logRowID, 0, latency, sendErr); logErr != nil {
					slog.Default().Warn("eventrouter: write dispatch log complete (transport err)",
						slog.String("subscription_id", sub.ID.String()),
						slog.String("err", logErr.Error()),
					)
				}
			}
			if !runtime.IsRetryableTransportError(sendErr) || attempt == r.retry.MaxAttempts {
				return sendErr
			}
			continue
		}

		if logRowID != uuid.Nil {
			if logErr := runtime.WriteDispatchLogComplete(ctx, r.pool, dispatchReq.TenantID, logRowID, resp.Status, latency, nil); logErr != nil {
				slog.Default().Warn("eventrouter: write dispatch log complete",
					slog.String("subscription_id", sub.ID.String()),
					slog.String("err", logErr.Error()),
				)
			}
		}

		// 2xx terminal success.
		if resp.Status >= 200 && resp.Status < 300 {
			return nil
		}
		// 4xx terminal failure — surface as error so the
		// router's caller can log a dispatch_failed metric.
		// We do NOT retry 4xx because the response indicates
		// the extension actively rejected the payload (likely
		// schema mismatch / auth misconfig) and another
		// identical attempt will fail identically.
		if resp.Status < 500 && resp.Status != 408 {
			return fmt.Errorf("subscription %s returned %d", sub.ID, resp.Status)
		}
		// 5xx + 408 are retryable.
		if attempt == r.retry.MaxAttempts {
			return fmt.Errorf("subscription %s exhausted retries with status %d", sub.ID, resp.Status)
		}
	}
	// Unreachable: every iteration either returns or continues
	// past the bottom of the loop; the loop bound is
	// retry.MaxAttempts and we explicitly return on attempt ==
	// MaxAttempts in both transport-error and 5xx branches.
	return errors.New("eventrouter: unreachable retry-loop exit")
}

// decodeFilter parses the JSONB filter column. Empty / nil → nil
// map (matches every payload — see filterMatches).
func decodeFilter(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// payloadOrEmpty returns "{}" for nil/empty payloads so the
// outer marshal produces valid JSON inside the `payload` key.
func payloadOrEmpty(p []byte) []byte {
	if len(p) == 0 {
		return []byte("{}")
	}
	return p
}
