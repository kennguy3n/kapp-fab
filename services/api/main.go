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
	"github.com/kennguy3n/kapp-fab/internal/base"
	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/docs"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/forms"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
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
	// Per-tenant field-level encryption is opt-in: when KAPP_MASTER_KEY
	// is set, derive per-tenant keys and plug the KeyManager into the
	// record store so schema fields marked {"encrypted": true} round-trip
	// through the database as ciphertext. Missing/short master keys are
	// logged and the store falls back to plaintext so local dev keeps
	// working without secrets plumbing.
	if masterKey, err := tenant.LoadMasterKey(); err == nil {
		km, err := tenant.NewKeyManager(masterKey, time.Hour)
		if err != nil {
			return err
		}
		recordStore = recordStore.WithEncryptor(km)
		log.Printf("api: per-tenant field encryption enabled")
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
	rateLimiter := platform.NewRateLimiter(platform.DefaultRateLimitConfig())
	quotaEnforcer := platform.NewQuotaEnforcer(pool)

	// Phase C finance engine — ledger store + invoice poster share
	// the same event publisher + audit logger so journal postings,
	// invoice lifecycle events, and KRecord mutations all emit into
	// the single outbox + audit tables used by the rest of the kernel.
	ledgerStore := ledger.NewPGStore(pool, eventPublisher, auditor)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, recordStore)

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
	objectStore := files.NewMemoryStore()
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

	// Agent tool executor — Phase B wires the CRM / tasks / approvals
	// tools against the same record store and workflow engine the HTTP
	// surface uses so dry-run and commit mode behave identically.
	// Phase C extends it with the finance tool suite; Phase D adds
	// inventory read + move tools.
	executor := agents.NewExecutor(recordStore, workflowEngine, auditor)
	agents.RegisterCRMTools(executor)
	agents.RegisterFinanceTools(executor, ledgerStore, invoicePoster)
	agents.RegisterInventoryTools(executor, inventoryStore)
	agents.RegisterHRTools(executor, hrStore)
	agents.RegisterLMSTools(executor, lmsStore)

	fh := &formsHandlers{store: formStore, registry: ktypeRegistry}
	th := &tenantHandlers{svc: tenantSvc}
	kh := &ktypeHandlers{registry: ktypeRegistry}
	rh := &recordHandlers{store: recordStore}
	wh := &workflowHandlers{engine: workflowEngine, store: recordStore, registry: ktypeRegistry}
	ah := &agentHandlers{executor: executor}
	aph := &approvalsHandlers{engine: workflowEngine, store: recordStore}
	auh := &auditHandlers{pool: pool}
	finh := &financeHandlers{store: ledgerStore, poster: invoicePoster}
	invh := &inventoryHandlers{store: inventoryStore}
	oh := &openAPIHandler{registry: ktypeRegistry}
	fileh := &filesHandlers{store: filesStore}
	bh := &baseHandlers{store: baseStore}
	dh := &docsHandlers{store: docsStore}
	eh := &eventsHandlers{pool: pool}
	vh := &viewHandlers{store: record.NewViewStore(pool)}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthHandler(pool))
	r.Get("/api/v1/", rootHandler)

	// Phase F event stream. SSE tail of the tenant's outbox so the web
	// UI can react to state changes without polling. Defined at the root
	// router so it does NOT inherit the 30s request timeout applied below
	// — chi's middleware.Timeout wraps the ResponseWriter and cancels the
	// context after the deadline, which would break any long-lived
	// stream. Idempotency/rate-limit are also skipped because SSE is a
	// GET and a spammed subscription is bounded by connection count.
	r.Route("/api/v1/events", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		r.Get("/stream", eh.stream)
	})

	// All non-streaming routes run under a 30s request deadline so a
	// slow handler can't hold a connection open indefinitely. The SSE
	// stream is deliberately registered above this group.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))

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
		r.Post("/credit-notes/{id}/post", finh.postCreditNote)
		r.Post("/debit-notes/{id}/post", finh.postDebitNote)
		r.Post("/tax-codes", finh.upsertTaxCode)
		r.Get("/tax-codes", finh.listTaxCodes)
		r.Get("/tax-codes/{code}", finh.getTaxCode)
		r.Post("/periods/lock", finh.lockPeriod)
		r.Get("/reports/trial-balance", finh.trialBalance)
		r.Get("/reports/ar-aging", finh.arAging)
		r.Get("/reports/ap-aging", finh.apAging)
		r.Get("/reports/income-statement", finh.incomeStatement)
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
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
		r.Use(platform.QuotaMiddleware(quotaEnforcer))
		r.Post("/items", invh.upsertItem)
		r.Get("/items", invh.listItems)
		r.Get("/items/{id}", invh.getItem)
		r.Post("/warehouses", invh.upsertWarehouse)
		r.Get("/warehouses", invh.listWarehouses)
		r.Post("/moves", invh.recordMove)
		r.Get("/moves", invh.listMoves)
		r.Post("/transfers", invh.recordTransfer)
		r.Get("/stock-levels", invh.listStockLevels)
		r.Get("/stock-levels/{id}", invh.stockLevelsByItem)
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
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
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
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
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
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
		r.Use(platform.QuotaMiddleware(quotaEnforcer))
		r.Get("/", dh.list)
		r.Post("/", dh.create)
		r.Get("/{id}", dh.get)
		r.Post("/{id}/versions", dh.saveVersion)
		r.Get("/{id}/versions", dh.versions)
		r.Post("/{id}/restore", dh.restore)
	})

	// Phase G saved views — per-user, per-KType filter/sort/column
	// layouts the RecordListPage persists so operators resume their
	// curated worklist across sessions. Mutations run under the same
	// idempotency + rate-limit + quota stack as record CRUD so a
	// spammed save cannot starve other tenants. RLS on saved_views
	// enforces tenant isolation; owner-only rules live in the store.
	r.Route("/api/v1/views", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
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
