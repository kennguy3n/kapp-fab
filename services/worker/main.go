// Command worker is the Kapp async worker process. It drains the event
// outbox and publishes messages to NATS. Later phases add workflow timer
// advancement, retries, and background job handlers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"golang.org/x/sync/errgroup"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/exporter"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk/imap/goimap"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/eventrouter"
	mktruntime "github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
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

	// Adaptive batcher bounds. min keeps each drain large enough
	// to amortise the FOR UPDATE SKIP LOCKED roundtrip; max caps
	// per-iteration work so a runaway grow loop cannot monopolise
	// the worker. drainBatch (above) seeds the starting point so
	// the steady-state behaviour matches the previous fixed-batch
	// configuration until the batcher observes a reason to move.
	adaptiveBatchMin = 16
	adaptiveBatchMax = 512

	// Latency budget for one DrainBatch + delivery cycle. Picked
	// to leave plenty of headroom above the NATS publish RTT
	// (~milliseconds) and the kchat-bridge side-effect calls
	// (~tens of milliseconds), while still shrinking aggressively
	// if a downstream goes slow and pushes the cycle past a
	// quarter-second.
	adaptiveLatencyBudget = 250 * time.Millisecond
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

	logger := platform.NewLogger(platform.LoggerConfig{
		Format:  cfg.LogFormat,
		Level:   cfg.LogLevel,
		Service: "worker",
		Env:     cfg.Env,
	}, os.Stderr)
	platform.InstallDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// OpenTelemetry tracing init runs BEFORE platform.NewPool so the
	// otelpgx tracer attached inside NewPool finds the global
	// TracerProvider this call sets. When KAPP_OTEL_ENDPOINT is
	// unset the provider is a no-op and the otelpgx hot-path is a
	// nil-check per query.
	tracingShutdown, err := platform.InitTracing(ctx, platform.LoadTracingConfig("kapp-worker", cfg.Env))
	if err != nil {
		return fmt.Errorf("worker: init tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown", slog.String("err", err.Error()))
		}
	}()

	// Per-worker Prometheus registry. Hoisted up here (above the
	// scheduler handler registration) so individual handlers can
	// opt into emitting telemetry at construction time — notably
	// the record-count reconciler, which surfaces drift between
	// tenant_record_counts and the krecords scan as a gauge so
	// silent regressions in the bump path page before they become
	// a billing dispute. The actual /metrics listener still wires
	// up below; the registry is just a thread-safe accumulator
	// until then.
	metrics := platform.NewMetricsRegistry()

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

	// Replica routing (A1). Worker uses the router for the scheduled
	// report runner and the insights query-cache refresh handler —
	// both pure SELECTs. Wiring (open pool, router build, sampler
	// start, metrics) is shared with the other four service
	// entrypoints via platform.WireReplicaRouter — see its docstring
	// for the teardown contract the helper enforces internally
	// (stopGauge → router.Close → replicaPool.Close). The metrics
	// registry was hoisted above the pool open (A2) so we can pass
	// it directly here and have the helper publish
	// kapp_replica_lag_seconds / kapp_replica_sample_errors_total
	// alongside the regular worker metrics.
	dbRouter, stopReplica, err := platform.WireReplicaRouter(ctx, "worker", cfg, pool, metrics)
	if err != nil {
		return err
	}
	defer stopReplica()

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
			slog.Default().Warn("nats drain",
				slog.String("err", err.Error()),
			)
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

	// Low-stock alert sweeper, webhook retry loop, scheduler,
	// export queue, and autoscaler are all SINGLETON loops: each
	// claims work cross-tenant against shared tables. Running
	// them on N replicas would either produce duplicates
	// (scheduler re-firing the same scheduled_actions row before
	// the first replica finishes), race on shared state
	// (autoscaler emitting conflicting platform_scale_events for
	// the same cell), or simply waste work (alerts sweeper
	// scanning every tenant N times). Phase 3 gates the entire
	// singleton stack behind a Postgres advisory-lock leader
	// election so N>1 worker pods can run hot-standby — only the
	// leader spawns these goroutines, and a peer takes over
	// within one poll interval when the leader's session
	// disconnects.
	alerts := newStockAlertWorker(pool, adminPool, publisher, dbutil.SetTenantContext)

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
	ktypeCache := platform.NewLRUCache(cfg.KTypeCacheSize, 5*time.Minute)
	ktypeRegistry := ktype.NewPGRegistry(pool, ktypeCache)
	recordStore := record.NewPGStoreWithRouter(dbRouter, ktypeRegistry, publisher, auditor)

	// Per-tenant field-level encryption: mirrors services/api/deps_build.go
	// initialisation so the worker can decrypt marketplace_extension_installations.
	// signing_secret values on the event-router fan-out path. Without this,
	// production deploys (which set KAPP_MASTER_KEY) would observe ciphertext
	// and fail HMAC signing with a misleading mismatch error. Missing /
	// short master keys keep the worker on the plaintext path (dev mode).
	var workerKeyManager *tenant.KeyManager
	if masterKey, mkErr := tenant.LoadMasterKey(); mkErr == nil {
		prevKey, perr := tenant.LoadPrevMasterKey()
		if perr != nil {
			return perr
		}
		km, kmErr := tenant.NewKeyManagerWithPrev(masterKey, prevKey, time.Hour)
		if kmErr != nil {
			return kmErr
		}
		workerKeyManager = km
		log.Printf("worker: per-tenant field encryption enabled")
	} else if !errors.Is(mkErr, tenant.ErrMasterKeyMissing) {
		return mkErr
	} else {
		log.Printf("worker: per-tenant field encryption disabled (%s unset)", tenant.MasterKeyEnvVar)
	}

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
	// Phase N5 — daily budget variance sweeper. Walks every active
	// finance.budget for the tenant, recomputes MTD actuals against
	// budget_lines for the current calendar month, and emits an
	// in-app notification per (account, cost_center) line whose
	// |variance| crosses the budget's (or the platform default)
	// threshold. Notifications fan out through the same router that
	// handles low-stock alerts so KChat DM / webhook / email
	// transports work out of the box.
	budgetStore := finance.NewBudgetStore(pool)
	schedRegistry.Register(
		finance.ActionTypeBudgetVariance,
		finance.NewVarianceAlertHandler(budgetStore, router.store),
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
	// Daily tenant_record_counts reconciliation — defends against
	// drift between the in-transaction counter bump (record store)
	// and the krecords source of truth. Reads the actual count(*),
	// UPSERTs it back into tenant_record_counts, and emits a Prom
	// drift gauge so a silent regression in the bump path pages
	// before it turns into an under-billed tenant. Seeded per
	// tenant by tenant.seedDefaultScheduledActions at the same 24h
	// cadence as the usage snapshot.
	schedRegistry.Register(
		platform.ActionTypeRecordCountRecount,
		platform.NewRecordCountReconciler(pool).WithMetrics(metrics),
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
	reportRunner := reporting.NewRunnerWithRouter(dbRouter)
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
	insightsRunner := insights.NewRunnerWithRouter(dbRouter, insightsCacheStore, insightsQueryStore, reportRunner).
		WithFeaturePolicy(insightsFeatures)
	schedRegistry.Register(
		insights.ActionTypeQueryCacheRefresh,
		NewQueryCacheRefreshHandler(insightsQueryStore, insightsRunner),
	)

	exportWorker := NewExportWorker(exporter.NewStore(pool, adminPool), recordStore, 5*time.Second)
	autoscaleEngine := platform.NewAutoscaleEngine(pool, platform.DefaultAutoscalePolicy(), nil)
	autoscaleLoop := platform.NewAutoscaleLoop(autoscaleEngine, 60*time.Second)

	// Adaptive batch sizing for the outbox drain. Starts at
	// drainBatch (the historical fixed value) so steady-state
	// behaviour is unchanged until the batcher observes a reason
	// to grow (saturation + fast) or shrink (over latency
	// budget). See services/worker/adaptive.go for the policy.
	batcher := NewAdaptiveBatcher(drainBatch, adaptiveBatchMin, adaptiveBatchMax, adaptiveLatencyBudget)

	slog.Default().Info("worker started",
		slog.String("nats", natsURL),
		slog.String("kchat_bridge", bridge.baseURL),
	)

	// Singleton goroutines + outbox drain are gated behind a
	// Postgres advisory-lock leader election so multi-replica
	// worker deployments only run them on the elected leader. A
	// peer takes over within one pollInterval (5s default) of
	// the leader's session disconnecting (crash, SIGKILL,
	// network partition longer than TCP keepalive).
	identity := workerIdentity()

	// Bind the outbox-drain metrics to the registry created above
	// the scheduler-handler block. Hoisting NewMetricsRegistry up
	// (A2) lets handlers opt into telemetry at construction time
	// (notably the record-count reconciler's drift gauge AND the A1
	// replica lag/error gauges) without forward-referencing a
	// registry that does not yet exist; the leader elector / drain
	// loop still see the same instance.
	drainDur := metrics.Histogram("kapp_outbox_drain_duration_seconds", "Outbox drain batch latency in seconds.", platform.DefaultDurationBuckets, "result")
	drainEvents := metrics.Counter("kapp_outbox_events_total", "Outbox events drained from the queue.", "result")

	if cfg.MetricsAddr != "" {
		go runWorkerMetricsServer(ctx, logger, cfg.MetricsAddr, metrics)
	}

	election := platform.NewLeaderElection(cfg.DatabaseURL, "kapp-worker", identity).WithMetrics(metrics)
	// Helpdesk-IMAP supervisor (Surface G). Wires the per-mailbox
	// Poller fleet against the leader-elected goroutine set. The
	// factory builds a fresh go-imap/v2 client per mailbox at
	// converge time; the supervisor short-circuits Start for
	// already-active mailboxes so the factory's per-Start cost
	// (one dial + TLS handshake) only fires on real lifecycle
	// transitions.
	//
	// FactoryOptions defaults are appropriate for production:
	// system root certs, 30 s dial timeout, 30 s per-command
	// timeout. Operators with bespoke IMAP servers (self-signed
	// certs, custom cipher pools) can override via env in a
	// follow-up wire-up; today we ship with defaults and the
	// IMAP fleet works against any RFC-compliant server.
	//
	// Password resolution wires the multi-scheme dispatcher: env: /
	// file:// refs are stateless and always work; vault:// / aws:// /
	// gcp:// refs are wired in only when the operator populated the
	// corresponding KAPP_SECRETS_* config. The PasswordCache amortises
	// remote-backend reads across the 60-second converge cadence
	// (5-minute per-mailbox TTL).
	imapFactory := goimap.NewFactory(goimap.FactoryOptions{})
	helpdeskPasswords := newWorkerPasswordResolver(ctx, cfg, 5*time.Minute, slog.Default())
	helpdeskIMAP := newHelpdeskIMAPState(pool, adminPool, recordStore, helpdeskStore, imapFactory, helpdeskPasswords, slog.Default())

	// Marketplace event router (B4). Constructed unconditionally so
	// the deliver() callback can fan events out to extension webhook
	// subscriptions. The transport is a production HTTPTransport
	// (HTTPS-only, 1 MiB body cap); the rate limiter is per-process
	// in-memory keyed by (tenant_id, extension_id). A future B6
	// follow-up may move the limiter to Redis so multiple worker
	// replicas share the budget — today each replica enforces its
	// own bucket, which is an over-budget bound rather than an
	// under-budget one (acceptable for ingress capacity protection).
	mktTransport := mktruntime.NewHTTPTransport()
	var mktEncryptor mktruntime.Encryptor
	if workerKeyManager != nil {
		mktEncryptor = workerKeyManager
	} else {
		mktEncryptor = mktruntime.NoopEncryptor()
	}
	mktLimiter := eventrouter.NewLimiter(100, time.Now)
	mktRouter := eventrouter.NewRouter(pool, mktTransport, mktEncryptor, mktLimiter, time.Now)

	// Marketplace review pipeline (B7). The review worker polls
	// marketplace_extension_review_state for `submitted` rows and
	// runs the automated checks (signature, manifest, KType
	// namespace, endpoint scheme, icon, UI static analysis, etc.)
	// against each. Singleton: only the elected leader drains the
	// queue. The pipeline pulls the bundle from the version row's
	// bundle_url over HTTPS; HTTPSource caps the body at
	// MaxBundleSizeBytes and the per-request timeout at 30 s.
	mktStore := marketplace.NewStore(pool)
	mktReviewPipeline := buildReviewPipeline(mktStore, 30*time.Second)
	// Reuse `identity` (computed above for leader election) rather
	// than calling workerIdentity() again — the function reads
	// os.Hostname() which is cheap but the duplication invites
	// drift if the identity source ever becomes non-deterministic.
	// The same identity is recorded as claimed_by on each review
	// claim and threaded through to UpdateReviewState's claim
	// guard, so consistency between leader-election and claim
	// attribution is load-bearing for forensic correlation.
	mktReviewWorker := NewReviewWorker(mktStore, mktReviewPipeline, slog.Default(), 5*time.Second, 4, identity)

	return election.Run(ctx, func(leaderCtx context.Context) error {
		return leadWorker(leaderCtx, leaderState{
			cfg:               cfg,
			publisher:         publisher,
			alerts:            alerts,
			router:            router,
			schedStore:        schedStore,
			schedRegistry:     schedRegistry,
			exportWorker:      exportWorker,
			autoscaleLoop:     autoscaleLoop,
			batcher:           batcher,
			nc:                nc,
			bridge:            bridge,
			helpdeskIMAP:      helpdeskIMAP,
			drainHistogram:    drainDur,
			drainCounter:      drainEvents,
			marketplaceRouter: mktRouter,
			reviewWorker:      mktReviewWorker,
		})
	})
}

// runWorkerMetricsServer mounts /metrics on a dedicated http.Server
// bound to cfg.MetricsAddr (e.g. ":9090"). Separate from the worker's
// main loop because the worker doesn't otherwise serve HTTP — this is
// the entire admin-port surface for the worker binary.
func runWorkerMetricsServer(ctx context.Context, logger *slog.Logger, addr string, reg *platform.MetricsRegistry) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Worker exposes only /metrics + /healthz, both short request-
	// response cycles. MetricsHTTPTimeouts uses tighter values than
	// the user-facing services. Tuning lives under KAPP_METRICS_*
	// (not KAPP_HTTP_*) so the same env namespace tunes every
	// metrics scrape listener across the fleet (api + worker) and
	// the user-facing KAPP_HTTP_* namespace cannot bleed in.
	timeouts := platform.LoadHTTPTimeoutsWithPrefix("KAPP_METRICS", platform.MetricsHTTPTimeouts())
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	timeouts.Apply(srv)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	logger.Info("metrics listening", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics listener", slog.String("err", err.Error()))
	}
}

