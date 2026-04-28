// Command kchat-bridge is the integration service that translates between
// KChat (Kapp's parent messaging product) and the Kapp platform. For Phase A
// it is a skeleton: it stands up an HTTP server, wires card rendering and
// slash-command dispatching, and leaves the actual KChat transport to later
// phases.
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

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/lms"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("kchat-bridge: %v", err)
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

	// Optional admin (BYPASSRLS) pool — required by the presence
	// webhook to walk user_tenants across tenants. When unset the
	// presence handler degrades to a no-op rather than 500-ing,
	// matching how services/api treats other cross-tenant reads.
	var adminPool *pgxpool.Pool
	if cfg.AdminDatabaseURL != "" {
		adminPool, err = platform.NewPool(ctx, cfg.AdminDatabaseURL)
		if err != nil {
			return err
		}
		defer adminPool.Close()
	}

	cache := platform.NewLRUCache(512, 5*time.Minute)
	registry := ktype.NewPGRegistry(pool, cache)
	eventPublisher := events.NewPGPublisher(pool)
	auditor := audit.NewPGLogger(pool)
	recordStore := record.NewPGStore(pool, registry, eventPublisher, auditor)
	workflowEngine := workflow.NewEngine(pool, eventPublisher, auditor)
	ledgerStore := ledger.NewPGStore(pool, eventPublisher, auditor)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, recordStore)
	inventoryStore := inventory.NewPGStore(pool, eventPublisher, auditor)
	// The `/post-invoice` and `/post-bill` KChat commands run the same
	// InvoicePoster the REST surface uses, so the inventory PosterHook
	// must be wired here too or a posted sales invoice / purchase bill
	// driven from chat would skip the goods-delivery / goods-receipt
	// move. Mirrors services/api/main.go:99-103.
	inventoryHook := inventory.NewPosterHook(inventoryStore)
	invoicePoster.
		WithSalesInvoiceHook(inventoryHook.OnSalesInvoicePosted).
		WithPurchaseBillHook(inventoryHook.OnPurchaseBillPosted)
	cards := &CardRenderer{registry: registry}
	composer := &Composer{registry: registry, records: recordStore, cards: cards}
	// The approvals renderer is what the worker service will call when
	// draining `approval.requested` / `approval.step_advanced` events
	// from the outbox to DM each approver a card. Exposed over HTTP at
	// /kchat/approvals/render so the worker stays stateless.
	approvalCards := NewApprovalCardRenderer(registry, cards, os.Getenv("KAPP_KCHAT_ACTIONS_BASE"))
	// Phase L Insights wiring. The dispatcher owns its own slice of
	// the insights stack so /insight + /dashboard-digest can run a
	// saved query without having to call back over HTTP into the API
	// service.
	reportingRunner := reporting.NewRunner(pool)
	insightsQueries := insights.NewQueryStore(pool)
	insightsDashboards := insights.NewDashboardStore(pool)
	insightsCache := insights.NewCacheStore(pool)
	// FeatureStore is also used by the presence handler below, but
	// constructed here so the insights runner can pick it up via
	// WithFeaturePolicy. Without this, RunSaved bypasses the SQL-mode
	// gate on the /insight slash command path for tenants downgraded
	// from enterprise to business.
	featureStore := tenant.NewFeatureStore(pool)
	insightsRunner := insights.NewRunner(pool, insightsCache, insightsQueries, reportingRunner).
		WithFeaturePolicy(featureStore)
	commands := &CommandDispatcher{
		registry:           registry,
		records:            recordStore,
		workflow:           workflowEngine,
		approvals:          workflowEngine,
		ledger:             ledgerStore,
		poster:             invoicePoster,
		inventory:          inventoryStore,
		lmsIssuer:          lms.NewCertificateIssuer(recordStore, pool),
		cards:              cards,
		formsBase:          os.Getenv("KAPP_FORMS_BASE_URL"),
		insightsQueries:    insightsQueries,
		insightsDashboards: insightsDashboards,
		insightsRunner:     insightsRunner,
		dashboardBase:      os.Getenv("KAPP_DASHBOARD_BASE_URL"),
	}

	// Presence webhook + supporting stores. The user store reuses the
	// shared pool — `users` is a global table so RLS doesn't matter.
	// The feature store gates the auto-attendance side-effect per
	// tenant via `attendance_kchat_sync` (constructed above so the
	// insights runner can also reuse it via WithFeaturePolicy).
	userStore := tenant.NewUserStore(pool).WithAdminPool(adminPool)
	presenceHandler := NewPresenceHandler(userStore, featureStore, recordStore)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Post("/kchat/cards/{ktype}", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		card, err := cards.RenderCard(req.Context(), chi.URLParam(req, "ktype"), body.Data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, card)
	})

	r.Post("/kchat/composer/actions", composer.HandleHTTP)

	// Phase G/L — KChat presence drives auto-attendance. Gated
	// per-tenant by the `attendance_kchat_sync` feature flag (a
	// no-op for tenants who haven't enabled it). Only `online`
	// transitions create records; idle/offline returns 204.
	r.Post("/kchat/presence", presenceHandler.HandleHTTP)

	// Approval-card render surface. The worker service drains
	// `approval.requested` / `approval.step_advanced` events from the
	// outbox and POSTs {tenant_id, approval_id} here to get the
	// per-approver card payload it should DM. Kept separate from the
	// decision surface (/kchat/commands approve) so rendering is
	// idempotent and free of side effects.
	r.Post("/kchat/approvals/render", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			TenantID   uuid.UUID `json:"tenant_id"`
			ApprovalID uuid.UUID `json:"approval_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.TenantID == uuid.Nil || body.ApprovalID == uuid.Nil {
			http.Error(w, "tenant_id and approval_id required", http.StatusBadRequest)
			return
		}
		approval, err := workflowEngine.GetApproval(req.Context(), body.TenantID, body.ApprovalID)
		if err != nil {
			if errors.Is(err, workflow.ErrApprovalNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Best-effort record hydration — a missing record still
		// yields a useful card (renderer falls back to approval-only
		// fields) so we don't let record lookup failures block the
		// approver DM.
		var rec *record.KRecord
		if r, err := recordStore.Get(req.Context(), body.TenantID, approval.RecordID); err == nil {
			rec = r
		}
		cardsByApprover, err := approvalCards.RenderForApprovers(req.Context(), approval, rec)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"approval_id": approval.ID,
			"state":       approval.State,
			"cards":       cardsByApprover,
		})
	})

	r.Post("/kchat/commands", func(w http.ResponseWriter, req *http.Request) {
		var body CommandRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := commands.Dispatch(req.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// Helpdesk thread back-post. The worker hits this surface when a
	// helpdesk.ticket carrying thread_id changes status, so the
	// originating KChat thread receives an inline status update card.
	// The bridge does not own KChat connectivity here — it logs the
	// payload at INFO and returns 202; a real KChat client adapter
	// can hook in later without changing the worker's contract.
	r.Post("/kchat/threads/post", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			TenantID uuid.UUID `json:"tenant_id"`
			Type     string    `json:"type"`
			ThreadID string    `json:"thread_id"`
			Title    string    `json:"title"`
			Body     string    `json:"body"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.ThreadID == "" {
			http.Error(w, "thread_id required", http.StatusBadRequest)
			return
		}
		log.Printf("kchat-bridge: thread post tenant=%s thread=%s type=%s title=%q",
			body.TenantID, body.ThreadID, body.Type, body.Title)
		w.WriteHeader(http.StatusAccepted)
	})

	// Phase L Insights right-pane card. KChat hits this endpoint when a
	// user opens a dashboard from the right-pane "Apps" surface; the
	// payload is a digest card listing every widget's latest summary.
	// Read-only — never mutates state, so no idempotency / quota needed.
	r.Post("/kchat/insights/dashboards/render", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			TenantID    uuid.UUID `json:"tenant_id"`
			DashboardID uuid.UUID `json:"dashboard_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.TenantID == uuid.Nil || body.DashboardID == uuid.Nil {
			http.Error(w, "tenant_id and dashboard_id required", http.StatusBadRequest)
			return
		}
		dash, err := insightsDashboards.Get(req.Context(), body.TenantID, body.DashboardID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		fakeReq := CommandRequest{
			TenantID: body.TenantID,
			Command:  "/dashboard-digest",
			Args:     []string{dash.Name},
		}
		resp, err := commands.dashboardDigest(req.Context(), fakeReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	addr := cfg.ListenAddr
	if addr == ":8080" {
		addr = ":8082"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("kchat-bridge: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("kchat-bridge: shutdown signal received")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
