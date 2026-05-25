package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// manufacturingHandlers exposes the Phase N6 BOM + work-order HTTP
// surface. Tenant scope is enforced by the middleware stack; these
// handlers translate HTTP into manufacturing.PGStore calls and map
// sentinel errors to the status codes the web client expects.
type manufacturingHandlers struct {
	store *manufacturing.PGStore
}

// ---------------------------------------------------------------------------
// BOMs
// ---------------------------------------------------------------------------

type bomComponentRequest struct {
	ComponentItemID uuid.UUID        `json:"component_item_id"`
	Qty             decimal.Decimal  `json:"qty"`
	UOM             string           `json:"uom"`
	ScrapPercent    *decimal.Decimal `json:"scrap_percent,omitempty"`
	SortOrder       int              `json:"sort_order"`
}

type createBOMRequest struct {
	ItemID     uuid.UUID             `json:"item_id"`
	Version    string                `json:"version"`
	OutputQty  decimal.Decimal       `json:"output_qty"`
	UOM        string                `json:"uom"`
	Notes      string                `json:"notes,omitempty"`
	Components []bomComponentRequest `json:"components"`
	// Activate, when true, transitions the freshly-created BOM
	// from draft to active immediately after insert. Convenient
	// for the common SME case where a single BOM per item is
	// authored end-to-end in one HTTP call.
	Activate bool `json:"activate,omitempty"`
}

func (h *manufacturingHandlers) createBOM(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	var req createBOMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in := manufacturing.CreateBOMInput{
		ItemID:    req.ItemID,
		Version:   req.Version,
		OutputQty: req.OutputQty,
		UOM:       req.UOM,
		Notes:     req.Notes,
	}
	for _, c := range req.Components {
		in.Components = append(in.Components, manufacturing.BOMComponent{
			ComponentItemID: c.ComponentItemID,
			Qty:             c.Qty,
			UOM:             c.UOM,
			ScrapPercent:    c.ScrapPercent,
			SortOrder:       c.SortOrder,
		})
	}
	bom, err := h.store.CreateBOM(r.Context(), t.ID, actor, in)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	if req.Activate {
		if err := h.store.SetBOMStatus(r.Context(), t.ID, bom.ID, manufacturing.BOMStatusActive); err != nil {
			writeManufacturingError(w, err)
			return
		}
		bom, err = h.store.GetBOM(r.Context(), t.ID, bom.ID)
		if err != nil {
			writeManufacturingError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, bom)
}

func (h *manufacturingHandlers) listBOMs(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	status := r.URL.Query().Get("status")
	out, err := h.store.ListBOMs(r.Context(), t.ID, status)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getBOM(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid bom id", http.StatusBadRequest)
		return
	}
	bom, err := h.store.GetBOM(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bom)
}

type setBOMStatusRequest struct {
	Status string `json:"status"`
}

func (h *manufacturingHandlers) setBOMStatus(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid bom id", http.StatusBadRequest)
		return
	}
	var req setBOMStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.store.SetBOMStatus(r.Context(), t.ID, id, req.Status); err != nil {
		writeManufacturingError(w, err)
		return
	}
	bom, err := h.store.GetBOM(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bom)
}

// ---------------------------------------------------------------------------
// Work orders
// ---------------------------------------------------------------------------

type createWorkOrderRequest struct {
	ItemID         uuid.UUID       `json:"item_id"`
	WarehouseID    uuid.UUID       `json:"warehouse_id"`
	PlannedQty     decimal.Decimal `json:"planned_qty"`
	ScheduledStart *time.Time      `json:"scheduled_start,omitempty"`
	ScheduledEnd   *time.Time      `json:"scheduled_end,omitempty"`
	Notes          string          `json:"notes,omitempty"`
}

func (h *manufacturingHandlers) createWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	var req createWorkOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	wo, err := h.store.CreateWorkOrder(r.Context(), t.ID, actor, manufacturing.CreateWorkOrderInput{
		ItemID:         req.ItemID,
		WarehouseID:    req.WarehouseID,
		PlannedQty:     req.PlannedQty,
		ScheduledStart: req.ScheduledStart,
		ScheduledEnd:   req.ScheduledEnd,
		Notes:          req.Notes,
	})
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wo)
}

func (h *manufacturingHandlers) listWorkOrders(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	status := r.URL.Query().Get("status")
	out, err := h.store.ListWorkOrders(r.Context(), t.ID, status)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	wo, err := h.store.GetWorkOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wo)
}

// workOrderActionRequest is the JSON envelope for the status-change
// endpoints. ActualQty is only consulted by /complete.
type workOrderActionRequest struct {
	ActualQty decimal.Decimal `json:"actual_qty,omitempty"`
}

func (h *manufacturingHandlers) releaseWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	wo, err := h.store.ReleaseWorkOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wo)
}

func (h *manufacturingHandlers) startWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	wo, err := h.store.StartWorkOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wo)
}

func (h *manufacturingHandlers) completeWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	var req workOrderActionRequest
	// Body is optional — empty body means "complete with actual =
	// planned" which is the most common path for a small shop.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	wo, err := h.store.CompleteWorkOrder(r.Context(), t.ID, id, actor, manufacturing.CompleteWorkOrderInput{
		ActualQty: req.ActualQty,
	})
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wo)
}

func (h *manufacturingHandlers) cancelWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	wo, err := h.store.CancelWorkOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wo)
}

func (h *manufacturingHandlers) closeWorkOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	wo, err := h.store.CloseWorkOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wo)
}

// writeManufacturingError maps the package's sentinel errors to HTTP
// status codes consistent with the rest of the API surface.
func writeManufacturingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manufacturing.ErrBOMNotFound),
		errors.Is(err, manufacturing.ErrWorkOrderNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, manufacturing.ErrBOMNotActive),
		errors.Is(err, manufacturing.ErrBOMHasNoComponents),
		errors.Is(err, manufacturing.ErrBOMSelfReference),
		errors.Is(err, manufacturing.ErrWorkOrderInvalidTransition),
		errors.Is(err, manufacturing.ErrWorkOrderInsufficientStock):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
