package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/notifications"
)

// notificationRouter fans outbox events out to per-notification
// channels (KChat DM, webhook, email). It reads a `notification`
// envelope on the event payload and invokes the matching channel:
//
//	{
//	  "notification": {
//	    "channel":     "kchat" | "webhook" | "email",
//	    "title":       "optional human-readable title",
//	    "body":        "optional body text",
//	    "webhook_url": "https://… (required when channel=webhook)",
//	    "email":       "user@example.com (required when channel=email)"
//	  },
//	  …
//	}
//
// The in-app channel is served directly by services/api's SSE
// endpoint off the events table, so there is nothing to do for it
// here. Email is stubbed out — the worker logs it until an SMTP
// adapter lands. Webhook POSTs the full event envelope to the URL.
type notificationRouter struct {
	bridge *kchatBridgeNotifier
	client *http.Client
	// pool is used to look up the per-tenant webhook signing secret
	// (tenants.quota->>'webhook_secret'). The tenants table is
	// control-plane, not tenant-scoped, so the app pool is fine — no
	// SET LOCAL app.tenant_id required.
	pool *pgxpool.Pool
	// adminPool bypasses RLS for cross-tenant scans (e.g. the
	// registered-webhook fan-out has to read every tenant's rows at
	// once). The app pool cannot do this because RLS pins the query
	// to the tenant set in app.tenant_id.
	adminPool *pgxpool.Pool
	// store persists every notification envelope to the inbox table
	// so the web bell/inbox surface is independent of transport
	// success. Nil is tolerated for dev setups without the schema.
	store *notifications.Store
	// smtp is the optional outbound mail adapter. Nil (or a zero
	// SMTPConfig) means SMTP is not configured and the email channel
	// falls back to logging the notice.
	smtp notifications.SMTPSender
	// webhookStore persists per-tenant outbound webhook subscriptions
	// and their delivery log so operators can audit failed POSTs.
	webhookStore *notifications.WebhookStore
}

// notificationEnvelope is the shape extracted from event payloads.
// Every field is optional so a producer can emit a partial envelope
// for the channels they care about and the router will skip the rest.
type notificationEnvelope struct {
	Channel    string `json:"channel"`
	Title      string `json:"title,omitempty"`
	Body       string `json:"body,omitempty"`
	WebhookURL string `json:"webhook_url,omitempty"`
	Email      string `json:"email,omitempty"`
	UserID     string `json:"user_id,omitempty"`
}

// route dispatches the event to the channels named in its payload.
// All failures are logged — delivery is best-effort; the durable
// source of truth is the events row itself plus the NATS subject
// the deliver loop already published.
func (r *notificationRouter) route(ctx context.Context, e events.Event) {
	if r == nil {
		return
	}
	// Fan out to tenant-registered webhook subscriptions before the
	// envelope gate — a producer might emit a pure event (no
	// notification envelope) and a tenant can still subscribe
	// external systems via the /api/v1/webhooks CRUD surface.
	r.fanOutRegisteredWebhooks(ctx, e)
	env := extractNotification(e.Payload)
	if env == nil {
		return
	}
	// Persist first so the inbox surface does not depend on transport
	// success: even if KChat / webhook / SMTP all fail, the user will
	// still see the notice in the web bell dropdown. Failures here are
	// logged and the transport delivery still proceeds.
	r.persistNotification(ctx, e, *env)
	switch env.Channel {
	case "kchat":
		if r.bridge != nil && r.bridge.enabled() {
			if err := r.postKChatNotice(ctx, e, *env); err != nil {
				log.Printf("worker: kchat notify %s: %v", e.Type, err)
			}
		}
	case "webhook":
		if env.WebhookURL == "" {
			return
		}
		if err := r.postWebhook(ctx, e, env.WebhookURL); err != nil {
			log.Printf("worker: webhook %s → %s: %v", e.Type, env.WebhookURL, err)
		}
	case "email":
		if env.Email == "" {
			return
		}
		if err := r.sendEmail(ctx, e, *env); err != nil {
			log.Printf("worker: email notify tenant=%s type=%s to=%q: %v",
				e.TenantID, e.Type, env.Email, err)
		}
	default:
		// Unknown / unset channel — ignore. Producers that only want
		// in-app SSE emit events with no notification envelope.
	}
}

