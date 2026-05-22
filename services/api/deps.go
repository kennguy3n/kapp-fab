package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/authz"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// apiDeps bundles every dependency the API router touches into a
// single value so the route registration function does not need a
// 70-parameter signature. Construction lives in main.go inside
// run() — populating this struct is the very last step before
// handing it to registerRoutes().
//
// Why a single bag instead of finer-grained groups? The route file
// references ~60 distinct values and many handlers depend on a
// shared sub-set (tenant + pool + audit + recordStore). Splitting
// into "db", "handlers", "middleware" sub-structs would push the
// duplication into the route file's call sites and obscure which
// fields a given route actually uses. The cost of a flat bag is
// the verbose struct literal; the cost of grouping would be
// touching every route registration when a sub-group gains a
// field.
type apiDeps struct {
	// Configuration loaded once at process start. Routes consume
	// only the SMTP fields directly; the rest is referenced by
	// handlers via their own stores.
	cfg *platform.Config

	// Database pools. `pool` is the standard tenant-scoped pool
	// (RLS GUC `app.tenant_id` is set per request); `adminPool` is
	// the BYPASSRLS pool used by admin-only routes that have to
	// scan across tenants (isolation audit, retention, exports).
	// `adminPool` may be nil in dev — handlers that require it are
	// either skipped during wiring or fail at request time with a
	// 503.
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool

	// Core platform services. tenantSvc is the only tenant store;
	// featureStore, quotaEnforcer, portalStore each back specific
	// route groups (see middleware composition below).
	tenantSvc     *tenant.PGStore
	featureStore  *tenant.FeatureStore
	quotaEnforcer *platform.QuotaEnforcer
	portalStore   *auth.PortalStore

	// Domain stores shared across multiple route groups.
	recordStore      *record.PGStore
	ledgerStore      *ledger.PGStore
	invoicePoster    *ledger.InvoicePoster
	paymentPoster    *ledger.PaymentPoster
	apiExchangeRates *ledger.ExchangeRateStore

	// Authz + audit. `authzEval` is the live PGEvaluator; the gate
	// closures below wrap it with the `authzEnforced` flag so
	// disabling enforcement collapses to no-ops without scattering
	// `if authzEnforced` branches across every route group.
	authzEval *authz.PGEvaluator
	auditor   *audit.PGLogger

	// Reusable middleware closures. `rateLimitMW` switches between
	// the Redis-backed and in-process backends at construction time
	// so handler code is oblivious to which is live; the others
	// wrap shared infrastructure (metering, feature flags, IP-keyed
	// token bucket) in chi-friendly shape.
	rateLimitMW       func(http.Handler) http.Handler
	apiCallMW         func(http.Handler) http.Handler
	featureMW         func(http.Handler) http.Handler
	authzGate         func(action, resource string) func(http.Handler) http.Handler
	authzMethodGate   func(readAction, writeAction, resource string) func(http.Handler) http.Handler
	publicFormIPLimit func(http.Handler) http.Handler

	// adminChain mounts the JWT + IsPlatformAdmin gate on a chi
	// sub-router. Defined as a closure (not a middleware) because
	// it has to attach two middlewares in order and short-circuits
	// with a 503 when the JWT signer is not configured — see the
	// extensive coupling note in routes.go where it's first used.
	adminChain func(r chi.Router)

	// userChain mounts the JWT-only gate (auth.Middleware +
	// auth.RequireActiveHomeTenant) on a chi sub-router. Used by
	// /api/v1/tenants/me — which before Phase 1 read the tenant
	// from the X-Tenant-ID header via platform.TenantMiddleware,
	// giving any unauthenticated client a privilege-escalation
	// path on POST /me/plan. Same 503 fallback shape as adminChain
	// when the JWT signer is not configured.
	userChain func(r chi.Router)

	// HTTP handlers, one per major route group. Each handler is a
	// pointer struct that carries its own dependencies; this slice
	// is the single registry the router walks.
	authh          *authHandlers
	eh             *eventsHandlers
	th             *tenantHandlers
	feath          *featuresHandlers
	plch           *placementHandlers
	reth           *retentionHandlers
	iah            *isolationAuditHandlers
	meth           *meteringHandlers
	kh             *ktypeHandlers
	whh            *webhookHandlers
	sh             *searchHandlers
	rh             *recordHandlers
	ph             *printHandlers
	fh             *formsHandlers
	wh             *workflowHandlers
	ah             *agentHandlers
	aph            *approvalsHandlers
	auh            *auditHandlers
	finh           *financeHandlers
	invh           *inventoryHandlers
	oh             *openAPIHandler
	fileh          *filesHandlers
	bh             *baseHandlers
	dh             *docsHandlers
	vh             *viewHandlers
	roleh          *rolesHandlers
	curh           *currencyHandlers
	hdh            *helpdeskHandlers
	reph           *reportsHandlers
	repsh          *reportScheduleHandlers
	exph           *exportHandlers
	dashh          *dashboardHandlers
	insh           *insightsHandlers
	insdsh         *insightsDataSourceHandlers
	insembh        *insightsEmbedHandlers
	hrh            *hrHandlers
	inboundHandler *helpdeskInboundHandlers
}
