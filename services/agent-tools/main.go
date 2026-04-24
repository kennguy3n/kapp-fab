// Command agent-tools is the Phase B "AI coworker" executor. It accepts
// HTTP POSTs from orchestrators (an LLM, a scripted integration, a human
// driving the UI) that want to invoke a named agent tool against a
// tenant and receive either a preview (dry-run) or a committed result.
// Every invocation is audited with actor_kind = "agent".
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

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
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
		log.Fatalf("agent-tools: %v", err)
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

	ktypeCache := platform.NewLRUCache(1024, 5*time.Minute)
	ktypeRegistry := ktype.NewPGRegistry(pool, ktypeCache)
	eventPublisher := events.NewPGPublisher(pool)
	auditor := audit.NewPGLogger(pool)
	recordStore := record.NewPGStore(pool, ktypeRegistry, eventPublisher, auditor)
	workflowEngine := workflow.NewEngine(pool, eventPublisher, auditor)
	tenantSvc := tenant.NewPGStore(pool)
	rateLimitCfg := platform.DefaultRateLimitConfig()
	rateLimiter := platform.NewRateLimiter(rateLimitCfg)
	var redisLimiter *platform.RedisRateLimiter
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		rl, err := platform.NewRedisRateLimiter(ctx, redisURL, rateLimitCfg)
		if err != nil {
			log.Printf("agent-tools: redis rate limiter init failed, falling back to in-process: %v", err)
		} else {
			redisLimiter = rl
			defer func() { _ = redisLimiter.Close() }()
			log.Printf("agent-tools: distributed rate limiter enabled (redis)")
		}
	}
	quotaEnforcer := platform.NewQuotaEnforcer(pool)

	// Domain stores shared between the API gateway and the standalone
	// agent-tools executor. Wiring them here keeps the two surfaces in
	// lock-step so an agent tool that works against the API pool also
	// works when invoked directly via the dedicated executor service.
	// Mirrors the wiring in services/api/main.go.
	ledgerStore := ledger.NewPGStore(pool, eventPublisher, auditor)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, recordStore)
	paymentPoster := ledger.NewPaymentPoster(ledgerStore, recordStore)
	inventoryStore := inventory.NewPGStore(pool, eventPublisher, auditor)
	inventoryHook := inventory.NewPosterHook(inventoryStore)
	invoicePoster.
		WithSalesInvoiceHook(inventoryHook.OnSalesInvoicePosted).
		WithPurchaseBillHook(inventoryHook.OnPurchaseBillPosted)
	hrStore := hr.NewStore(pool)
	lmsStore := lms.NewStore(pool)

	executor := agents.NewExecutor(recordStore, workflowEngine, auditor)
	agents.RegisterCRMTools(executor)
	agents.RegisterFinanceTools(executor, ledgerStore, invoicePoster, paymentPoster)
	agents.RegisterInventoryTools(executor, inventoryStore)
	agents.RegisterHRTools(executor, hrStore)
	agents.RegisterLMSTools(executor, lmsStore)

	h := &toolsHandler{executor: executor}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", healthz)
	// Tool discovery — callers list available tools by name.
	r.Get("/api/v1/agents/tools", h.list)
	// Tool invocation shares the same middleware stack as KRecord CRUD
	// (ARCHITECTURE.md §11): TenantMiddleware authoritatively sets the
	// tenant from X-Tenant-ID so the invocation body can't spoof it,
	// idempotency runs first so retried agent calls don't double-write,
	// and rate-limit + quota cap per-tenant agent traffic.
	r.Route("/api/v1/agents/tools", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		r.Use(platform.IdempotencyMiddleware(pool))
		if redisLimiter != nil {
			r.Use(platform.RedisRateLimitMiddleware(redisLimiter))
		} else {
			r.Use(platform.RateLimitMiddleware(rateLimiter))
		}
		r.Use(platform.QuotaMiddleware(quotaEnforcer))
		r.Post("/{name}", h.invoke)
	})

	addr := os.Getenv("AGENT_TOOLS_LISTEN_ADDR")
	if addr == "" {
		addr = ":8082"
	}
	srv := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("agent-tools: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		log.Printf("agent-tools: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	sc, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(sc)
}

type toolsHandler struct {
	executor *agents.Executor
}

func (h *toolsHandler) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tools": h.executor.Tools()})
}

func (h *toolsHandler) invoke(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "tool name required", http.StatusBadRequest)
		return
	}
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var inv agents.Invocation
	if err := json.NewDecoder(r.Body).Decode(&inv); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	// The tenant on the context (set by TenantMiddleware from
	// X-Tenant-ID) is authoritative — an attacker can't spoof
	// cross-tenant writes by setting tenant_id in the invocation body.
	inv.TenantID = t.ID
	inv.ToolName = name
	res, err := h.executor.Invoke(r.Context(), inv)
	if err != nil {
		writeToolError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeToolError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agents.ErrUnknownTool):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, agents.ErrConfirmationRequired):
		http.Error(w, err.Error(), http.StatusPreconditionRequired)
	case errors.Is(err, agents.ErrInvalidMode),
		errors.Is(err, agents.ErrMissingContext):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