// leaderState bundles the dependencies that the leader-only goroutines
// + drain loop need. Keeping them in one struct lets us pass a single
// argument into leadWorker, avoids a parameter explosion, and keeps
// the leader-callback signature compatible with platform.LeaderElection.
type leaderState struct {
	cfg           *platform.Config
	publisher     *events.PGPublisher
	alerts        *stockAlertWorker
	router        *notificationRouter
	schedStore    *scheduler.Store
	schedRegistry *scheduler.Registry
	exportWorker  *ExportWorker
	autoscaleLoop *platform.AutoscaleLoop
	batcher       *AdaptiveBatcher
	nc            *nats.Conn
	bridge        *kchatBridgeNotifier

	// marketplaceRouter fans drained outbox events out to
	// marketplace_webhook_subscriptions registered at install time
	// (manifest.webhooks_consumed[] + manifest.posting_hooks[]).
	// nil disables the marketplace fan-out (test/dev paths that
	// don't have the extension runtime tables wired). The router
	// holds its own transport + rate-limit state.
	marketplaceRouter *eventrouter.Router

	// reviewWorker drives the B7 automated-review pipeline:
	// claims `submitted` versions, runs the structural / signature
	// checks, persists findings, and transitions to
	// automated_passed | manual_review | rejected. Singleton on
	// the elected leader so the same version isn't double-scanned.
	reviewWorker *ReviewWorker

	// helpdeskIMAP is the supervisor for the per-mailbox IMAP
	// poller goroutines (Surface G). nil when adminPool is
	// unavailable or no IMAP client factory is wired; in either
	// case the leader skips the supervisor goroutine and the
	// helpdesk inbound path stays on the webhook-only surface.
	helpdeskIMAP *helpdeskIMAPState

	// Prometheus-compatible drain metrics. Observed inside drainLoop
	// after each DrainBatch completes — latency goes into the histogram,
	// event count goes into the counter (split by result label so
	// success vs error cases are countable separately).
	drainHistogram *platform.HistogramVec
	drainCounter   *platform.CounterVec
}

