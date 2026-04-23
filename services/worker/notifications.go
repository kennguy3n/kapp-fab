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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/events"
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
	env := extractNotification(e.Payload)
	if env == nil {
		return
	}
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
		// SMTP adapter is deferred. Log so the event trail shows the
		// notification was observed and routed even when delivery
		// isn't wired yet.
		log.Printf("worker: email notify (stub) tenant=%s type=%s to=%q title=%q",
			e.TenantID, e.Type, env.Email, env.Title)
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
