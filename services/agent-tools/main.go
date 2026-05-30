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
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
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

	logger := platform.NewLogger(platform.LoggerConfig{
		Format:  cfg.LogFormat,
		Level:   cfg.LogLevel,
		Service: "agent-tools",
		Env:     cfg.Env,
	}, os.Stderr)
	platform.InstallDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracingShutdown, err := platform.InitTracing(ctx, platform.LoadTracingConfig("kapp-agent-tools", cfg.Env))
	if err != nil {
		return fmt.Errorf("agent-tools: init tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown", slog.String("err", err.Error()))
		}
	}()

	pool, err := platform.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	ktypeCache := platform.NewLRUCache(cfg.KTypeCacheSize, 5*time.Minute)
	ktypeRegistry := ktype.NewPGRegistry(pool, ktypeCache)
	eventPublisher := events.NewPGPublisher(pool)
	auditor := audit.NewPGLogger(pool)
	// A1: read-only paths on the agent-tools service (record.Get,
	// list-shape lookups inside tool executions) route through the
	// PoolRouter so a configured replica absorbs the read load.
	// Writes (record.Create/Update/Delete in the tool handlers)
	// always go to the primary regardless of routing decisions.
	dbRouter := dbutil.NewPoolRouter(pool)
	if cfg.ReadReplicaURL != "" {
		replicaPool, err := platform.NewPool(ctx, cfg.ReadReplicaURL)
		if err != nil {
			return fmt.Errorf("agent-tools: open read replica pool: %w", err)
		}
		defer replicaPool.Close()
		dbRouter = dbRouter.WithReplica(replicaPool, cfg.ReadReplicaLagTolerance, cfg.ReadReplicaLagSampleInterval)
		dbRouter.StartLagSampler(ctx, cfg.ReadReplicaLagSampleInterval)
	}
	recordStore := record.NewPGStoreWithRouter(dbRouter, ktypeRegistry, eventPublisher, auditor)
	workflowEngine := workflow.NewEngine(pool, eventPublisher, auditor)
	tenantCache := platform.NewLRUCache(cfg.TenantCacheSize, 30*time.Second)
	tenantSvc := tenant.NewPGStore(pool).WithCache(tenantCache)
	rateLimitCfg := platform.DefaultRateLimitConfig()
	rateLimiter := platform.NewRateLimiter(rateLimitCfg)
	var redisLimiter *platform.RedisRateLimiter
	if cfg.RedisURL != "" {
		rl, err := platform.NewRedisRateLimiter(ctx, cfg.RedisURL, rateLimitCfg)
		if err != nil {
			if cfg.RequireRedis {
				return fmt.Errorf("agent-tools: redis rate limiter init failed and KAPP_REQUIRE_REDIS=1: %w", err)
			}
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
	agents.RegisterManufacturingTools(executor, manufacturing.NewPGStore(pool, inventoryStore))
	agents.RegisterHRTools(executor, hrStore)
	agents.RegisterLMSTools(executor, lmsStore)

	h := &toolsHandler{executor: executor}

	r := chi.NewRouter()
	// Standard chain order across all kapp services: RealIP first
	// (rewrites RemoteAddr from forwarded headers so every layer
	// downstream sees the originating client), Recoverer next so a
	// panic in any subsequent middleware turns into a 500 rather
	// than killing the goroutine, then RequestIDMiddleware so every
	// request carries a stable id BEFORE handlers see it. Mirrors
	// the api and importer service chains.
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(platform.RequestIDMiddleware(logger))
	r.Use(platform.TracingMiddleware("kapp-agent-tools"))
	r.Use(middleware.Timeout(30 * time.Second))

	// Phase 5: JWT-derived tenant + user identity, mirroring
	// services/api. Before Phase 5 this sidecar trusted the
	// X-Tenant-ID header outright, which let any caller on the
	// internal network spoof a tenant id and run agent tools
	// against rows that belong to somebody else. The mitigation
	// is rate-limited by KAPP_REQUIRE_JWT:
	//
	//   - signer != nil  (KAPP_JWT_SECRET set): JWT required on
	//     /api/v1/agents/tools. The X-Tenant-ID header is ignored
	//     once auth.Middleware is active.
	//   - signer == nil + KAPP_REQUIRE_JWT=1: refuse to boot.
	//   - signer == nil + default: WARN + legacy header path,
	//     bridge mode for clusters that have not rolled JWT to
	//     their sidecars yet.
	signer, signerErr := auth.SignerFromEnv()
	sessionStore := auth.NewPGSessionStore(pool)
	requireJWT := auth.RequireJWT()
	switch {
	case signer != nil:
		logger.Info("jwt auth enabled", slog.String("algorithm", "HS256"))
	case requireJWT:
		return fmt.Errorf("agent-tools: KAPP_REQUIRE_JWT=1 but KAPP_JWT_SECRET is unset or invalid: %w", signerErr)
	default:
		logger.Warn(
			"agent-tools running WITHOUT JWT auth — X-Tenant-ID header is trusted; "+
				"this is UNSAFE for any non-trusted network. Set KAPP_JWT_SECRET to enable "+
				"JWT-derived tenant scoping and KAPP_REQUIRE_JWT=1 to make the boot fail "+
				"loudly when the secret is missing",
			slog.String("signer_err", signerErr.Error()),
		)
	}

	r.Get("/healthz", healthz)
	// Tool discovery — callers list available tools by name.
	r.Get("/api/v1/agents/tools", h.list)
	// Tool invocation shares the same middleware stack as KRecord CRUD
	// (ARCHITECTURE.md §11): auth.Middleware authoritatively writes
	// the JWT-claim tenant + user onto the request context so the
	// invocation body can't spoof either; idempotency runs first so
	// retried agent calls don't double-write; and rate-limit + quota
	// cap per-tenant agent traffic. The legacy header path is
	// preserved as a fallback for the bridge-mode case described
	// above (signer == nil + KAPP_REQUIRE_JWT unset).
	r.Route("/api/v1/agents/tools", func(r chi.Router) {
		if signer != nil {
			r.Use(auth.Middleware(signer, tenantSvc, sessionStore))
			r.Use(auth.RequireActiveHomeTenant())
		} else {
			r.Use(platform.TenantMiddleware(tenantSvc))
		}
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
	// Agent-tools is a request-response service (no long-lived
	// streams) so DefaultHTTPTimeouts applies. Tool invocations
	// occasionally do non-trivial work (LLM-mediated calls into
	// internal stores); WriteTimeout=120s is the upper bound and
	// operators can bump KAPP_HTTP_WRITE_TIMEOUT if their tools
	// legitimately need longer.
	timeouts := platform.LoadHTTPTimeouts(platform.DefaultHTTPTimeouts())
	srv := &http.Server{Addr: addr, Handler: r}
	timeouts.Apply(srv)
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
	// The tenant on the context is authoritative — an attacker
	// cannot spoof cross-tenant writes by setting tenant_id in
	// the invocation body. The context tenant is sourced from
	// either auth.Middleware (JWT tid claim, when KAPP_JWT_SECRET
	// is set) or platform.TenantMiddleware (X-Tenant-ID header,
	// legacy bridge mode), depending on which middleware was
	// mounted on the route group above. In both cases the same
	// invariant holds: handlers MUST use the context tenant, not
	// any field the caller wrote into the request body.
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