// leadWorker runs the singleton goroutines + outbox drain. Invoked
// from inside platform.LeaderElection.Run, so it is guaranteed that
// only one worker replica is executing this function at any time
// (modulo the brief two-leader overlap window documented in
// platform/leader.go). On leaderCtx cancellation (lock lost,
// process shutting down) all sub-goroutines unwind through the
// derived contexts and the function returns.
func leadWorker(leaderCtx context.Context, s leaderState) error {
	// Singleton sweepers. Each owns its own ticker / claim loop
	// and unwinds when leaderCtx cancels, so we don't need to
	// join them explicitly — the next leader will start fresh
	// copies.
	go s.alerts.Run(leaderCtx)
	go s.router.runWebhookRetryLoop(leaderCtx, 5*time.Second)
	go scheduler.RunLoop(leaderCtx, s.schedStore, s.schedRegistry, 10*time.Second)
	go s.exportWorker.Run(leaderCtx)
	go s.autoscaleLoop.Run(leaderCtx)
	if s.reviewWorker != nil {
		go s.reviewWorker.Run(leaderCtx)
	}
	if s.helpdeskIMAP != nil {
		// Per-mailbox IMAP pollers. The supervisor handles
		// Manager.StopAll on leaderCtx cancellation so a
		// graceful leadership transfer drains every active
		// connection cleanly. Logs its own start/drain lines.
		go func() { _ = s.helpdeskIMAP.supervisor.Run(leaderCtx) }()
	}

	return drainLoop(leaderCtx, s)
}

