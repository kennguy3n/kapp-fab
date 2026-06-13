package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/warehouse"
)

// warehouseSyncHandlers exposes CRUD for warehouse sync configs plus
// "run now" and run-history listing under /api/v1/warehouse-sync. The
// worker owns scheduled dispatch via
// warehouse.ActionTypeWarehouseSync; this surface persists config and
// drives an on-demand run through the SAME orchestration the scheduler
// uses, so a manual run and a cron run are recorded identically.
type warehouseSyncHandlers struct {
	configs *warehouse.ConfigStore
	runs    *warehouse.RunStore
	sync    *warehouse.SyncHandler
}

// warehouseSyncRequest is the create/update body. Watermarks and
// last-run state are server-owned and never accepted from the client.
// Enabled is a pointer so an omitted field is distinguishable from an
// explicit false: omitting it defaults to enabled (matching the DB
// column default) rather than silently creating a disabled sync.
type warehouseSyncRequest struct {
	Name                    string    `json:"name"`
	DestinationDataSourceID uuid.UUID `json:"destination_datasource_id"`
	DestinationSchema       string    `json:"destination_schema"`
	Sources                 []string  `json:"sources"`
	CronExpression          string    `json:"cron_expression"`
	Mode                    string    `json:"mode"`
	Enabled                 *bool     `json:"enabled"`
}

// enabledOrDefault resolves the tri-state Enabled field: a nil (omitted)
// value defaults to true so a client that does not mention enablement
// gets an active sync, matching the warehouse_sync_configs.enabled DB
// default.
func (req warehouseSyncRequest) enabledOrDefault() bool {
	if req.Enabled == nil {
		return true
	}
	return *req.Enabled
}

func (h *warehouseSyncHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.configs.List(r.Context(), t.ID)
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configs": out})
}

func (h *warehouseSyncHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req warehouseSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	out, err := h.configs.Create(r.Context(), warehouse.Config{
		TenantID:                t.ID,
		Name:                    req.Name,
		DestinationDataSourceID: req.DestinationDataSourceID,
		DestinationSchema:       req.DestinationSchema,
		Sources:                 req.Sources,
		CronExpression:          req.CronExpression,
		Mode:                    req.Mode,
		Enabled:                 req.enabledOrDefault(),
		CreatedBy:               &actor,
	})
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *warehouseSyncHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	out, err := h.configs.Get(r.Context(), t.ID, id)
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *warehouseSyncHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	var req warehouseSyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.configs.Update(r.Context(), warehouse.Config{
		TenantID:                t.ID,
		ID:                      id,
		Name:                    req.Name,
		DestinationDataSourceID: req.DestinationDataSourceID,
		DestinationSchema:       req.DestinationSchema,
		Sources:                 req.Sources,
		CronExpression:          req.CronExpression,
		Mode:                    req.Mode,
		Enabled:                 req.enabledOrDefault(),
	})
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *warehouseSyncHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	if err := h.configs.Delete(r.Context(), t.ID, id); err != nil {
		writeWarehouseError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// run starts an on-demand export (TriggerManual) for a config and
// returns 202 Accepted with the newly-created 'running' run. The export
// itself is driven on a context detached from this request, so a large
// sync (up to MaxSourcesPerConfig sources, each with its own copy
// budget) is never bound by HTTP gateway/load-balancer timeouts. The
// caller polls the run-history endpoint (GET .../runs) for the terminal
// status and per-source row counts. A non-2xx is reserved for problems
// that prevent the run from starting at all: an unknown config (404),
// or another run already holding this config's lock (ErrRunInProgress
// -> 409). The detached export's own failures land on the run row, not
// on this response.
func (h *warehouseSyncHandlers) run(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	cfg, err := h.configs.Get(r.Context(), t.ID, id)
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	run, finish, err := h.sync.StartManualRun(r.Context(), t.ID, cfg)
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	// Detach from the request so the export survives the response being
	// written; finish owns releasing the per-config run lock and bounds
	// itself with an overall deadline. Failures are recorded on the run
	// row, so they are surfaced via the history endpoint, not lost.
	go func() {
		ctx := context.WithoutCancel(r.Context())
		if err := finish(ctx); err != nil {
			platform.LoggerFromContext(ctx).Error("warehouse: manual run",
				"tenant", t.ID, "config", cfg.ID, "run", run.ID, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, run)
}

// runs lists a config's run history, newest first. ?limit caps the
// page (store-bounded).
func (h *warehouseSyncHandlers) runHistory(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid config id", http.StatusBadRequest)
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, perr := strconv.Atoi(raw); perr == nil {
			limit = n
		}
	}
	out, err := h.runs.List(r.Context(), t.ID, id, limit)
	if err != nil {
		writeWarehouseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func writeWarehouseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, warehouse.ErrConfigNotFound), errors.Is(err, warehouse.ErrRunNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, warehouse.ErrInvalidConfig):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, warehouse.ErrRunInProgress):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
