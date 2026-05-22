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
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/authz"
	"github.com/kennguy3n/kapp-fab/internal/base"
	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/docs"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/exporter"
	"github.com/kennguy3n/kapp-fab/internal/files"
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
	// Authorization evaluator. The cache TTL is intentionally short
	// (30s) so role/permission changes propagate quickly; the role
	// management API (rolesHandlers) also flushes the relevant
	// entries explicitly on every mutation.
	authzCache := platform.NewLRUCache(512, 30*time.Second)
	authzEval := authz.NewPGEvaluator(pool, authzCache)
	// Authorization gating is ENABLED by default. Set
	// KAPP_AUTHZ_ENFORCE=0 (or "false") to explicitly opt out — useful
	// only for local development against pre-JWT clients that still
	// authenticate via X-Tenant-ID alone. Production deployments
	// should NEVER disable this; opting out is logged at WARN so the
	// misconfiguration is visible in operator dashboards.
	//
	// When enforcement is on, the gate runs authz.Middleware (or
	// MethodMiddleware) against every gated route. Routes that always
	// require authorization (e.g. /api/v1/roles) mount
	// authz.Middleware directly and are not affected by this gate.
	authzDisabled := os.Getenv("KAPP_AUTHZ_ENFORCE") == "0" || strings.EqualFold(os.Getenv("KAPP_AUTHZ_ENFORCE"), "false")
	authzEnforced := !authzDisabled
	authzGate := func(action, resource string) func(http.Handler) http.Handler {
		if !authzEnforced {
			return func(next http.Handler) http.Handler { return next }
		}
		return authz.Middleware(authzEval, action, resource)
	}
	authzMethodGate := func(readAction, writeAction, resource string) func(http.Handler) http.Handler {
		if !authzEnforced {
			return func(next http.Handler) http.Handler { return next }
		}
		return authz.MethodMiddleware(authzEval, readAction, writeAction, resource)
	}
	if authzEnforced {
		log.Printf("api: authz enforcement ENABLED (default)")
	} else {
		log.Printf("api: WARN authz enforcement DISABLED (KAPP_AUTHZ_ENFORCE=%q) — every gated route is wide open; do NOT run this in production", os.Getenv("KAPP_AUTHZ_ENFORCE"))
	}
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
	var ipRedisLimiter *platform.RedisIPRateLimiter
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		rl, err := platform.NewRedisRateLimiter(ctx, redisURL, rateLimitCfg)
		if err != nil {
			log.Printf("api: redis rate limiter init failed, falling back to in-process: %v", err)
		} else {
			redisLimiter = rl
			defer func() { _ = redisLimiter.Close() }()
			log.Printf("api: distributed rate limiter enabled (redis)")
		}
		ipRL, err := platform.NewRedisIPRateLimiter(ctx, redisURL)
		if err != nil {
			log.Printf("api: redis ip rate limiter init failed, falling back to in-process: %v", err)
		} else {
			ipRedisLimiter = ipRL
			defer func() { _ = ipRedisLimiter.Close() }()
			log.Printf("api: distributed ip rate limiter enabled (redis)")
		}
	}
	// IP-keyed rate limiter for public, un-authenticated routes
	// (e.g. POST /api/v1/forms/{id}/submit). Production replicas
	// share the bucket via Redis when REDIS_URL is set; otherwise
	// each pod enforces independently, which is still useful as a
	// per-pod abuse cap.
	//
	// When falling back to the in-process limiter we launch a
	// background sweeper bound to the run() context so a
	// distributed bot attack with millions of unique source IPs
	// cannot accumulate stale bucket entries indefinitely. Redis
	// handles the same problem natively via per-key EXPIRE.
	var ipRateBackend platform.IPRateLimiterBackend
	if ipRedisLimiter != nil {
		ipRateBackend = ipRedisLimiter
	} else {
		inProc := platform.NewInProcIPRateLimiter()
		go inProc.RunSweeper(ctx, platform.DefaultIPSweepInterval)
		ipRateBackend = inProc
	}
	publicFormIPLimit := platform.IPRateLimitMiddleware(ipRateBackend, 10, 10)
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

	// Domain KTypes are upserted at boot so a fresh deployment has a
	// working schema set without requiring an out-of-band
	// migration. See ktype_boot.go for the per-domain wiring; this
	// call is a single seam so future domains land in one file.
	if err := registerBootKTypes(ctx, ktypeRegistry); err != nil {
		return err
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
	agents.RegisterProjectTools(executor)
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
	// recordHandlers calls AuthorizeRecord from update()/delete() to
	// enforce per-record conditions like owner_only. The handler
	// guards the call with `h.eval != nil`, so leave eval unset when
	// authz enforcement is off — otherwise actorOrDefault returns
	// phaseASystemActor (a non-Nil UUID with no role rows in
	// user_tenant_roles), the evaluator finds zero permissions, and
	// every PATCH/DELETE on records 403s in dev/test environments
	// that have not yet wired JWT auth.
	var recordEval authz.Evaluator
	if authzEnforced {
		recordEval = authzEval
	}
	rh := &recordHandlers{store: recordStore, eval: recordEval}
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
	roleh := &rolesHandlers{pool: pool, eval: authzEval}
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

	// adminChain wraps a chi router with the JWT + IsPlatformAdmin
	// gate. Control-plane endpoints (tenant CRUD, /api/v1/admin/*,
	// POST /api/v1/ktypes) call this so an unauthenticated client
	// cannot suspend / archive / delete tenants. When the JWT signer
	// is not configured the chain mounts a single middleware that
	// returns 503 — refusing to register the routes would surface as
	// a confusing 404, and silently allowing them through would
	// reintroduce the very vulnerability this chain exists to close.
	//
	// CONTEXT TENANT IS SCRUBBED AFTER ADMIN MIDDLEWARE. auth.Middleware
	// stamps platform.WithTenant(ctx, t) using the JWT's `tid`
	// claim (the admin's home tenant) so ordinary tenant-scoped
	// routes can call platform.TenantFromContext. Control-plane
	// routes operate on a DIFFERENT tenant — the one named in the
	// URL (e.g. /api/v1/tenants/{id}/suspend). If a handler mounted
	// here ever fell back to TenantFromContext (either intentionally
	// or by absent-minded reuse of a shared helper), it would
	// silently scope the operation to the admin's own tenant
	// instead of the URL target — a cross-tenant correctness bug
	// that RLS would not surface because the admin's row IS
	// visible to itself.
	//
	// auth.AdminMiddleware (the second middleware in the chain
	// below) calls platform.ClearTenant on the way through, so any
	// admin handler that calls platform.TenantFromContext gets nil
	// and must handle that branch explicitly. The current handlers
	// (tenantsHandlers, isolationAuditHandlers, ktypeHandlers.register)
	// all resolve their target from chi.URLParam / the request body
	// and call tenant.PGStore directly with an explicit tenant ID,
	// so the scrub is invisible to them. The admin's home tenant
	// remains recoverable from the JWT claims (auth.ClaimsFromContext)
	// if a future handler genuinely needs it.
	//
	// When adding a handler here: resolve the target tenant
	// explicitly from chi.URLParam (or the request body) and pass
	// it down the call stack as a uuid.UUID. Calling
	// platform.TenantFromContext under adminChain is by design a
	// nil return.
	adminChain := func(r chi.Router) {
		if authh.signer == nil {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "admin routes require JWT auth; set KAPP_JWT_SECRET", http.StatusServiceUnavailable)
				})
			})
			return
		}
		r.Use(auth.Middleware(authh.signer, tenantSvc, sessionStore))
		r.Use(auth.AdminMiddleware())
	}

	// userChain is the JWT-only counterpart to adminChain. Used by
	// /api/v1/tenants/me to derive the tenant from claims.TenantID
	// instead of the X-Tenant-ID header that platform.TenantMiddleware
	// honored before Phase 1. RequireActiveHomeTenant refuses requests
	// admitted via the platform-admin recovery bypass so a recovering
	// admin cannot also mutate tenant-scoped data via /me. See the
	// coupling note in routes.go where it is mounted.
	userChain := func(r chi.Router) {
		if authh.signer == nil {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "user routes require JWT auth; set KAPP_JWT_SECRET", http.StatusServiceUnavailable)
				})
			})
			return
		}
		r.Use(auth.Middleware(authh.signer, tenantSvc, sessionStore))
		r.Use(auth.RequireActiveHomeTenant())
	}

	d := &apiDeps{
		cfg:               cfg,
		pool:              pool,
		adminPool:         adminPool,
		tenantSvc:         tenantSvc,
		featureStore:      featureStore,
		quotaEnforcer:     quotaEnforcer,
		portalStore:       portalStore,
		recordStore:       recordStore,
		ledgerStore:       ledgerStore,
		invoicePoster:     invoicePoster,
		paymentPoster:     paymentPoster,
		apiExchangeRates:  apiExchangeRates,
		authzEval:         authzEval,
		auditor:           auditor,
		rateLimitMW:       rateLimitMW,
		apiCallMW:         apiCallMW,
		featureMW:         featureMW,
		authzGate:         authzGate,
		authzMethodGate:   authzMethodGate,
		publicFormIPLimit: publicFormIPLimit,
		adminChain:        adminChain,
		userChain:         userChain,
		authh:             authh,
		eh:                eh,
		th:                th,
		feath:             feath,
		plch:              plch,
		reth:              reth,
		iah:               iah,
		meth:              meth,
		kh:                kh,
		whh:               whh,
		sh:                sh,
		rh:                rh,
		ph:                ph,
		fh:                fh,
		wh:                wh,
		ah:                ah,
		aph:               aph,
		auh:               auh,
		finh:              finh,
		invh:              invh,
		oh:                oh,
		fileh:             fileh,
		bh:                bh,
		dh:                dh,
		vh:                vh,
		roleh:             roleh,
		curh:              curh,
		hdh:               hdh,
		reph:              reph,
		repsh:             repsh,
		exph:              exph,
		dashh:             dashh,
		insh:              insh,
		insdsh:            insdsh,
		insembh:           insembh,
		hrh:               hrh,
		inboundHandler:    inboundHandler,
	}
	r := registerRoutes(d)

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