// drainLoop is the LISTEN/NOTIFY-driven outbox drain, extracted out of
// run() so it can be invoked as the leader callback. The body is
// otherwise identical to the previous in-line implementation: dedicated
// *pgx.Conn for LISTEN, capped exponential backoff on reconnect, and
// adaptive batch sizing via s.batcher.
func drainLoop(ctx context.Context, s leaderState) error {

	// Phase 2.2: LISTEN/NOTIFY-driven drain loop. A dedicated *pgx.Conn
	// (NOT a pgxpool slot) subscribes to the "kapp_events" channel. On
	// INSERT the trigger fires pg_notify and the worker wakes within
	// milliseconds. A 2s timeout on WaitForNotification acts as a
	// heartbeat fallback for the edge case where a notify is lost.
	//
	// Using pgx.Connect directly (instead of pool.Acquire) deliberately
	// keeps the LISTEN connection OUT of the pgxpool so it does not
	// permanently occupy a pool slot — the worker also runs scheduler,
	// export, autoscaler, and webhook-retry goroutines that all draw
	// from the same pool, and stealing a slot for the life of the
	// process would reduce concurrency for the rest of them.
	//
	// Failure handling: WaitForNotification's error is now inspected. A
	// DeadlineExceeded (from the 2s wait timeout) is the expected idle
	// path. Any other error indicates the underlying socket has died
	// (PG restart, network partition, idle disconnect); in that case we
	// log a warning, close the dead conn, sleep one tickInterval to
	// avoid a hot reconnect spin if the DB itself is down, then re-dial
	// and re-issue LISTEN. The reconnect loop never gives up — the
	// worker is the only path that drains the outbox, so silently
	// degrading to a permanent CPU-burning loop (the bug Devin Review
	// caught) would be much worse than a slow recovery cycle.
	listenConn, err := acquireListenConn(ctx, s.cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("worker: initial LISTEN conn: %w", err)
	}
	defer func() {
		if listenConn != nil {
			_ = listenConn.Close(context.Background())
		}
	}()

	deliverFn := deliver(s.nc, s.bridge, s.router, s.marketplaceRouter)
	for {
		waitCtx, waitCancel := context.WithTimeout(ctx, tickInterval)
		_, waitErr := listenConn.WaitForNotification(waitCtx)
		waitCancel()

		if ctx.Err() != nil {
			slog.Default().Info("shutdown signal received")
			return nil
		}

		// Any error other than the expected deadline means the LISTEN
		// socket has been disrupted. Without this branch the loop would
		// spin at the speed of DrainBatch (hundreds of iterations per
		// second), saturating Postgres with no useful work and silently
		// losing the LISTEN subscription forever.
		//
		// The reconnect path is an inner loop that retries with
		// capped exponential backoff (tickInterval → 2× → 4× → ... →
		// maxBackoff) until either ctx is cancelled or
		// acquireListenConn succeeds. This guarantees that listenConn
		// is non-nil when we exit the branch and re-enter
		// WaitForNotification at the top of the next iteration —
		// closing the nil-panic window Devin Review caught in the
		// first iteration of this fix. The reconnect loop never gives
		// up because the worker is the only path that drains the
		// outbox; bailing out would silently lose every subsequent
		// event until the process restarts.
		if waitErr != nil && !errors.Is(waitErr, context.DeadlineExceeded) {
			slog.Default().Warn("LISTEN connection error, reconnecting",
				slog.String("err", waitErr.Error()),
			)
			_ = listenConn.Close(context.Background())
			listenConn = nil

			backoff := tickInterval
			const maxBackoff = 30 * time.Second
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				newConn, rerr := acquireListenConn(ctx, s.cfg.DatabaseURL)
				if rerr == nil {
					listenConn = newConn
					slog.Default().Info("LISTEN reconnected")
					break
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				slog.Default().Warn("LISTEN reconnect failed",
					slog.Duration("retry_in", backoff),
					slog.String("err", rerr.Error()),
				)
			}
		}

		// Adaptive batch sizing: read the current limit, time the
		// drain, then feed (n, duration) back into the batcher.
		// The latency-first policy in adaptive.go aggressively
		// shrinks on overshoots, so a sudden downstream slow-down
		// (NATS, kchat-bridge) tightens batches within a few
		// iterations without operator intervention.
		limit := s.batcher.Limit()
		start := time.Now()
		n, err := s.publisher.DrainBatch(ctx, limit, deliverFn)
		dur := time.Since(start)
		result := "ok"
		if err != nil {
			result = "error"
			slog.Default().Error("drain batch",
				slog.Int("limit", limit),
				slog.String("err", err.Error()),
			)
		}
		if s.drainHistogram != nil {
			s.drainHistogram.Observe(dur.Seconds(), result)
		}
		if s.drainCounter != nil && n > 0 {
			// The `n` returned by DrainBatch counts events that
			// were successfully delivered AND marked
			// `delivered_at = now()` inside a per-tenant tx that
			// committed. Even when the cycle-level `err` is
			// non-nil (e.g. a later tenant's drain failed mid-
			// loop), the n events that made it through are
			// genuinely "ok" — they will not be re-delivered.
			// Label them as such. Cycle-level success/failure
			// is captured separately by the histogram's `result`
			// label above.
			s.drainCounter.Add(uint64(n), "ok")
		}
		s.batcher.Observe(n, dur)
	}
}