// postKChatNotice delegates to the bridge's generic notify endpoint.
// The bridge owns the card renderer for user-facing notices; the
// worker just hands it the tenant + payload.
func (r *notificationRouter) postKChatNotice(
	ctx context.Context,
	e events.Event,
	env notificationEnvelope,
) error {
	body, err := json.Marshal(map[string]any{
		"tenant_id": e.TenantID,
		"type":      e.Type,
		"title":     env.Title,
		"body":      env.Body,
		"user_id":   env.UserID,
		"payload":   json.RawMessage(e.Payload),
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := r.bridge.baseURL + "/kchat/notifications/render"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(snippet))
	}
	return nil
}

// postWebhook POSTs the full event envelope to the configured URL so
// an external consumer receives the same {id, tenant_id, type,
// payload, created_at} shape the outbox stores. The body is signed
// with HMAC-SHA256 using the per-tenant webhook secret
// (tenants.quota->>'webhook_secret'). Consumers verify by recomputing
// the same HMAC and comparing against the X-Kapp-Signature header. If
// no secret is configured for the tenant, the signature header is
// omitted — receivers should refuse unsigned deliveries in production.
func (r *notificationRouter) postWebhook(ctx context.Context, e events.Event, url string) error {
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kapp-Event-Type", e.Type)
	req.Header.Set("X-Kapp-Event-Id", e.ID.String())
	req.Header.Set("X-Kapp-Tenant-Id", e.TenantID.String())
	if sig, err := r.signWebhookBody(ctx, e.TenantID, body); err != nil {
		// Secret lookup failures are logged and the delivery proceeds
		// unsigned; we prefer losing integrity over dropping events.
		log.Printf("worker: webhook sign tenant=%s: %v", e.TenantID, err)
	} else if sig != "" {
		req.Header.Set("X-Kapp-Signature", "sha256="+sig)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(snippet))
	}
	return nil
}

// sendEmail dispatches the notification envelope through the SMTP
// adapter. When no adapter is configured (or the adapter returns
// ErrSMTPDisabled) we degrade gracefully to a log line so the event
// trail still shows the notice was observed.
func (r *notificationRouter) sendEmail(ctx context.Context, e events.Event, env notificationEnvelope) error {
	if r.smtp == nil {
		log.Printf("worker: email notify (no smtp) tenant=%s type=%s to=%q title=%q",
			e.TenantID, e.Type, env.Email, env.Title)
		return nil
	}
	subject := env.Title
	if subject == "" {
		subject = e.Type
	}
	body := env.Body
	if body == "" {
		body = string(e.Payload)
	}
	err := r.smtp.Send(ctx, []string{env.Email}, subject, body)
	if errors.Is(err, notifications.ErrSMTPDisabled) {
		log.Printf("worker: email notify (smtp disabled) tenant=%s type=%s to=%q", e.TenantID, e.Type, env.Email)
		return nil
	}
	return err
}

// persistNotification writes an inbox row for the supplied event so
// the in-app bell/inbox surface is transport-independent. Failures
// are logged; the outbox row has already been published to NATS and
// the durable event log is the source of truth.
func (r *notificationRouter) persistNotification(ctx context.Context, e events.Event, env notificationEnvelope) {
	if r.store == nil {
		return
	}
	var uid *uuid.UUID
	if env.UserID != "" {
		if parsed, err := uuid.Parse(env.UserID); err == nil {
			uid = &parsed
		}
	}
	in := notifications.CreateInput{
		TenantID: e.TenantID,
		UserID:   uid,
		Type:     e.Type,
		Title:    env.Title,
		Body:     env.Body,
		Payload:  e.Payload,
	}
	if _, err := r.store.Create(ctx, in); err != nil {
		log.Printf("worker: persist notification tenant=%s type=%s: %v", e.TenantID, e.Type, err)
	}
}

