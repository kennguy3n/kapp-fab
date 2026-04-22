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

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
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

	tenantSvc := tenant.NewPGStore(pool)
	ktypeCache := platform.NewLRUCache(1024, 5*time.Minute)
	ktypeRegistry := ktype.NewPGRegistry(pool, ktypeCache)
	eventPublisher := events.NewPGPublisher(pool)
	auditor := audit.NewPGLogger(pool)
	recordStore := record.NewPGStore(pool, ktypeRegistry, eventPublisher, auditor)
	rateLimiter := platform.NewRateLimiter(platform.DefaultRateLimitConfig())
	quotaEnforcer := platform.NewQuotaEnforcer(pool)

	th := &tenantHandlers{svc: tenantSvc}
	kh := &ktypeHandlers{registry: ktypeRegistry}
	rh := &recordHandlers{store: recordStore}
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