// workerIdentity returns a leader-log identity for this process. Prefers
// the OS hostname (which in Kubernetes is the pod name, perfect for
// correlating logs against pods); falls back to a fixed string when
// the hostname is unavailable (rare; only happens on misconfigured
// containers).
func workerIdentity() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "kapp-worker"
}

// acquireListenConn dials a dedicated *pgx.Conn (outside the pgxpool) and
// issues LISTEN kapp_events on it. Centralising the dial + LISTEN sequence
// keeps the initial-connect path and the reconnect path identical, so a
// future tweak to the LISTEN channel name or the conn config only needs
// to land in one place. On any failure the connection is closed and the
// error is returned to the caller.
func acquireListenConn(ctx context.Context, connString string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("dial listen conn: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN kapp_events"); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("LISTEN kapp_events: %w", err)
	}
	return conn, nil
}

// deliver returns the drain-callback that publishes a batch of events to NATS
// and fans out best-effort side-effects (KChat bridge cards, notification
// router). Each event in the batch is delivered concurrently (max 8 goroutines)
// to cut tail latency when a single side-effect (e.g. an HTTP call to
// kchat-bridge) is slow. The concurrency limit prevents unbounded goroutine
// growth on very large batches.
//
// Error semantics: any NATS publish failure fails the entire batch (so the
// outbox row stays undelivered and retries on the next drain cycle).
// Side-effect failures (bridge, router) are logged but do NOT fail the batch —
// the event is already durably on NATS, so a flapping sidecar never blocks
// forward progress of the outbox.
func deliver(nc *nats.Conn, bridge *kchatBridgeNotifier, router *notificationRouter, mktRouter *eventrouter.Router) func(ctx context.Context, batch []events.Event) error {
	return func(ctx context.Context, batch []events.Event) error {
		g, ctx := errgroup.WithContext(ctx)
		g.SetLimit(8)
		for _, e := range batch {
			e := e
			g.Go(func() error {
				subject := fmt.Sprintf("kapp.events.%s", e.Type)
				payload, err := json.Marshal(e)
				if err != nil {
					return fmt.Errorf("marshal event %s: %w", e.ID, err)
				}
				if err := nc.Publish(subject, payload); err != nil {
					return fmt.Errorf("publish %s: %w", subject, err)
				}
				// Best-effort side-effects: failures are logged, not propagated.
				if bridge.enabled() && isApprovalNotificationEvent(e.Type) {
					if err := bridge.renderApprovalCard(ctx, e); err != nil {
						slog.Default().Warn("kchat render",
							slog.String("event_type", e.Type),
							slog.String("err", err.Error()),
						)
					}
				}
				if router != nil {
					router.route(ctx, e)
				}
				// Marketplace event router (B4). Best-effort side-
				// effect alongside NATS publish + kchat-bridge —
				// route errors (including subscription-lookup DB
				// outages) are logged but never bubble up. A slow
				// or broken extension cannot stall the outbox
				// drain, and a transient marketplace-DB issue
				// cannot block NATS / kchat-bridge delivery for
				// the rest of the batch. Extension event delivery
				// has at-most-once semantics today; a follow-up
				// (B6 / B7) can add a dead-letter table for
				// failed routings if at-least-once is needed. The
				// router writes per-attempt dispatch_log rows; a
				// higher-level metric on dispatch outcomes is
				// also layered on at B6 / B7 time. The error
				// contract is documented at
				// internal/marketplace/eventrouter/router.go on
				// the RouteBatch doc-comment.
				if mktRouter != nil {
					if _, err := mktRouter.RouteBatch(ctx, []events.Event{e}); err != nil {
						slog.Default().Warn("marketplace router",
							slog.String("event_type", e.Type),
							slog.String("event_id", e.ID.String()),
							slog.String("err", err.Error()),
						)
					}
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return err
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