// signWebhookBody computes the HMAC-SHA256 of body using the tenant's
// webhook secret and returns its hex digest. The secret is read from
// `tenants.quota->>'webhook_secret'`. Returns ("", nil) when no
// secret is configured — caller skips the signature header.
func (r *notificationRouter) signWebhookBody(ctx context.Context, tenantID uuid.UUID, body []byte) (string, error) {
	if r.pool == nil {
		return "", nil
	}
	var secret *string
	err := r.pool.QueryRow(ctx,
		`SELECT quota->>'webhook_secret' FROM tenants WHERE id = $1`,
		tenantID,
	).Scan(&secret)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("load webhook secret: %w", err)
	}
	if secret == nil || *secret == "" {
		return "", nil
	}
	mac := hmac.New(sha256.New, []byte(*secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// extractNotification pulls the `notification` envelope out of the
// event payload. Returns nil if the payload is missing / malformed /
// has no notification field so the router cheaply skips pure-event
// rows that never need external delivery.
func extractNotification(raw []byte) *notificationEnvelope {
	if len(raw) == 0 {
		return nil
	}
	var wrapper struct {
		Notification *notificationEnvelope `json:"notification"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Notification == nil {
		return nil
	}
	if wrapper.Notification.Channel == "" {
		return nil
	}
	return wrapper.Notification
}

// maxWebhookAttempts is the hard retry ceiling. Matches the task
// requirement (max 5 attempts) and bounds the exponential backoff
// schedule in webhookBackoff.
const maxWebhookAttempts = 5

// webhookRetryBatch is the upper bound on the number of pending
// retries the polling loop will claim per tick. Keeps one slow
// endpoint from starving the rest when many rows come due together.
const webhookRetryBatch = 50

// fanOutRegisteredWebhooks looks up every active webhook for the
// event's tenant, filters by event-type prefix, and attempts one
// POST per matching hook. The call is non-blocking in the sense
// that no retry sleep happens on the drain goroutine: failed
// attempts are logged to webhook_deliveries with a next_retry_at,
// and a dedicated polling loop (runWebhookRetryLoop) later picks
// those rows up. This keeps the outbox drain moving even when a
// customer's endpoint is down.
func (r *notificationRouter) fanOutRegisteredWebhooks(ctx context.Context, e events.Event) {
	if r == nil || r.webhookStore == nil || r.adminPool == nil {
		return
	}
	hooks, err := r.webhookStore.ListActiveAcrossTenants(ctx, r.adminPool)
	if err != nil {
		log.Printf("worker: list active webhooks: %v", err)
		return
	}
	for _, h := range hooks {
		if h.TenantID != e.TenantID {
			continue
		}
		if !webhookMatches(h, e.Type) {
			continue
		}
		r.deliverWebhook(ctx, h, e, 1)
	}
}

// runWebhookRetryLoop polls webhook_deliveries for rows whose
// next_retry_at has elapsed, re-assembles the original event from
// the events table, and re-posts via deliverWebhook. Each claimed
// row has its next_retry_at cleared atomically so a second worker
// (or the same worker on the next tick) never double-delivers. The
// loop survives worker restarts — pending retries sit in the table
// until claimed, so a crash between the first failure and the retry
// simply delays the retry by one tick rather than dropping it.
func (r *notificationRouter) runWebhookRetryLoop(ctx context.Context, interval time.Duration) {
	if r == nil || r.webhookStore == nil || r.adminPool == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.processPendingWebhookRetries(ctx)
		}
	}
}

// processPendingWebhookRetries does one tick of the retry loop.
// Extracted so tests can drive the logic without a ticker.
func (r *notificationRouter) processPendingWebhookRetries(ctx context.Context) {
	pending, err := r.webhookStore.ClaimPendingRetries(ctx, r.adminPool, maxWebhookAttempts, webhookRetryBatch)
	if err != nil {
		log.Printf("worker: claim pending webhook retries: %v", err)
		return
	}
	for _, p := range pending {
		hook, err := r.webhookStore.GetAdmin(ctx, r.adminPool, p.TenantID, p.WebhookID)
		if err != nil {
			log.Printf("worker: load hook tenant=%s hook=%s: %v", p.TenantID, p.WebhookID, err)
			continue
		}
		if !hook.Active {
			continue
		}
		stored, err := r.webhookStore.GetEvent(ctx, r.adminPool, p.TenantID, p.EventID)
		if err != nil {
			log.Printf("worker: load event tenant=%s event=%s: %v", p.TenantID, p.EventID, err)
			continue
		}
		ev := events.Event{
			ID:       stored.ID,
			TenantID: stored.TenantID,
			Type:     stored.Type,
			Payload:  stored.Payload,
		}
		r.deliverWebhook(ctx, *hook, ev, p.Attempt+1)
	}
}

// webhookMatches returns true when the subscription's event_filters
// array either contains the event type literally or a prefix of it
// (ending with "*"). An empty filter list means "all events".
func webhookMatches(h notifications.Webhook, eventType string) bool {
	var filters []string
	if len(h.EventFilters) == 0 {
		return true
	}
	if err := json.Unmarshal(h.EventFilters, &filters); err != nil {
		return true
	}
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f == "" {
			continue
		}
		if f == eventType {
			return true
		}
		if len(f) > 1 && f[len(f)-1] == '*' {
			prefix := f[:len(f)-1]
			if len(eventType) >= len(prefix) && eventType[:len(prefix)] == prefix {
				return true
			}
		}
	}
	return false
}

// deliverWebhook POSTs one attempt and logs the outcome to the
// delivery log. A failed attempt (transport error or non-2xx) is
// persisted with a next_retry_at computed from webhookBackoff so
// the retry polling loop picks it up later — this function never
// sleeps and never recurses. That keeps the outbox drain goroutine
// off the retry-latency critical path: a slow customer endpoint
// can delay at most one POST worth of latency per drain, not the
// full exponential-backoff window.
func (r *notificationRouter) deliverWebhook(ctx context.Context, h notifications.Webhook, e events.Event, attempt int) {
	body, err := json.Marshal(e)
	if err != nil {
		r.recordWebhookAttempt(ctx, h, e, attempt, nil, "", false, fmt.Errorf("marshal: %w", err), nil)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		r.recordWebhookAttempt(ctx, h, e, attempt, nil, "", false, err, nil)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kapp-Event-Type", e.Type)
	req.Header.Set("X-Kapp-Event-Id", e.ID.String())
	req.Header.Set("X-Kapp-Tenant-Id", e.TenantID.String())
	if h.Secret != "" {
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		req.Header.Set("X-Kapp-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.recordWebhookAttempt(ctx, h, e, attempt, nil, "", false, err, scheduleNextRetry(attempt))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	status := resp.StatusCode
	if status >= 200 && status < 300 {
		r.recordWebhookAttempt(ctx, h, e, attempt, &status, string(snippet), true, nil, nil)
		return
	}
	r.recordWebhookAttempt(ctx, h, e, attempt, &status, string(snippet), false, fmt.Errorf("non-2xx"), scheduleNextRetry(attempt))
}

// scheduleNextRetry returns a pointer to the wall-clock time the
// next attempt should run at, or nil when the attempt ceiling is
// reached and the row should stay terminal (delivered=false with
// no next_retry_at).
func scheduleNextRetry(attempt int) *time.Time {
	if attempt >= maxWebhookAttempts {
		return nil
	}
	t := time.Now().UTC().Add(webhookBackoff(attempt))
	return &t
}

// recordWebhookAttempt is a thin wrapper over webhookStore.RecordDelivery
// that swallows log errors so a failed delivery-log write does not
// short-circuit the retry loop.
func (r *notificationRouter) recordWebhookAttempt(
	ctx context.Context,
	h notifications.Webhook,
	e events.Event,
	attempt int,
	status *int,
	body string,
	delivered bool,
	err error,
	next *time.Time,
) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_, recErr := r.webhookStore.RecordDelivery(ctx, h.TenantID, notifications.DeliveryInput{
		WebhookID:    h.ID,
		EventID:      e.ID,
		EventType:    e.Type,
		StatusCode:   status,
		ResponseBody: body,
		Attempt:      attempt,
		Delivered:    delivered,
		Error:        msg,
		NextRetryAt:  next,
	})
	if recErr != nil {
		log.Printf("worker: record webhook delivery tenant=%s hook=%s: %v", h.TenantID, h.ID, recErr)
	}
}

// webhookBackoff returns the wait before attempt N+1 given the just-
// failed attempt. 1 → 1s, 2 → 2s, 3 → 4s, 4 → 8s, 5 → 16s.
func webhookBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Second
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	return d
}
