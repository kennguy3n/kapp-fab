// Command api is the Kapp HTTP gateway / BFF. It exposes REST endpoints for
// KType and KRecord operations, health probes, and (future) event streaming.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/base"
	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/docs"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/exporter"
	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/forms"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/hr"
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
	"github.com/kennguy3n/kapp-fab/internal/sales"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("api: %v", err)
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

	// Optional admin pool used for cross-tenant control-plane reads
	// (tenant → user lookups, public form resolution). Nil when
	// ADMIN_DB_URL is unset; callers fall back to the app pool and
	// return empty results under the default-deny RLS policy.
	var adminPool *pgxpool.Pool
	if cfg.AdminDatabaseURL != "" {
		adminPool, err = platform.NewPool(ctx, cfg.AdminDatabaseURL)
		if err != nil {
			return err
		}
		defer adminPool.Close()
	}

	tenantSvc := tenant.NewPGStore(pool)
	ktypeCache := platform.NewLRUCache(1024, 5*time.Minute)
	ktypeRegistry := ktype.NewPGRegistry(pool, ktypeCache)
	eventPublisher := events.NewPGPublisher(pool)
	auditor := audit.NewPGLogger(pool)
	recordStore := record.NewPGStore(pool, ktypeRegistry, eventPublisher, auditor)
	// Per-tenant field-level encryption is opt-in: when KAPP_MASTER_KEY
	// is set, derive per-tenant keys and plug the KeyManager into the
	// record store so schema fields marked {"encrypted": true} round-trip
	// through the database as ciphertext. Missing/short master keys are
	// logged and the store falls back to plaintext so local dev keeps
	// working without secrets plumbing.
	var keyManager *tenant.KeyManager
	if masterKey, err := tenant.LoadMasterKey(); err == nil {
		prevKey, perr := tenant.LoadPrevMasterKey()
		if perr != nil {
			return perr
		}
		km, err := tenant.NewKeyManagerWithPrev(masterKey, prevKey, time.Hour)
		if err != nil {
			return err
		}
		keyManager = km
		recordStore = recordStore.WithEncryptor(km)
		if prevKey != nil {
			log.Printf("api: per-tenant field encryption enabled (dual-key rotation active)")
		} else {
			log.Printf("api: per-tenant field encryption enabled")
		}
	} else if !errors.Is(err, tenant.ErrMasterKeyMissing) {
		return err
	} else {
		log.Printf("api: per-tenant field encryption disabled (%s unset)", tenant.MasterKeyEnvVar)
	}
	workflowEngine := workflow.NewEngine(pool, eventPublisher, auditor)
	formStore := forms.NewStore(pool, ktypeRegistry, recordStore)
	if adminPool != nil {
		formStore = formStore.WithAdminPool(adminPool)
	}
	// Rate limiter: REDIS_URL opts into the distributed Redis-backed
	// limiter so multiple API replicas share a token bucket per
	// tenant. Absent the env var we fall back to the in-process
	// limiter so local dev continues to work without Redis.
	rateLimitCfg := platform.DefaultRateLimitConfig()
	rateLimiter := platform.NewRateLimiter(rateLimitCfg)
	var redisLimiter *platform.RedisRateLimiter
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		rl, err := platform.NewRedisRateLimiter(ctx, redisURL, rateLimitCfg)
		if err != nil {
			log.Printf("api: redis rate limiter init failed, falling back to in-process: %v", err)
		} else {
			redisLimiter = rl
			defer func() { _ = redisLimiter.Close() }()
			log.Printf("api: distributed rate limiter enabled (redis)")
		}
	}
	quotaEnforcer := platform.NewQuotaEnforcer(pool)

	// Phase J — tenant feature flags, plan definitions, and usage
	// metering. FeatureStore backs the per-tenant feature-gate
	// middleware; PlanStore backs /api/v1/plans and plan changes;
	// MeteringStore + MeteringBuffer absorb api_calls and
	// storage_bytes increments without stalling the hot path.
	featureStore := tenant.NewFeatureStore(pool)
	planStore := tenant.NewPlanStore(pool)
	meteringStore := tenant.NewMeteringStore(pool)
	meteringBuffer := platform.NewMeteringBuffer(meteringStore, platform.DefaultMeteringBufferConfig())
	defer meteringBuffer.Close(context.Background())

	// Phase C finance engine — ledger store + invoice poster share
	// the same event publisher + audit logger so journal postings,
	// invoice lifecycle events, and KRecord mutations all emit into
	// the single outbox + audit tables used by the rest of the kernel.
	apiExchangeRates := ledger.NewExchangeRateStore(pool)
	ledgerStore := ledger.NewPGStore(pool, eventPublisher, auditor).WithExchangeRates(apiExchangeRates)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, recordStore)
	paymentPoster := ledger.NewPaymentPoster(ledgerStore, recordStore)

	// Phase D inventory engine — items, warehouses, append-only stock
	// moves, and the derived stock_levels view. Wiring the same event
	// publisher + audit logger keeps inventory mutations on the shared
	// outbox + audit trail. PosterHook plugs the store into
	// InvoicePoster so a posted sales invoice automatically emits a
	// goods-delivery move and a posted purchase bill emits a
	// goods-receipt move; the partial unique index on
	// inventory_moves(source_id, …) keeps replays idempotent.
	inventoryStore := inventory.NewPGStore(pool, eventPublisher, auditor)
	inventoryHook := inventory.NewPosterHook(inventoryStore)
	invoicePoster.
		WithSalesInvoiceHook(inventoryHook.OnSalesInvoicePosted).
		WithPurchaseBillHook(inventoryHook.OnPurchaseBillPosted)

	// Phase E leave-balance ledger + lesson-progress projections.
	// Employee / leave-request / course / lesson records live in the
	// generic KRecord store; the dedicated stores only cover the
	// append-only and per-user rollup tables defined in
	// migrations/000006_hr.sql and 000007_lms.sql.
	hrStore := hr.NewStore(pool)
	lmsStore := lms.NewStore(pool)

	// Phase F wires the shared attachment layer, the Base KApp ad-hoc
	// tables, and the Docs KApp artifact documents on top of the same
	// tenant / idempotency / rate-limit / quota stack used by the rest
	// of the API. The object store defaults to an in-process MemoryStore
	// so local dev works without MinIO; production overrides it by
	// mounting an S3-compatible store through the ObjectStore interface.
	// Object store layering, outermost in:
	//
	//   1. files.PerTenantS3Store  -> routes by tenant id
	//   2. ZK fabric per-tenant *S3Store (bucket comes off the tenants row)
	//   3. Fallback platform-wide *S3Store (legacy MinIO bucket) or MemoryStore
	//
	// Tenants without ZK fabric credentials drop to (3) so existing
	// deployments keep working — the ZK rollout can run gradually
	// instead of all-at-once.
	var fallbackStore files.ObjectStore = files.NewMemoryStore()
	if bucket := os.Getenv("S3_BUCKET"); bucket != "" {
		s3cfg := files.S3StoreConfig{
			Endpoint:       os.Getenv("S3_ENDPOINT"),
			Region:         os.Getenv("S3_REGION"),
			Bucket:         bucket,
			AccessKey:      os.Getenv("S3_ACCESS_KEY"),
			SecretKey:      os.Getenv("S3_SECRET_KEY"),
			ForcePathStyle: true,
		}
		s3store, err := files.NewS3Store(ctx, s3cfg)
		if err != nil {
			return fmt.Errorf("files: init S3 store: %w", err)
		}
		fallbackStore = s3store
		log.Printf("api: fallback object store = S3 (bucket=%s endpoint=%s)", bucket, s3cfg.Endpoint)
	} else {
		log.Printf("api: fallback object store = in-memory (S3_BUCKET unset)")
	}
	zkEndpoint := os.Getenv("ZK_FABRIC_ENDPOINT")
	zkRegion := os.Getenv("ZK_FABRIC_REGION")
	if zkRegion == "" {
		zkRegion = "us-east-1"
	}
	objectStore := files.ObjectStore(fallbackStore)
	if zkEndpoint != "" {
		resolver := newZKTenantResolver(tenantSvc, zkEndpoint, zkRegion)
		perTenant, err := files.NewPerTenantS3Store(files.PerTenantConfig{
			Resolver: resolver,
			Fallback: fallbackStore,
			Endpoint: zkEndpoint,
			Region:   zkRegion,
		})
		if err != nil {
			return fmt.Errorf("files: init per-tenant ZK store: %w", err)
		}
		objectStore = perTenant
		log.Printf("api: per-tenant ZK object store enabled (endpoint=%s)", zkEndpoint)
	}
	filesStore := files.NewStore(pool, objectStore)
	baseStore := base.NewStore(pool)
	docsStore := docs.NewStore(pool)

	// Register the domain KTypes at boot so a fresh deployment has a
	// working schema set without requiring an out-of-band migration.
	// The registry upserts on conflict so repeated restarts are a
	// no-op. Finance (Phase C), inventory (Phase D), and HR+LMS
	// (Phase E) all register here.
	if err := finance.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	if err := inventory.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	if err := hr.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	if err := lms.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	if err := crm.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	// Phase G additions — sales/procurement, bank reconciliation,
	// cost centres, and payroll live next to (not inside) the
	// finance/hr catalogs so a deployment can opt out by dropping
	// the registration calls.
	if err := sales.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	for _, kt := range ledger.BankKTypes() {
		if err := ktypeRegistry.Register(ctx, kt); err != nil {
			return fmt.Errorf("register bank ktype %s: %w", kt.Name, err)
		}
	}
	if err := ktypeRegistry.Register(ctx, ledger.CostCenterKType()); err != nil {
		return fmt.Errorf("register cost_center ktype: %w", err)
	}
	for _, kt := range hr.PayrollKTypes() {
		if err := ktypeRegistry.Register(ctx, kt); err != nil {
			return fmt.Errorf("register payroll ktype %s: %w", kt.Name, err)
		}
	}
	// Phase M shift scheduling. Registered separately from the
	// Phase E HR catalog so an existing deployment can opt out by
	// dropping these two lines without touching the older
	// hr.RegisterKTypes call.
	for _, kt := range hr.ShiftKTypes() {
		if err := ktypeRegistry.Register(ctx, kt); err != nil {
			return fmt.Errorf("register shift ktype %s: %w", kt.Name, err)
		}
	}
	// Phase I — register helpdesk KTypes. The helpdesk store manages
	// typed SLA policies + breach log while tickets themselves ride
	// the generic KRecord plumbing.
	if err := helpdesk.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}
	// Phase I — exchange-rate KType so it shows up in the KType
	// registry + records surface alongside other finance masters.
	if err := ktypeRegistry.Register(ctx, ledger.ExchangeRateKType()); err != nil {
		return fmt.Errorf("register exchange_rate ktype: %w", err)
	}

	// Phase I stores — multi-currency, helpdesk, reporting.
	// apiExchangeRates is shared with the ledger so foreign-currency
	// posting (Phase J/K) and the rate browser endpoints converge on
	// the same in-process store; aliased here for readability.
	exchangeRateStore := apiExchangeRates
	helpdeskStore := helpdesk.NewStore(pool)
	reportStore := reporting.NewStore(pool)
	reportRunner := reporting.NewRunner(pool)

	// Phase L — Insights. The query store + dashboard store back the
	// /api/v1/insights surface; the runner wraps reporting.Runner so
	// saved queries reuse the validated grammar but execute under
	// per-tenant statement_timeout + cache awareness.
	insightsQueryStore := insights.NewQueryStore(pool)
	insightsDashboardStore := insights.NewDashboardStore(pool)
	insightsCacheStore := insights.NewCacheStore(pool)
	insightsRunner := insights.NewRunner(pool, insightsCacheStore, insightsQueryStore, reportRunner)

	// Phase L deferred — external data sources, dashboard embeds. The
	// data source store encrypts connection strings with the per-
	// tenant key manager; the embed store uses an admin pool for the
	// unauth lookup path so RLS doesn't gate anonymous fetches by
	// the dashboard's owning tenant. Pool manager caps external
	// connections at DefaultMaxPools per process.
	// keyManager is a typed *KeyManager that may be nil when
	// KAPP_MASTER_KEY is unset. Gate the interface assignment on the
	// concrete-pointer check so the store's `s.enc == nil` plaintext
	// fallback fires; otherwise the typed-nil-in-interface trap
	// makes every encrypt/decrypt call return an error and breaks
	// data-source CRUD in dev environments without a master key.
	var dsEncryptor insights.Encryptor
	if keyManager != nil {
		dsEncryptor = keyManager
	}
	insightsDataSources := insights.NewDataSourceStore(pool, dsEncryptor)
	insightsPools := insights.NewPoolManager()
	defer insightsPools.Close()
	insightsExternal := insights.NewExternalRunner(insightsDataSources, insightsPools)
	insightsRunner = insightsRunner.
		WithExternal(insightsExternal).
		WithPlanGate(tenantPlanLookup{store: tenantSvc}, tenant.MaxJoinsForPlan).
		WithFeaturePolicy(featureStore)
	insightsEmbeds := insights.NewEmbedStore(pool, adminPool)

	// Agent tool executor — Phase B wires the CRM / tasks / approvals
	// tools against the same record store and workflow engine the HTTP
	// surface uses so dry-run and commit mode behave identically.
	// Phase C extends it with the finance tool suite; Phase D adds
	// inventory read + move tools.
	executor := agents.NewExecutor(recordStore, workflowEngine, auditor)
	agents.RegisterCRMTools(executor)
	agents.RegisterFinanceTools(executor, ledgerStore, invoicePoster, paymentPoster)
	agents.RegisterInventoryTools(executor, inventoryStore)
	agents.RegisterInventoryReorderTool(executor, inventory.NewReorderHandler(recordStore, inventoryStore))
	agents.RegisterHRTools(executor, hrStore)
	agents.RegisterPayrollTools(executor, hr.NewPayrollEngine(recordStore, ledgerStore).WithCountryResolver(tenantCountryResolver(tenantSvc)))
	agents.RegisterLMSTools(executor, lmsStore)
	agents.RegisterCertificateTool(executor, lms.NewCertificateIssuer(recordStore, pool))
	agents.RegisterHelpdeskTools(executor, helpdeskStore)
	agents.RegisterInsightsTools(executor, insightsQueryStore, insightsDashboardStore, insightsRunner)

	// rateLimitMW picks the Redis-backed limiter when wired, otherwise
	// falls back to the in-process limiter. Both implement the same
	// contract (Allow(tenantID, rpm, burst)) so wiring-time selection
	// keeps handler code oblivious to the backend.
	var rateLimitMW func(http.Handler) http.Handler
	if redisLimiter != nil {
		rateLimitMW = platform.RedisRateLimitMiddleware(redisLimiter)
	} else {
		rateLimitMW = platform.RateLimitMiddleware(rateLimiter)
	}
	apiCallMW := platform.APICallMiddleware(meteringBuffer)
	featureMW := platform.DynamicFeatureMiddleware(featureStore)

	fh := &formsHandlers{store: formStore, registry: ktypeRegistry}
	wizard := tenant.NewWizard(pool)
	var zkFabricClient *tenant.ZKFabricClient
	if zkClient := tenant.NewZKFabricClient(tenant.ZKFabricClientConfig{
		Endpoint:       os.Getenv("ZK_FABRIC_CONSOLE_ENDPOINT"),
		AdminToken:     os.Getenv("ZK_FABRIC_ADMIN_TOKEN"),
		BucketTemplate: os.Getenv("ZK_FABRIC_BUCKET_TEMPLATE"),
	}); zkClient != nil {
		zkFabricClient = zkClient
		wizard = wizard.WithZKFabricProvisioner(zkClient).
			WithPlacementPolicySource(tenant.NewEnvPlacementSource(
				os.Getenv("ZK_FABRIC_PROVIDERS"),
				os.Getenv("ZK_FABRIC_CACHE_HINT"),
			))
		log.Printf("api: ZK fabric tenant provisioning enabled (console=%s)", os.Getenv("ZK_FABRIC_CONSOLE_ENDPOINT"))
	}
	th := &tenantHandlers{svc: tenantSvc, wizard: wizard}
	feath := &featuresHandlers{features: featureStore, tenants: tenantSvc}
	plch := &placementHandlers{tenants: tenantSvc, fabric: zkFabricClient}
	// Phase J/K — data retention policies and the runtime isolation
	// audit report. Both surfaces require adminPool because the
	// retention sweeper bypasses RLS for cross-tenant scans and the
	// audit probes need GUC-less queries.
	var reth *retentionHandlers
	var iah *isolationAuditHandlers
	if adminPool != nil {
		retentionStore := platform.NewRetentionStore(pool, adminPool)
		reth = &retentionHandlers{store: retentionStore}
		iah = &isolationAuditHandlers{auditor: platform.NewIsolationAuditor(pool, adminPool)}
	}
	meth := &meteringHandlers{metering: meteringStore, tenants: tenantSvc, plans: planStore, features: featureStore}
	kh := &ktypeHandlers{registry: ktypeRegistry}
	rh := &recordHandlers{store: recordStore}
	sh := &searchHandlers{store: recordStore}
	webhookStore := notifications.NewWebhookStore(pool)
	whh := &webhookHandlers{store: webhookStore}
	printTemplateStore := print.NewTemplateStore(pool)
	printRenderer := print.NewRenderer(printTemplateStore, objectStore, nil)
	ph := &printHandlers{records: recordStore, renderer: printRenderer}
	portalStore := auth.NewPortalStore(pool)
	wh := &workflowHandlers{engine: workflowEngine, store: recordStore, registry: ktypeRegistry}
	ah := &agentHandlers{executor: executor}
	aph := &approvalsHandlers{engine: workflowEngine, store: recordStore}
	auh := &auditHandlers{pool: pool}
	finh := &financeHandlers{store: ledgerStore, poster: invoicePoster, payments: paymentPoster}
	invh := &inventoryHandlers{store: inventoryStore}
	oh := &openAPIHandler{registry: ktypeRegistry}
	fileh := &filesHandlers{store: filesStore, meter: meteringBuffer}
	bh := &baseHandlers{store: baseStore}
	dh := &docsHandlers{store: docsStore}
	eh := &eventsHandlers{pool: pool}
	vh := &viewHandlers{store: record.NewViewStore(pool)}
	// Phase I handlers — multi-currency, helpdesk (SLA policies),
	// reports (saved + ad-hoc), and dashboard KPI aggregation.
	curh := &currencyHandlers{store: exchangeRateStore}
	hdh := &helpdeskHandlers{store: helpdeskStore}
	reph := &reportsHandlers{store: reportStore, runner: reportRunner}
	repsh := &reportScheduleHandlers{store: reporting.NewScheduleStore(pool)}
	exph := &exportHandlers{store: exporter.NewStore(pool, adminPool)}
	dashh := &dashboardHandlers{store: dashboard.NewStore(pool).WithConverter(dashboardRateAdapter{rates: apiExchangeRates})}
	insh := &insightsHandlers{
		queries:    insightsQueryStore,
		dashboards: insightsDashboardStore,
		runner:     insightsRunner,
		features:   featureStore,
	}
	insdsh := &insightsDataSourceHandlers{
		store:    insightsDataSources,
		pools:    insightsPools,
		features: featureStore,
	}
	insembh := &insightsEmbedHandlers{
		embeds:      insightsEmbeds,
		dashboards:  insightsDashboardStore,
		queries:     insightsQueryStore,
		runner:      insightsRunner,
		features:    featureStore,
		rateLimiter: rateLimiter,
	}

	// Inbound email → ticket. Wired only when adminPool is
	// available — the resolver SELECTs against tenant_support_domains
	// outside any tenant's RLS context (admin bypass policy).
	var inboundHandler *helpdeskInboundHandlers
	if adminPool != nil {
		resolver := helpdesk.NewPGTenantResolver(adminPool)
		inboundHandler = &helpdeskInboundHandlers{
			handler: helpdesk.NewInboundEmailHandler(resolver, recordStore, helpdeskStore, phaseASystemActor),
			secret:  os.Getenv("HELPDESK_INBOUND_TOKEN"),
		}
	}
	// Phase J payroll engine — reuses the record store + ledger
	// store so posted pay_runs ride the same JE / idempotency
	// path as AR/AP.
	hrh := &hrHandlers{engine: hr.NewPayrollEngine(recordStore, ledgerStore).WithCountryResolver(tenantCountryResolver(tenantSvc))}

	// Phase H JWT auth. The signer is built from KAPP_JWT_SECRET; when
	// the secret is absent we log and skip wiring the SSO endpoints so
	// local dev that still relies on the X-Tenant-ID header keeps
	// working. The session store is tenant-scoped and is wired even
	// when SSO is off so a future boot can pick it up without a
	// restart-time schema change.
	authh := &authHandlers{}
	sessionStore := auth.NewPGSessionStore(pool)
	if adminPool != nil {
		sessionStore = sessionStore.WithQuotaLoader(func(ctx context.Context, tenantID uuid.UUID) (json.RawMessage, error) {
			t, err := tenantSvc.Get(ctx, tenantID)
			if err != nil {
				return nil, err
			}
			return t.Quota, nil
		})
	}
	if signer, err := newAuthSigner(); err == nil {
		kchat := auth.NewHTTPKChatClient(os.Getenv("KCHAT_BASE_URL"), os.Getenv("KCHAT_API_KEY"))
		authh.signer = signer
		authh.svc = auth.NewSSOService(kchat, signer, sessionStore, pool, adminPool)
		log.Printf("api: JWT auth enabled (HS256)")
	} else {
		log.Printf("api: JWT auth disabled (%v)", err)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthHandler(pool))
	r.Get("/api/v1/", rootHandler)

	// Phase H auth routes. SSO and refresh are unauthenticated (they
	// bootstrap the auth context); the rest of the surface will be
	// migrated onto the Bearer-token middleware over subsequent PRs
	// while the X-Tenant-ID header keeps working for local dev.
	r.Route("/api/v1/auth", func(r chi.Router) {
		r.Post("/sso", authh.sso)
		r.Post("/refresh", authh.refresh)
	})

	// Phase F event stream. SSE tail of the tenant's outbox so the web
	// UI can react to state changes without polling. Defined at the root
	// router so it does NOT inherit the 30s request timeout applied below
	// — chi's middleware.Timeout wraps the ResponseWriter and cancels the
	// context after the deadline, which would break any long-lived
	// stream. Idempotency/rate-limit are also skipped because SSE is a
	// GET and a spammed subscription is bounded by connection count.
	r.Route("/api/v1/events", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		r.Use(apiCallMW)
		r.Get("/stream", eh.stream)
	})

	// All non-streaming routes run under a 30s request deadline so a
	// slow handler can't hold a connection open indefinitely. The SSE
	// stream is deliberately registered above this group.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))

		// Helpdesk customer portal. Auth endpoints are unauthenticated
		// — they run the magic-link flow themselves. Ticket endpoints
		// require a portal-scoped JWT issued by /auth/verify. No
		// X-Tenant-ID header is expected on portal routes; the tenant
		// is taken from the JWT (for data routes) or the request body
		// (for auth endpoints) so external customers never have to
		// know their tenant's internal UUID.
		//
		// Registered inside the 30s timeout group so a slow or
		// malicious portal client can't hold a goroutine + DB conn
		// open indefinitely. Portal handlers are regular request /
		// response, no streaming, so the deadline is safe.
		if authh.signer != nil {
			porh := &portalHandlers{
				tenants:  tenantSvc,
				portal:   portalStore,
				signer:   authh.signer,
				records:  recordStore,
				mailer:   stdoutPortalMailer{},
				features: featureStore,
			}
			r.Route("/api/v1/portal", func(r chi.Router) {
				r.Route("/auth", func(r chi.Router) {
					// /auth/* gate inline inside the handlers — they need
					// the tenant lookup first and can't share the
					// claims-based middleware below.
					r.Post("/request", porh.requestMagicLink)
					r.Post("/verify", porh.verifyMagicLink)
				})
				r.Route("/tickets", func(r chi.Router) {
					r.Use(portalAuthMiddleware(authh.signer))
					// FeaturePortal gate sits after auth so the tenant
					// is taken from the JWT claims — standard
					// DynamicFeatureMiddleware cannot be used here
					// because the portal skips TenantMiddleware.
					r.Use(portalFeatureMiddleware(featureStore))
					// Bridge the portal claims into the platform tenant
					// + user context slots so the standard rate-limit /
					// api-call / quota / idempotency middleware below
					// runs unchanged. Without this the portal surface
					// would have no rate limiting and a stolen portal
					// JWT could create unbounded ticket replies.
					r.Use(portalTenantContextMiddleware(tenantSvc))
					r.Use(apiCallMW)
					r.Use(platform.IdempotencyMiddleware(pool))
					r.Use(rateLimitMW)
					r.Use(platform.QuotaMiddleware(quotaEnforcer))
					r.Get("/", porh.listTickets)
					r.Post("/", porh.createTicket)
					r.Get("/{id}", porh.getTicket)
					r.Post("/{id}/reply", porh.replyTicket)
				})
			})
		}

		// Control-plane tenant lifecycle routes (not tenant-scoped).
		r.Route("/api/v1/tenants", func(r chi.Router) {
			r.Get("/", th.list)
			r.Post("/", th.create)
			r.Get("/{id}", th.get)
			r.Post("/{id}/suspend", th.suspend)
			r.Post("/{id}/activate", th.activate)
			r.Post("/{id}/archive", th.archive)
			r.Delete("/{id}", th.delete)
			r.Post("/{id}/setup", th.setup)
			r.Get("/{id}/features", feath.list)
			r.Put("/{id}/features", feath.update)
			r.Get("/{id}/placement", plch.get)
			r.Put("/{id}/placement", plch.put)
			if reth != nil {
				r.Get("/{id}/retention", reth.list)
				r.Put("/{id}/retention", reth.put)
			}
			r.Get("/{id}/usage", meth.usage)
			r.Get("/{id}/usage/history", meth.usageHistory)
			r.Post("/{id}/plan", meth.changePlan)

			// /tenants/me/* — JWT-resolved tenant variants. The web
			// UI can call these without knowing its own tenant
			// uuid; the handler resolves it off the JWT claims via
			// TenantMiddleware.
			r.Route("/me", func(r chi.Router) {
				r.Use(platform.TenantMiddleware(tenantSvc))
				r.Get("/features", feath.listMe)
				r.Get("/usage", meth.usageMe)
				r.Get("/usage/history", meth.usageHistory)
				r.Post("/plan", meth.changePlanMe)
			})
		})

		// Plan definitions are shared metadata (not tenant-scoped) so
		// they live at /api/v1/plans alongside /api/v1/ktypes.
		r.Route("/api/v1/plans", func(r chi.Router) {
			r.Get("/", meth.listPlans)
		})

		// Phase J/K — runtime isolation audit. Returns the JSON
		// report from platform.IsolationAuditor.Run. Admin-only
		// in spirit; the route group is intentionally not wrapped
		// in TenantMiddleware because the audit must run with the
		// admin GUC. Operators authenticate via the same JWT
		// envelope as other admin surfaces.
		if iah != nil {
			r.Route("/api/v1/admin", func(r chi.Router) {
				r.Get("/isolation-audit", iah.get)
				// Phase G — tier upgrade endpoint. Replaces the
				// scripts/upgrade_tier.sh shell script with an
				// admin-only API call. Requires adminPool because
				// CREATE SCHEMA + cross-schema INSERT must run
				// outside any tenant-scoped RLS context.
				if adminPool != nil {
					tih := &tierUpgradeHandlers{
						tenants:   th,
						adminPool: adminPool,
						auditor:   auditor,
					}
					r.Post("/tenants/{id}/upgrade-tier", tih.upgrade)

					// Phase M Task 7 — admin-only multi-tenant
					// consolidation. The store reads each member
					// tenant's trial balance via the admin pool
					// (BYPASSRLS) so a single run can span tenants.
					rates := ledger.NewExchangeRateStore(pool)
					consStore := ledger.NewConsolidationStore(adminPool, ledgerStore, rates)
					ch := &consolidationHandlers{store: consStore}
					r.Post("/consolidation/groups", ch.createGroup)
					r.Post("/consolidation/groups/{id}/run", ch.run)
				}
			})
		}

		// KType registry routes (shared metadata, not tenant-scoped).
		r.Route("/api/v1/ktypes", func(r chi.Router) {
			r.Post("/", kh.register)
			r.Get("/", kh.list)
			r.Get("/{name}", kh.get)
		})

		// Webhook management + delivery-log surface. Gated behind
		// the per-tenant `webhook` feature flag (derived from the
		// path via DynamicFeatureMiddleware). CRUD runs under the
		// same middleware stack as other mutation routes so the
		// tenant cannot bypass idempotency / rate-limit / quota.
		r.Route("/api/v1/webhooks", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", whh.list)
			r.Post("/", whh.create)
			r.Get("/{id}", whh.get)
			r.Put("/{id}", whh.update)
			r.Delete("/{id}", whh.delete)
			r.Get("/{id}/deliveries", whh.deliveries)
		})

		// Full-text search across the krecords table. Reads are
		// tenant-scoped (RLS on krecords already covers it) so the
		// group only needs tenant + api-call middleware; idempotency
		// and quota are skipped because GET /search is a pure read.
		r.Route("/api/v1/search", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(rateLimitMW)
			r.Get("/", sh.search)
		})

		// KRecord CRUD routes. These require tenant context, rate limiting,
		// quota enforcement, and idempotency keys on mutations.
		r.Route("/api/v1/records", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			// Idempotency runs before rate-limit/quota so a replay of a
			// previously-successful mutation returns the cached response even
			// when the tenant has since hit its rate-limit or quota ceiling —
			// the replay is not a new unit of work (ARCHITECTURE.md §8 rule 6).
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Post("/{ktype}", rh.create)
			r.Get("/{ktype}", rh.list)
			// Bulk actions endpoint — multi-id status_change, delete,
			// or CSV export in one transaction. Matches the pattern
			// frappe/frappe uses on its List View: the UI collects
			// selected rows and dispatches to a single backend entry
			// point rather than looping over per-row endpoints.
			r.Post("/{ktype}/bulk", rh.bulk)
			r.Get("/{ktype}/{id}", rh.get)
			r.Patch("/{ktype}/{id}", rh.update)
			r.Delete("/{ktype}/{id}", rh.delete)
			// Print surface — HTML preview + PDF download per
			// record. Sits under /records so the tenant +
			// rate-limit middleware is inherited, but the
			// FeaturePrint flag is enforced explicitly here:
			// DynamicFeatureMiddleware keys on the URL domain
			// segment ("records") which has no per-feature
			// mapping, so the print routes would otherwise be
			// silently un-gated even when the tenant's plan has
			// FeaturePrint=false.
			r.Group(func(pr chi.Router) {
				pr.Use(platform.FeatureMiddleware(featureStore, tenant.FeaturePrint))
				pr.Get("/{ktype}/{id}/pdf", ph.pdf)
				pr.Get("/{ktype}/{id}/html", ph.html)
			})
			// Workflow action endpoint (ARCHITECTURE.md §10). Runs under the
			// same tenant + idempotency + rate-limit + quota stack as record
			// CRUD so a spammed transition can't starve other tenants.
			r.Post("/{ktype}/{id}/actions/{action}", wh.action)
			// Workflow-run read endpoint. The list/kanban UI hydrates the
			// RightPane from this so it can show the authoritative state
			// the engine holds rather than inferring it from the record
			// data field (ARCHITECTURE.md §7).
			r.Get("/{ktype}/{id}/workflow-run", wh.getRunByRecord)
		})

		// Agent tool invocation surface. ARCHITECTURE.md §10-§11 requires
		// every mutation to be tenant-scoped and attributable, so mutating
		// calls run under the same middleware stack as record CRUD. The
		// read-only list endpoint lives in the same route group for
		// discoverability even though it does not need idempotency.
		r.Route("/api/v1/agents", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/tools", ah.list)
			r.Post("/tools/{name}", ah.invoke)
		})

		// Approvals surface. GET endpoints are safe to replay under
		// IdempotencyMiddleware (the middleware short-circuits non-mutating
		// methods) and the mutations (POST /, POST /{id}/decide) need the
		// same tenant + idempotency + rate-limit + quota stack as record
		// CRUD so a spammed approve / reject can't starve other tenants.
		r.Route("/api/v1/approvals", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", aph.list)
			r.Post("/", aph.create)
			r.Get("/{id}", aph.get)
			r.Post("/{id}/decide", aph.decide)
		})

		// Audit log read surface. Queries the audit_log table under tenant
		// context via dbutil.WithTenantTx so RLS is enforced. Admin-only in
		// production; auth enforcement lands with the broader auth layer
		// in Phase C.
		r.Route("/api/v1/audit", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Get("/", auh.list)
			r.Get("/verify", auh.verify)
		})

		// Finance surface (Phase C). Chart of accounts, journal entries,
		// invoice/bill posting, period lockout, and reports. Mutations
		// need the full tenant + idempotency + rate-limit + quota stack
		// because a spammed post can't be allowed to starve other tenants
		// or double-post an invoice under replay.
		r.Route("/api/v1/finance", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Post("/accounts", finh.createAccount)
			r.Get("/accounts", finh.listAccounts)
			r.Get("/accounts/{code}", finh.getAccount)
			r.Post("/journal-entries", finh.postJournalEntry)
			r.Get("/journal-entries", finh.listJournalEntries)
			r.Get("/journal-entries/{id}", finh.getJournalEntry)
			r.Post("/invoices/{id}/post", finh.postInvoice)
			r.Post("/bills/{id}/post", finh.postBill)
			r.Post("/credit-notes/{id}/post", finh.postCreditNote)
			r.Post("/debit-notes/{id}/post", finh.postDebitNote)
			r.Post("/payments/{id}/post", finh.postPayment)
			r.Post("/tax-codes", finh.upsertTaxCode)
			r.Get("/tax-codes", finh.listTaxCodes)
			r.Get("/tax-codes/{code}", finh.getTaxCode)
			r.Post("/periods/lock", finh.lockPeriod)
			r.Get("/reports/trial-balance", finh.trialBalance)
			r.Get("/reports/ar-aging", finh.arAging)
			r.Get("/reports/ap-aging", finh.apAging)
			r.Get("/reports/income-statement", finh.incomeStatement)
			// Phase I — exchange rate CRUD + ad-hoc convert + unrealized
			// gain/loss calculator. Lookups do not mutate so they skip
			// the idempotency key requirement enforced by the middleware.
			r.Post("/exchange-rates", curh.upsertRate)
			r.Get("/exchange-rates", curh.listRates)
			r.Get("/exchange-rates/convert", curh.convert)
			r.Post("/exchange-rates/unrealized", curh.unrealizedGL)
		})

		// Phase J payroll surface — generate draft payslips for a
		// pay_run and post the approved batch as a single journal
		// entry. The pay_run / payslip KRecords themselves ride the
		// generic CRUD at /api/v1/records/hr.pay_run and hr.payslip.
		r.Route("/api/v1/hr", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Post("/pay-runs/{id}/generate", hrh.generatePayslips)
			r.Post("/pay-runs/{id}/post", hrh.postPayRun)
			r.Get("/pay-runs/{id}/payslips", hrh.listPayRunPayslips)
		})

		// Phase I helpdesk surface. Tickets themselves ride the generic
		// KRecord CRUD at /api/v1/records/helpdesk.ticket; these routes
		// back the SLA policy list/upsert the UI needs when authoring
		// policies and the per-ticket SLA log the right pane renders.
		r.Route("/api/v1/helpdesk", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Post("/sla-policies", hdh.upsertPolicy)
			r.Get("/sla-policies", hdh.listPolicies)
			r.Get("/sla-policies/resolve", hdh.resolvePolicy)
			r.Get("/tickets/{id}/sla-log", hdh.ticketLog)
		})

		// Inbound email → ticket. Sits OUTSIDE the JWT-tenant
		// middleware because the relay does not carry session
		// credentials; instead we authenticate by static shared
		// secret and resolve the tenant from the recipient host.
		// Rate limited per-IP via the shared rate limiter so a
		// flood of inbound mail cannot starve other writers.
		if inboundHandler != nil {
			r.Route("/api/v1/helpdesk/inbound-email", func(r chi.Router) {
				r.Use(rateLimitMW)
				r.Post("/", inboundHandler.post)
			})
		}

		// Phase I reports surface. Saved report CRUD + ad-hoc execution
		// under the same tenant/idempotency/rate-limit/quota stack so
		// spammed runs cannot starve other tenants.
		r.Route("/api/v1/reports", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", reph.list)
			r.Post("/", reph.create)
			r.Post("/run", reph.runAdhoc)
			r.Get("/{id}", reph.get)
			r.Put("/{id}", reph.update)
			r.Delete("/{id}", reph.delete)
			r.Get("/{id}/run", reph.runSaved)
			r.Patch("/{id}/share", reph.share)
		})

		// Phase K — data export queue. Submission enqueues; the
		// worker (services/worker/export_worker.go) drains it and
		// streams payload via /download.
		r.Route("/api/v1/exports", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", exph.list)
			r.Post("/", exph.create)
			r.Get("/{id}", exph.get)
			r.Get("/{id}/download", exph.download)
		})

		// Phase K — report schedules. CRUD only; the worker owns
		// dispatch via reporting.ActionTypeReportSchedule.
		r.Route("/api/v1/report-schedules", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", repsh.list)
			r.Post("/", repsh.create)
			r.Get("/{id}", repsh.get)
			r.Put("/{id}", repsh.update)
			r.Delete("/{id}", repsh.delete)
		})

		// Phase L Insights. CRUD for saved queries + dashboards,
		// cache-aware query execution under per-tenant
		// statement_timeout, dashboard widget upsert/delete, and
		// role/user share grants. Gated on the `insights`
		// feature flag via DynamicFeatureMiddleware so a free /
		// starter plan can't reach the surface even with a
		// stolen tenant header.
		r.Route("/api/v1/insights", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))

			r.Route("/queries", func(r chi.Router) {
				r.Get("/", insh.listQueries)
				r.Post("/", insh.createQuery)
				r.Get("/{id}", insh.getQuery)
				r.Put("/{id}", insh.updateQuery)
				r.Delete("/{id}", insh.deleteQuery)
				r.Post("/{id}/run", insh.runQuery)
				// Raw-SQL editor mode (Phase M). Gated by an
				// additional `insights_sql_editor` feature flag
				// on top of the parent `insights` gate so a non-
				// enterprise plan with a stolen tenant header
				// can't reach the surface even with `insights`
				// turned on.
				r.Group(func(r chi.Router) {
					r.Use(platform.FeatureMiddleware(featureStore, tenant.FeatureInsightsSQLEditor))
					r.Post("/{id}/run-sql", insh.runRawSQL)
				})
				r.Post("/{id}/share", insh.shareQuery)
				r.Get("/{id}/shares", insh.listQueryShares)
				r.Delete("/{id}/shares/{shareID}", insh.deleteQueryShare)
			})
			r.Route("/dashboards", func(r chi.Router) {
				r.Get("/", insh.listDashboards)
				r.Post("/", insh.createDashboard)
				r.Get("/{id}", insh.getDashboard)
				r.Put("/{id}", insh.updateDashboard)
				r.Delete("/{id}", insh.deleteDashboard)
				r.Post("/{id}/share", insh.shareDashboard)
				r.Get("/{id}/shares", insh.listDashboardShares)
				r.Delete("/{id}/shares/{shareID}", insh.deleteDashboardShare)
				r.Post("/{id}/widgets", insh.upsertWidget)
				r.Delete("/{id}/widgets/{widgetID}", insh.deleteWidget)
				// Embed-token CRUD on a per-dashboard collection.
				// Auth-gated; the public unauth lookup lives at
				// /api/v1/insights/embed/{token} (mounted below).
				r.Get("/{id}/embeds", insembh.list)
				r.Post("/{id}/embeds", insembh.create)
				r.Post("/{id}/embeds/{embed_id}/revoke", insembh.revoke)
			})
			// External data sources (Phase L deferred). Connection
			// strings are encrypted at rest; the test endpoint
			// pings the remote with SELECT 1 so the UI can
			// distinguish a typo from a credential failure.
			r.Route("/data-sources", func(r chi.Router) {
				r.Get("/", insdsh.list)
				r.Post("/", insdsh.create)
				r.Put("/{id}", insdsh.update)
				r.Delete("/{id}", insdsh.delete)
				r.Post("/{id}/test", insdsh.test)
			})
		})

		// Public unauth dashboard embed endpoint. Mounted outside
		// the auth chain so anonymous viewers can fetch a
		// pre-rendered dashboard via a long-lived bearer token.
		// Rate-limit middleware here uses a per-IP fallback; the
		// handler itself bills the owning tenant's bucket so a
		// viral embed can't starve other tenants.
		r.Route("/api/v1/insights/embed", func(r chi.Router) {
			r.Use(rateLimitMW)
			r.Get("/{token}", insembh.public)
		})

		// Phase I KPI dashboard aggregation. Reads only, so no idempotency
		// needed — quota + rate-limit keep it in bounds.
		r.Route("/api/v1/dashboard", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/summary", dashh.summary)
		})

		// Inventory surface (Phase D). Item + warehouse masters, the
		// append-only stock-move ledger, the stock_levels view, and the
		// valuation report. Mutations run under the same tenant +
		// idempotency + rate-limit + quota stack as finance because a
		// spammed move post can't starve other tenants or double-post a
		// source-record move under replay (the partial unique index on
		// inventory_moves handles that at the DB layer).
		r.Route("/api/v1/inventory", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Post("/items", invh.upsertItem)
			r.Get("/items", invh.listItems)
			r.Get("/items/{id}", invh.getItem)
			r.Post("/warehouses", invh.upsertWarehouse)
			r.Get("/warehouses", invh.listWarehouses)
			r.Post("/moves", invh.recordMove)
			r.Get("/moves", invh.listMoves)
			r.Post("/moves/{id}/reverse", invh.reverseMove)
			r.Post("/transfers", invh.recordTransfer)
			r.Get("/stock-levels", invh.listStockLevels)
			r.Get("/stock-levels/{id}", invh.stockLevelsByItem)
			r.Post("/batches", invh.createBatch)
			r.Get("/items/{id}/batches", invh.listBatchesByItem)
			r.Get("/reports/valuation", invh.valuation)
		})

		// Forms KApp. Creation and tenant-scoped lookups go through the
		// tenant middleware; public read + submit explicitly do NOT so
		// anonymous submissions work. Public-submit rate limiting is the
		// reverse-proxy's job since there is no X-Tenant-ID header to key
		// on (ARCHITECTURE.md §12).
		r.Route("/api/v1/forms", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(platform.TenantMiddleware(tenantSvc))
				r.Use(apiCallMW)
				r.Use(featureMW)
				r.Post("/", fh.create)
			})
			r.Get("/{id}", fh.public)
			r.Post("/{id}/submit", fh.submit)
		})

		// Phase F file attachments. Uploads run under the full tenant +
		// idempotency + rate-limit + quota stack so a spammed upload cannot
		// starve other tenants; the object store dedups by SHA-256 so
		// rehosting the same source attachment across tenants costs one
		// physical blob.
		r.Route("/api/v1/files", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Post("/", fileh.upload)
			r.Get("/{id}", fileh.get)
			r.Get("/{id}/content", fileh.download)
		})

		// Phase F Base KApp — ad-hoc tables per tenant. Same middleware
		// stack as records: a tenant can't starve another via spammed
		// row inserts, and RLS stops cross-tenant row reads even if a
		// URL is forged.
		r.Route("/api/v1/base", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/tables", bh.listTables)
			r.Post("/tables", bh.createTable)
			r.Get("/tables/{id}", bh.getTable)
			r.Patch("/tables/{id}", bh.updateTable)
			r.Get("/tables/{id}/rows", bh.listRows)
			r.Post("/tables/{id}/rows", bh.createRow)
			r.Patch("/tables/{id}/rows/{rowID}", bh.updateRow)
			r.Delete("/tables/{id}/rows/{rowID}", bh.deleteRow)
		})

		// Phase F Docs KApp — artifact documents with append-only version
		// history. SaveVersion and Restore each write a new history row
		// under tenant context; the immutable history table has no UPDATE
		// or DELETE policy so an audit replay always reproduces the edit
		// timeline.
		r.Route("/api/v1/docs", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", dh.list)
			r.Post("/", dh.create)
			r.Get("/{id}", dh.get)
			r.Post("/{id}/versions", dh.saveVersion)
			r.Get("/{id}/versions", dh.versions)
			r.Post("/{id}/restore", dh.restore)
		})

		// Phase H notifications inbox — durable in-app bell/inbox surface
		// backed by the notifications table. External transports (KChat,
		// webhook, email) are served by the worker; this endpoint backs
		// the web inbox regardless of transport success.
		notifStore := notifications.NewStore(pool)
		nh := newNotificationsHandlers(notifStore)
		r.Route("/api/v1/notifications", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(rateLimitMW)
			r.Get("/", nh.list)
			r.Post("/{id}/read", nh.markRead)
			r.Post("/read-all", nh.markAllRead)
		})

		// Phase G saved views — per-user, per-KType filter/sort/column
		// layouts the RecordListPage persists so operators resume their
		// curated worklist across sessions. Mutations run under the same
		// idempotency + rate-limit + quota stack as record CRUD so a
		// spammed save cannot starve other tenants. RLS on saved_views
		// enforces tenant isolation; owner-only rules live in the store.
		r.Route("/api/v1/views", func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Use(apiCallMW)
			r.Use(featureMW)
			r.Use(platform.IdempotencyMiddleware(pool))
			r.Use(rateLimitMW)
			r.Use(platform.QuotaMiddleware(quotaEnforcer))
			r.Get("/", vh.list)
			r.Post("/", vh.create)
			r.Get("/{id}", vh.get)
			r.Patch("/{id}", vh.update)
			r.Delete("/{id}", vh.delete)
		})

		// OpenAPI machine-readable schema served for API consumers.
		r.Get("/api/v1/openapi.json", oh.serve)
	}) // end timeout-guarded group

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api: listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("api: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	log.Printf("api: shutdown complete")
	return nil
}

func healthHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func rootHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// tenantCountryResolver adapts *tenant.PGStore to the
// hr.CountryResolver shape so the payroll engine can fetch a
// tenant's ISO 3166-1 alpha-2 country code without importing the
// tenant package directly. Lookup failures collapse to "" + nil
// because the engine treats both as "no statutory pack" and we'd
// rather fail-soft a slip than block payroll on a control-plane
// hiccup.
func tenantCountryResolver(svc *tenant.PGStore) hr.CountryResolver {
	if svc == nil {
		return nil
	}
	return func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		t, err := svc.Get(ctx, tenantID)
		if err != nil {
			return "", nil
		}
		return t.Country, nil
	}
}
