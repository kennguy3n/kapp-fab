// Command worker is the Kapp async worker process. It drains the event
// outbox and publishes messages to NATS. Later phases add workflow timer
// advancement, retries, and background job handlers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

const (
	tickInterval = 2 * time.Second
	drainBatch   = 100
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func run() error {
	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := platform.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Admin pool (BYPASSRLS) is optional but required for control-plane
	// scans that span tenants — notably the low-stock sweep, which
	// otherwise returns zero rows because the shared kapp_app session
	// has no app.tenant_id set and RLS default-denies.
	var adminPool *pgxpool.Pool
	if cfg.AdminDatabaseURL != "" {
		adminPool, err = platform.NewPool(ctx, cfg.AdminDatabaseURL)
		if err != nil {
			return fmt.Errorf("connect admin pool: %w", err)
		}
		defer adminPool.Close()
	}

	natsURL := cfg.EventBusURL
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	nc, err := nats.Connect(natsURL,
		nats.Name("kapp-worker"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer func() {
		if err := nc.Drain(); err != nil {
			log.Printf("worker: nats drain: %v", err)
		}
	}()

	publisher := events.NewPGPublisher(pool)

	// kchat-bridge base URL drives the approval-card notification path.
	// When set, the worker POSTs {tenant_id, approval_id} to
	// <bridge>/kchat/approvals/render for every approval lifecycle
	// event drained from the outbox so the reviewer / approver gets a
	// DM card in KChat. Empty disables the notification (useful for
	// local dev without a bridge) — the event is still published to
	// NATS for the general event-bus consumers.
	bridge := &kchatBridgeNotifier{
		baseURL: strings.TrimRight(os.Getenv("KAPP_KCHAT_BRIDGE_URL"), "/"),
		client:  &http.Client{Timeout: 5 * time.Second},
	}

	// Phase F notification router. The worker is the single point that
	// fans an outbox event out to the per-tenant notification channels:
	// KChat DMs via the bridge, in-app SSE is served directly from the
	// events table by services/api, and email + webhook are invoked
	// here when the event payload carries a `notification` envelope.
	// Email is logged as a stub until an SMTP adapter lands; webhook
	// POSTs the raw event envelope to `notification.webhook_url`.
	smtpCfg := notifications.SMTPConfig{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		User:     cfg.SMTPUser,
		Password: cfg.SMTPPassword,
		From:     cfg.SMTPFrom,
	}
	router := &notificationRouter{
		bridge: bridge,
		client: &http.Client{Timeout: 5 * time.Second},
		pool:   pool,
		store:  notifications.NewStore(pool),
		smtp:   notifications.NewSMTPAdapter(smtpCfg),
	}

	// Low-stock alert sweeper runs alongside the outbox drain so a
	// below-threshold SKU produces a KChat alert within one sweep
	// interval. The sweeper shares the outbox publisher, so emitted
	// alerts go through the same delivery / dedupe pipeline as any
	// other `inventory.*` event.
	alerts := newStockAlertWorker(pool, adminPool, publisher, dbutil.SetTenantContext)
	go alerts.Run(ctx)

	log.Printf("worker: started; draining every %s; nats=%s; kchat-bridge=%q", tickInterval, natsURL, bridge.baseURL)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("worker: shutdown signal received")
			return nil
		case <-ticker.C:
			if _, err := publisher.DrainBatch(ctx, drainBatch, deliver(nc, bridge, router)); err != nil {
				log.Printf("worker: drain batch: %v", err)
			}
		}
	}
}

func deliver(nc *nats.Conn, bridge *kchatBridgeNotifier, router *notificationRouter) func(ctx context.Context, batch []events.Event) error {
	return func(ctx context.Context, batch []events.Event) error {
		for _, e := range batch {
			subject := fmt.Sprintf("kapp.events.%s", e.Type)
			payload, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("marshal event %s: %w", e.ID, err)
			}
			if err := nc.Publish(subject, payload); err != nil {
				return fmt.Errorf("publish %s: %w", subject, err)
			}
			// Fan out approval lifecycle events to kchat-bridge so the
			// reviewer / approver receives the DM card. Render failures
			// are logged but do not fail the drain — the event is
			// already durably on NATS and the outbox row will be marked
			// delivered so it will not retry. Phase E treats the card
			// notification as best-effort; the approval itself lives in
			// Postgres and is visible via the Approvals page.
			if bridge.enabled() && isApprovalNotificationEvent(e.Type) {
				if err := bridge.renderApprovalCard(ctx, e); err != nil {
					log.Printf("worker: kchat render %s: %v", e.Type, err)
				}
			}
			// Phase F: route generic notification events to the per-
			// tenant configured channels (KChat DM, webhook, email).
			// Failures are logged — the NATS publish already succeeded
			// and the in-app SSE tail is served directly from the
			// events table, so a failed sidecar delivery never blocks
			// the outbox drain.
			if router != nil {
				router.route(ctx, e)
			}
		}
		return nc.Flush()
	}
}

// kchatBridgeNotifier is the minimal HTTP client the worker uses to ask
// kchat-bridge to render an approval card for a given {tenant, approval}
// pair. The bridge already owns the renderer + KType card templates;
// the worker just tells it which approval to hydrate.
type kchatBridgeNotifier struct {
	baseURL string
	client  *http.Client
}

func (b *kchatBridgeNotifier) enabled() bool { return b != nil && b.baseURL != "" }

// renderApprovalCard POSTs a render request to kchat-bridge for the
// approval referenced by the event payload. The payload schema is the
// one emitted by workflow.Engine.RequestApproval / Decide — `approval_id`
// plus the event's tenant_id envelope.
func (b *kchatBridgeNotifier) renderApprovalCard(ctx context.Context, e events.Event) error {
	var payload struct {
		ApprovalID uuid.UUID `json:"approval_id"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return fmt.Errorf("decode approval event payload: %w", err)
	}
	if payload.ApprovalID == uuid.Nil {
		return fmt.Errorf("approval event missing approval_id")
	}
	body, err := json.Marshal(map[string]any{
		"tenant_id":   e.TenantID,
		"approval_id": payload.ApprovalID,
	})
	if err != nil {
		return fmt.Errorf("marshal render body: %w", err)
	}
	url := b.baseURL + "/kchat/approvals/render"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	// Drain-and-close pattern so the net/http Transport can return the
	// TCP connection to its keep-alive pool. Without the drain, each
	// successful render POST would force a fresh connection — wasteful
	// given this runs on every drained approval event.
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

// isApprovalNotificationEvent returns true for the approval lifecycle
// event types the kchat-bridge should re-render a card for. Decision
// events (granted, rejected) and step advancement also produce follow
// up cards so the original approver — and the requester — see the
// state change inline.
func isApprovalNotificationEvent(t string) bool {
	switch t {
	case "approval.requested", "approval.step_advanced", "approval.granted", "approval.rejected":
		return true
	default:
		return false
	}
}
