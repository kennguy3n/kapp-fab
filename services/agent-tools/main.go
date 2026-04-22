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
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
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

	executor := agents.NewExecutor(recordStore, workflowEngine, auditor)
	agents.RegisterCRMTools(executor)

	h := &toolsHandler{executor: executor}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", healthz)
	// Tool discovery — callers list available tools by name.
	r.Get("/api/v1/agents/tools", h.list)
	// Tool invocation. The handler does NOT use TenantMiddleware because
	// the tenant id is supplied inside the invocation envelope, not via
	// the X-Tenant-ID header — agent callers are typically long-lived
	// bots that may legitimately drive multiple tenants in one session.
	r.Post("/api/v1/agents/tools/{name}", h.invoke)

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
	var inv agents.Invocation
	if err := json.NewDecoder(r.Body).Decode(&inv); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
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
