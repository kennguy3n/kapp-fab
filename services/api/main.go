// Command api is the Kapp HTTP gateway / BFF. It exposes REST endpoints for
// KType and KRecord operations, health probes, and (future) event streaming.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/forms"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
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
	workflowEngine := workflow.NewEngine(pool, eventPublisher, auditor)
	formStore := forms.NewStore(pool, ktypeRegistry, recordStore)
	if adminPool != nil {
		formStore = formStore.WithAdminPool(adminPool)
	}
	rateLimiter := platform.NewRateLimiter(platform.DefaultRateLimitConfig())
	quotaEnforcer := platform.NewQuotaEnforcer(pool)

	// Phase C finance engine — ledger store + invoice poster share
	// the same event publisher + audit logger so journal postings,
	// invoice lifecycle events, and KRecord mutations all emit into
	// the single outbox + audit tables used by the rest of the kernel.
	ledgerStore := ledger.NewPGStore(pool, eventPublisher, auditor)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, recordStore)

	// Register the finance KTypes at boot so a fresh deployment has a
	// working chart-of-accounts / journal-entry / AR / AP schema set
	// without requiring an out-of-band migration. The registry upserts
	// on conflict so repeated restarts are a no-op.
	if err := finance.RegisterKTypes(ctx, ktypeRegistry); err != nil {
		return err
	}

	// Agent tool executor — Phase B wires the CRM / tasks / approvals
	// tools against the same record store and workflow engine the HTTP
	// surface uses so dry-run and commit mode behave identically.
	// Phase C extends it with the finance tool suite.
	executor := agents.NewExecutor(recordStore, workflowEngine, auditor)
	agents.RegisterCRMTools(executor)
	agents.RegisterFinanceTools(executor, ledgerStore, invoicePoster)

	fh := &formsHandlers{store: formStore, registry: ktypeRegistry}
	th := &tenantHandlers{svc: tenantSvc}
	kh := &ktypeHandlers{registry: ktypeRegistry}
	rh := &recordHandlers{store: recordStore}
	wh := &workflowHandlers{engine: workflowEngine, store: recordStore, registry: ktypeRegistry}
	ah := &agentHandlers{executor: executor}
	aph := &approvalsHandlers{engine: workflowEngine, store: recordStore}
	auh := &auditHandlers{pool: pool}
	finh := &financeHandlers{store: ledgerStore, poster: invoicePoster}
	oh := &openAPIHandler{registry: ktypeRegistry}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", healthHandler(pool))
	r.Get("/api/v1/", rootHandler)

	// Control-plane tenant lifecycle routes (not tenant-scoped).
	r.Route("/api/v1/tenants", func(r chi.Router) {
		r.Get("/", th.list)
		r.Post("/", th.create)
		r.Get("/{id}", th.get)
		r.Post("/{id}/suspend", th.suspend)
		r.Post("/{id}/activate", th.activate)
		r.Post("/{id}/archive", th.archive)
		r.Delete("/{id}", th.delete)
	})

	// KType registry routes (shared metadata, not tenant-scoped).
	r.Route("/api/v1/ktypes", func(r chi.Router) {
		r.Post("/", kh.register)
		r.Get("/", kh.list)
		r.Get("/{name}", kh.get)
	})

	// KRecord CRUD routes. These require tenant context, rate limiting,
	// quota enforcement, and idempotency keys on mutations.
	r.Route("/api/v1/records", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		// Idempotency runs before rate-limit/quota so a replay of a
		// previously-successful mutation returns the cached response even
		// when the tenant has since hit its rate-limit or quota ceiling —
		// the replay is not a new unit of work (ARCHITECTURE.md §8 rule 6).
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
		r.Use(platform.QuotaMiddleware(quotaEnforcer))
		r.Post("/{ktype}", rh.create)
		r.Get("/{ktype}", rh.list)
		r.Get("/{ktype}/{id}", rh.get)
		r.Patch("/{ktype}/{id}", rh.update)
		r.Delete("/{ktype}/{id}", rh.delete)
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
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
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
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
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
		r.Get("/", auh.list)
	})

	// Finance surface (Phase C). Chart of accounts, journal entries,
	// invoice/bill posting, period lockout, and reports. Mutations
	// need the full tenant + idempotency + rate-limit + quota stack
	// because a spammed post can't be allowed to starve other tenants
	// or double-post an invoice under replay.
	r.Route("/api/v1/finance", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
		r.Use(platform.QuotaMiddleware(quotaEnforcer))
		r.Post("/accounts", finh.createAccount)
		r.Get("/accounts", finh.listAccounts)
		r.Get("/accounts/{code}", finh.getAccount)
		r.Post("/journal-entries", finh.postJournalEntry)
		r.Get("/journal-entries", finh.listJournalEntries)
		r.Get("/journal-entries/{id}", finh.getJournalEntry)
		r.Post("/invoices/{id}/post", finh.postInvoice)
		r.Post("/bills/{id}/post", finh.postBill)
		r.Post("/periods/lock", finh.lockPeriod)
		r.Get("/reports/trial-balance", finh.trialBalance)
		r.Get("/reports/ar-aging", finh.arAging)
		r.Get("/reports/ap-aging", finh.apAging)
		r.Get("/reports/income-statement", finh.incomeStatement)
	})

	// Forms KApp. Creation and tenant-scoped lookups go through the
	// tenant middleware; public read + submit explicitly do NOT so
	// anonymous submissions work. Public-submit rate limiting is the
	// reverse-proxy's job since there is no X-Tenant-ID header to key
	// on (ARCHITECTURE.md §12).
	r.Route("/api/v1/forms", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(platform.TenantMiddleware(tenantSvc))
			r.Post("/", fh.create)
		})
		r.Get("/{id}", fh.public)
		r.Post("/{id}/submit", fh.submit)
	})

	// OpenAPI machine-readable schema served for API consumers.
	r.Get("/api/v1/openapi.json", oh.serve)

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
