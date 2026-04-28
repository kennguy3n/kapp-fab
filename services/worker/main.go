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

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/exporter"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/print"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

const (
	tickInterval = 2 * time.Second
	drainBatch   = 100
)

// workerSystemActor is the deterministic non-nil actor attributed
// to background-generated records in the worker (recurring invoice
// generator, future SLA-driven writes, scheduler-owned patches).
// Parallels phaseASystemActor in services/api/records.go so audit
// trails remain coherent whether the originating service was the
// API or the worker.
var workerSystemActor = uuid.MustParse("00000000-0000-0000-0000-000000000003")

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
		bridge:       bridge,
		client:       &http.Client{Timeout: 5 * time.Second},
		pool:         pool,
		adminPool:    adminPool,
		store:        notifications.NewStore(pool),
		smtp:         notifications.NewSMTPAdapter(smtpCfg),
		webhookStore: notifications.NewWebhookStore(pool),
	}

	// Low-stock alert sweeper runs alongside the outbox drain so a
	// below-threshold SKU produces a KChat alert within one sweep
	// interval. The sweeper shares the outbox publisher, so emitted
	// alerts go through the same delivery / dedupe pipeline as any
	// other `inventory.*` event.
	alerts := newStockAlertWorker(pool, adminPool, publisher, dbutil.SetTenantContext)
	go alerts.Run(ctx)

	// Webhook retry loop. Failed webhook POSTs persist a row in
	// webhook_deliveries with next_retry_at set; this loop atomically
	// claims due rows across tenants and re-posts via the same
	// deliverWebhook path. Running the retry loop out-of-band keeps
	// the outbox drain free of any time.Sleep on a misbehaving
	// customer endpoint — a slow tenant webhook no longer stalls
	// unrelated tenants' events.
	go router.runWebhookRetryLoop(ctx, 5*time.Second)

	// Scheduled actions engine. Registers both:
	//   - the recurring AR invoice generator against action_type
	//     "recurring_invoice"; reuses the api's record store +
	//     invoice poster wiring so generated invoices emit the same
	//     audit + outbox + inventory hook chain as a hand-authored
	//     invoice. The worker stays a lightweight subset of the api
	//     stack — no encryption, no quota enforcer — because
	//     neither matters for a synthetic background-generated
	//     draft.
	//   - the SLA breach sweeper against action_type
	//     "sla_breach_check"; the tenant wizard seeds that cadence
	//     per new tenant so the handler has live work once the
	//     scheduler claims a row.
	schedStore := scheduler.NewStore(pool, adminPool)
	schedRegistry := scheduler.NewRegistry()
	auditor := audit.NewPGLogger(pool)
	ktypeCache := platform.NewLRUCache(1024, 5*time.Minute)
	ktypeRegistry := ktype.NewPGRegistry(pool, ktypeCache)
	recordStore := record.NewPGStore(pool, ktypeRegistry, publisher, auditor)
	exchangeRates := ledger.NewExchangeRateStore(pool)
	ledgerStore := ledger.NewPGStore(pool, publisher, auditor).WithExchangeRates(exchangeRates)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, recordStore)
	inventoryStore := inventory.NewPGStore(pool, publisher, auditor)
	inventoryHook := inventory.NewPosterHook(inventoryStore)
	invoicePoster.
		WithSalesInvoiceHook(inventoryHook.OnSalesInvoicePosted).
		WithPurchaseBillHook(inventoryHook.OnPurchaseBillPosted)
	// systemActor stamps Created/UpdatedBy on generated invoices and
	// is forwarded into PostSalesInvoice, which rejects uuid.Nil.
	// Matches the deterministic actor used by the api's records
	// handler (services/api/records.go) so audit trails stay
	// coherent across origin services — a generated-then-posted
	// invoice and a hand-authored-then-posted invoice attribute to
	// the same synthetic UUID when no human actor is in context.
	recurringEngine := finance.NewRecurringEngine(recordStore,
		func(ctx context.Context, tenantID, invoiceID, actorID uuid.UUID) error {
			_, err := invoicePoster.PostSalesInvoice(ctx, tenantID, invoiceID, actorID)
			return err
		},
	).WithSystemActor(workerSystemActor)
	schedRegistry.Register(finance.ActionTypeRecurringInvoice, recurringEngine)
	helpdeskStore := helpdesk.NewStore(pool)
	schedRegistry.Register(
		helpdesk.ActionTypeSLABreach,
		helpdesk.NewSLABreachHandler(pool, helpdeskStore, publisher, dbutil.SetTenantContext),
	)
	// Inventory reorder automation — sweeps low-stock items per
	// tenant and opens draft procurement.purchase_orders against
	// each item's preferred supplier. Idempotent within a 24h
	// window so consecutive sweeps never duplicate a draft.
	reorderHandler := inventory.NewReorderHandler(recordStore, inventoryStore).
		WithSystemActor(workerSystemActor)
	schedRegistry.Register(inventory.ActionTypeReorder, reorderHandler)
	// Monthly unrealized FX gain/loss revaluation. Walks open AR/AP
	// foreign-currency balances per tenant, posts adjustment entries
	// against the per-account gain/loss accounts at the current rate.
	schedRegistry.Register(
		ledger.ActionTypeUnrealizedGainLoss,
		ledger.NewUnrealizedGainLossJob(ledgerStore, exchangeRates, workerSystemActor),
	)
	// Daily tenant usage snapshot — re-samples storage_bytes and
	// krecord_count per tenant so the dashboard's absolute
	// counters are accurate even on quiet days. API-call deltas
	// continue to flow through the metering middleware.
	meteringStore := tenant.NewMeteringStore(pool)
	schedRegistry.Register(
		tenant.ActionTypeUsageSnapshot,
		tenant.NewUsageSnapshotHandler(meteringStore),
	)
	// Daily data retention sweep — deletes old audit/event/SLA/
	// notification rows per tenant according to the policies in
	// data_retention_policies. Each DELETE runs under
	// dbutil.WithTenantTx so RLS is the final guarantor that we
	// only ever touch the target tenant's rows.
	retentionStore := platform.NewRetentionStore(pool, adminPool)
	schedRegistry.Register(
		platform.ActionTypeDataRetentionSweep,
		platform.NewRetentionSweeper(retentionStore),
	)
	// Periodic report scheduler — iterates report_schedules per
	// tenant tick, runs each due saved report, renders to CSV or
	// PDF, and emails the configured recipient list. Uses the same
	// SMTP adapter the notification router does so the local-dev
	// "SMTP disabled" path is a soft no-op.
	reportScheduleStore := reporting.NewScheduleStore(pool)
	reportSavedStore := reporting.NewStore(pool)
	reportRunner := reporting.NewRunner(pool)
	pdfConverter := print.DetectConverter()
	schedRegistry.Register(
		reporting.ActionTypeReportSchedule,
		NewReportScheduleHandler(reportScheduleStore, reportSavedStore, reportRunner, pdfConverter, router.smtp),
	)
	// Phase K — LMS course-completion certificate auto-issuer.
	// Walks completed lms.enrollment rows per tenant tick and issues
	// a certificate for any that do not already have one.
	certificateIssuer := lms.NewCertificateIssuer(recordStore, pool)
	schedRegistry.Register(
		CertificateActionType,
		NewCertificateAutoIssuer(certificateIssuer, recordStore, workerSystemActor),
	)

	// Phase L — Insights query cache refresh. The handler walks
	// every saved query for the tenant and runs it through the
	// cache-aware runner; fresh cache rows short-circuit so the
	// sweeper effectively pays the SQL cost only for expired
	// entries. Seeded by tenant.seedDefaultScheduledActions for
	// plans that include FeatureInsights.
	insightsQueryStore := insights.NewQueryStore(pool)
	insightsCacheStore := insights.NewCacheStore(pool)
	insightsFeatures := tenant.NewFeatureStore(pool)
	insightsRunner := insights.NewRunner(pool, insightsCacheStore, insightsQueryStore, reportRunner).
		WithFeaturePolicy(insightsFeatures)
	schedRegistry.Register(
		insights.ActionTypeQueryCacheRefresh,
		NewQueryCacheRefreshHandler(insightsQueryStore, insightsRunner),
	)

	go scheduler.RunLoop(ctx, schedStore, schedRegistry, 10*time.Second)

	// Phase K — data export queue worker. Runs alongside the
	// scheduled-action loop on a separate ticker because export
	// payloads can be large; we don't want a slow CSV render to
	// stall the scheduled-action draining cadence.
	go NewExportWorker(exporter.NewStore(pool, adminPool), recordStore, 5*time.Second).Run(ctx)

	// Phase G — cell autoscaler. Reads the `cells` table on a
	// minute cadence, applies the configured policy thresholds,
	// and persists each decision into platform_scale_events. Runs
	// platform-wide (every cell) so it does NOT participate in
	// scheduled_actions, which are tenant-scoped by design.
	autoscaleEngine := platform.NewAutoscaleEngine(pool, platform.DefaultAutoscalePolicy(), nil)
	go platform.NewAutoscaleLoop(autoscaleEngine, 60*time.Second).Run(ctx)

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
