// Command importer is the Phase F data-onboarding service. It hosts
// the HTTP surface that operators drive through the Discover → Export
// → Normalize → Map → Validate → Stage → Reconcile → Accept →
// Cutover pipeline. The heavy lifting lives in internal/importer; this
// binary wires adapters + the HTTP handlers and nothing more.
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/importer"
	"github.com/kennguy3n/kapp-fab/internal/importer/adapters"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("importer: %v", err)
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

	jobStore := importer.NewJobStore(pool)
	stagingStore := importer.NewStagingStore(pool)
	validator := importer.NewValidator(ktypeRegistry, nil)
	reconciler := importer.NewReconciler(stagingStore)
	pipeline := importer.NewPipeline(jobStore, stagingStore, validator, reconciler, recordStore)
	pipeline.RegisterAdapter(adapters.NewCSVAdapter())
	// Wizard's "JSON" dropdown sends source_type="json"; register the
	// JSON alias so the CSV adapter's json branch (format: "json") is
	// reachable without a second implementation.
	pipeline.RegisterAdapter(adapters.NewJSONAdapter())
	pipeline.RegisterAdapter(adapters.NewFrappeAdapter())

	h := &importHandlers{pipeline: pipeline, jobs: jobStore, staging: stagingStore}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(120 * time.Second))

	r.Get("/healthz", healthHandler(pool))

	// All import routes live under the same tenant + idempotency +
	// rate-limit + quota stack as the record CRUD surface. Imports
	// are mutations by their nature — a replay that swapped tenants
	// or bypassed quotas would be a cross-tenant data leak or a
	// noisy-neighbour vector.
	r.Route("/api/v1/imports", func(r chi.Router) {
		r.Use(platform.TenantMiddleware(tenantSvc))
		r.Use(platform.IdempotencyMiddleware(pool))
		r.Use(platform.RateLimitMiddleware(rateLimiter))
		r.Use(platform.QuotaMiddleware(quotaEnforcer))
		r.Post("/", h.create)
		r.Get("/", h.list)
		r.Get("/{id}", h.get)
		r.Post("/{id}/map", h.submitMapping)
		r.Post("/{id}/validate", h.validate)
		r.Post("/{id}/accept", h.accept)
		r.Get("/{id}/errors", h.errors)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("importer: listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("importer: shutdown signal received")
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
	log.Printf("importer: shutdown complete")
	return nil
}

// importHandlers serves the six endpoints the import wizard UI needs.
// The pipeline methods do the real work — the handlers are thin
// glue around request/response marshaling + tenant context lookup.
type importHandlers struct {
	pipeline *importer.Pipeline
	jobs     *importer.JobStore
	staging  *importer.StagingStore
}

// createImportRequest is the body for POST /api/v1/imports. The
// config is an opaque JSON blob the target adapter knows how to read.
type createImportRequest struct {
	SourceType string          `json:"source_type"`
	Config     json.RawMessage `json:"config"`
}

// create registers a fresh ImportJob and kicks off the Discover +
// Export + Stage sub-pipeline synchronously so the operator gets a
// concrete job + row count back in one round trip. Long-running
// stages (hundreds of thousands of rows) should run in the worker;
// the synchronous flow keeps Phase F self-contained.
func (h *importHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.SourceType == "" {
		http.Error(w, "source_type required", http.StatusBadRequest)
		return
	}

	job, err := h.jobs.Create(r.Context(), importer.ImportJob{
		TenantID:   t.ID,
		SourceType: req.SourceType,
		Config:     req.Config,
		CreatedBy:  actorOrDefault(r.Context()),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, _, err := h.pipeline.StartStaging(r.Context(), job); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-read so the response reflects the post-staging state.
	job, err = h.jobs.Get(r.Context(), t.ID, job.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// list returns recent import jobs for the tenant, newest first.
func (h *importHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	jobs, err := h.jobs.List(r.Context(), t.ID, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// get returns a single job's status, progress, and error summary.
func (h *importHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := h.jobs.Get(r.Context(), t.ID, id)
	if err != nil {
		if errors.Is(err, importer.ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// submitMapping stores the operator's DocType→KType + field mapping
// on the job. Shape is adapter-specific; the pipeline passes it
// verbatim to resolveTarget.
type submitMappingRequest struct {
	Mapping json.RawMessage `json:"mapping"`
}

func (h *importHandlers) submitMapping(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	var req submitMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	job, err := h.jobs.UpdateMapping(r.Context(), t.ID, id, req.Mapping)
	if err != nil {
		if errors.Is(err, importer.ErrJobNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// validate flips every pending staging row to valid or invalid and
// then runs the reconciler over the resulting counts.
func (h *importHandlers) validate(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := h.jobs.Get(r.Context(), t.ID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	valid, invalid, err := h.pipeline.Validate(r.Context(), job)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rec, err := h.pipeline.Reconcile(r.Context(), job, importer.SourceSummary{
		Count: valid + invalid,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	job, err = h.jobs.Get(r.Context(), t.ID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":           job,
		"valid":         valid,
		"invalid":       invalid,
		"reconciliation": rec,
	})
}

// accept promotes every `valid` staging row to a live KRecord. The
// pipeline handles the status transitions; the handler just picks up
// the actor and surfaces the imported count.
func (h *importHandlers) accept(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := h.jobs.Get(r.Context(), t.ID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	imported, err := h.pipeline.Accept(r.Context(), job, actorOrDefault(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	job, err = h.jobs.Get(r.Context(), t.ID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":      job,
		"imported": imported,
	})
}

// errors surfaces the per-row validation errors for the job. The UI
// renders this as a table so operators can fix the source and retry.
func (h *importHandlers) errors(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	rows, err := h.staging.ListByJob(r.Context(), t.ID, id, importer.StagingInvalid, 500, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, rows)
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

// phaseFSystemActor is the deterministic actor attributed to imports
// that arrive without an authenticated user. Mirrors services/api's
// phaseASystemActor so audit trails stay coherent across services.
var phaseFSystemActor = uuid.MustParse("00000000-0000-0000-0000-000000000002")

func actorOrDefault(ctx context.Context) uuid.UUID {
	if id := platform.UserIDFromContext(ctx); id != uuid.Nil {
		return id
	}
	return phaseFSystemActor
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
