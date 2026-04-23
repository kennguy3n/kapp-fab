package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

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
// payload, created_at} shape the outbox stores.
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
