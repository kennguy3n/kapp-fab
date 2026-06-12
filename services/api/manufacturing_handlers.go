package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
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

// bomComponentRequest is the HTTP shape for one component on a
// createBOM call. Ordering is implicit in the JSON array position —
// the store assigns sort_order = (index + 1) inside CreateBOM, so we
// intentionally do not accept a `sort_order` field. Earlier drafts
// had one, but the store always overrode it (see
// internal/manufacturing/store.go's CreateBOM contract), which gave
// HTTP clients the false impression they controlled ordering. The
// canonical contract is now: "the components array's order IS the
// BOM's display order". Clients reorder by re-sending the full
// array; partial updates are not supported.
type bomComponentRequest struct {
	ComponentItemID uuid.UUID        `json:"component_item_id"`
	Qty             decimal.Decimal  `json:"qty"`
	UOM             string           `json:"uom"`
	ScrapPercent    *decimal.Decimal `json:"scrap_percent,omitempty"`
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
		// Pushing activation into the store layer makes create-
		// and-activate a single transaction; if the activation
		// step fails the insert rolls back too, so the client's
		// retry doesn't have to navigate a half-finished BOM
		// already occupying the (tenant_id, item_id, version)
		// unique slot. See CreateBOMInput.Activate.
		Activate: req.Activate,
	}
	for _, c := range req.Components {
		// SortOrder intentionally omitted — the store assigns it
		// from the array index in CreateBOM. See the
		// bomComponentRequest doc comment.
		in.Components = append(in.Components, manufacturing.BOMComponent{
			ComponentItemID: c.ComponentItemID,
			Qty:             c.Qty,
			UOM:             c.UOM,
			ScrapPercent:    c.ScrapPercent,
		})
	}
	bom, err := h.store.CreateBOM(r.Context(), t.ID, actor, in)
	if err != nil {
		writeManufacturingError(w, err)
		return
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
// endpoints. Every field except ActualQty is consulted only by
// /complete, and only when the finished good or a component is
// lot/serial-tracked.
type workOrderActionRequest struct {
	ActualQty decimal.Decimal `json:"actual_qty,omitempty"`

	// FinishedBatchID / FinishedSerials carry the lot and serials the
	// finished-good receipt is booked into. Required when the output
	// item is lot- / serial-tracked.
	FinishedBatchID *uuid.UUID `json:"finished_batch_id,omitempty"`
	FinishedSerials []string   `json:"finished_serials,omitempty"`

	// ComponentBatches / ComponentSerials key off the component item id
	// (a UUID string in JSON) and supply the lot / serials each
	// consumption move draws down. Required for every lot- /
	// serial-tracked component.
	ComponentBatches map[uuid.UUID]uuid.UUID `json:"component_batches,omitempty"`
	ComponentSerials map[uuid.UUID][]string  `json:"component_serials,omitempty"`
}

// completeInput maps the decoded request onto the store input. It is a
// single chokepoint for the lot/serial passthrough: the handler must
// forward every tracking field, otherwise the store's Phase-1
// validateTrackedMove sees nil/empty serials+batches and rejects the
// completion of any tracked item with ErrSerialRequired/ErrLotRequired.
// Kept as a pure method so the forwarding is unit-testable without a DB.
func (req workOrderActionRequest) completeInput() manufacturing.CompleteWorkOrderInput {
	return manufacturing.CompleteWorkOrderInput{
		ActualQty:        req.ActualQty,
		FinishedBatchID:  req.FinishedBatchID,
		FinishedSerials:  req.FinishedSerials,
		ComponentBatches: req.ComponentBatches,
		ComponentSerials: req.ComponentSerials,
	}
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
	//
	// Guard on `r.Body != nil && r.ContentLength != 0` rather than
	// `r.ContentLength > 0`. For chunked-transfer-encoded requests
	// (which any HTTP/1.1 client may use, and which curl emits when
	// you pipe stdin into -d @-), net/http sets ContentLength to -1
	// to signal "unknown until EOF". The old `> 0` check silently
	// dropped the body on those requests, so the server completed
	// with actualQty defaulted to planned even when the operator
	// explicitly supplied a different yield. io.EOF is treated as
	// "body was empty after all" rather than a 400, matching the
	// pattern fixed in services/api/consolidation_handlers.go.
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	wo, err := h.store.CompleteWorkOrder(r.Context(), t.ID, id, actor, req.completeInput())
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
		errors.Is(err, manufacturing.ErrWorkOrderNotFound),
		errors.Is(err, manufacturing.ErrRoutingNotFound),
		errors.Is(err, manufacturing.ErrWorkCenterNotFound),
		errors.Is(err, manufacturing.ErrJobCardNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, manufacturing.ErrMRPRunNotFound),
		errors.Is(err, manufacturing.ErrSubcontractOrderNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, manufacturing.ErrWorkCenterDuplicateName):
		// Duplicate (tenant_id, name) — a conflict the client can
		// resolve by renaming, so 409 rather than the 422 used for
		// malformed input.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, inventory.ErrInsufficientLotStock),
		errors.Is(err, inventory.ErrSerialNotAvailable),
		errors.Is(err, inventory.ErrSerialAlreadyInStock),
		errors.Is(err, inventory.ErrDuplicateSourceMove):
		// Lot/serial state conflicts surfaced by CompleteWorkOrder's
		// Phase-2 inventory moves (over-issue of a lot, a serial that
		// is no longer in stock / already in stock, or an idempotent
		// replay). The client can resolve by correcting the lot/serial
		// payload or retrying, so 409 rather than 422.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, inventory.ErrLotRequired),
		errors.Is(err, inventory.ErrSerialRequired),
		errors.Is(err, inventory.ErrSerialUnsupported),
		errors.Is(err, inventory.ErrSerialQtyMismatch),
		errors.Is(err, inventory.ErrDuplicateSerialInput),
		errors.Is(err, inventory.ErrBatchNotFound),
		errors.Is(err, inventory.ErrBatchItemMismatch):
		// The work order's lot/serial payload is malformed for the
		// item's tracking contract (missing lot, wrong serial count,
		// duplicate or unsupported serials, unknown/mismatched batch).
		// validateTrackedMove raises these in Phase 1, before the
		// status flip, so the completion is rejected as client input
		// validation rather than a server fault.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, manufacturing.ErrBOMNotActive),
		errors.Is(err, manufacturing.ErrBOMHasNoComponents),
		errors.Is(err, manufacturing.ErrBOMSelfReference),
		errors.Is(err, manufacturing.ErrBOMDuplicateComponent),
		errors.Is(err, manufacturing.ErrBOMInvalidTransition),
		errors.Is(err, manufacturing.ErrWorkOrderInvalidTransition),
		errors.Is(err, manufacturing.ErrWorkOrderInsufficientStock),
		errors.Is(err, manufacturing.ErrRoutingNotActive),
		errors.Is(err, manufacturing.ErrRoutingHasNoOperations),
		errors.Is(err, manufacturing.ErrRoutingInvalidTransition),
		errors.Is(err, manufacturing.ErrRoutingDuplicateSequence),
		errors.Is(err, manufacturing.ErrJobCardInvalidTransition),
		errors.Is(err, manufacturing.ErrCapacityRangeInvalid),
		errors.Is(err, manufacturing.ErrMRPNoDemand),
		errors.Is(err, manufacturing.ErrMRPCyclicBOM),
		errors.Is(err, manufacturing.ErrSubcontractNoComponents),
		errors.Is(err, manufacturing.ErrSubcontractDuplicateComponent),
		errors.Is(err, manufacturing.ErrSubcontractInvalidTransition),
		errors.Is(err, manufacturing.ErrSubcontractInsufficientStock),
		// ErrInvalidInput is the umbrella sentinel for client-supplied
		// validation failures (empty / zero / out-of-range fields on
		// the create/update endpoints — invalid bom status, negative
		// actual_qty, over-yield actual_qty, missing item_id /
		// warehouse_id / version, etc.). The store wraps it with %w
		// so errors.Is matches every wrapped variant, and we
		// short-circuit them all to 422 here in a single arm rather
		// than minting a sentinel per field.
		errors.Is(err, manufacturing.ErrInvalidInput):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---------------------------------------------------------------------------
// Work centers
// ---------------------------------------------------------------------------

type createWorkCenterRequest struct {
	Name                 string          `json:"name"`
	CapacityPerHour      decimal.Decimal `json:"capacity_per_hour"`
	OperatingHoursPerDay decimal.Decimal `json:"operating_hours_per_day"`
	EfficiencyPercent    decimal.Decimal `json:"efficiency_percent"`
	Notes                string          `json:"notes,omitempty"`
}

func (h *manufacturingHandlers) createWorkCenter(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	var req createWorkCenterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	wc, err := h.store.CreateWorkCenter(r.Context(), t.ID, actor, manufacturing.CreateWorkCenterInput{
		Name:                 req.Name,
		CapacityPerHour:      req.CapacityPerHour,
		OperatingHoursPerDay: req.OperatingHoursPerDay,
		EfficiencyPercent:    req.EfficiencyPercent,
		Notes:                req.Notes,
	})
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, wc)
}

func (h *manufacturingHandlers) listWorkCenters(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.ListWorkCenters(r.Context(), t.ID, r.URL.Query().Get("status"))
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getWorkCenter(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work center id", http.StatusBadRequest)
		return
	}
	wc, err := h.store.GetWorkCenter(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wc)
}

type setWorkCenterStatusRequest struct {
	Status string `json:"status"`
}

func (h *manufacturingHandlers) setWorkCenterStatus(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work center id", http.StatusBadRequest)
		return
	}
	var req setWorkCenterStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.store.SetWorkCenterStatus(r.Context(), t.ID, id, req.Status); err != nil {
		writeManufacturingError(w, err)
		return
	}
	wc, err := h.store.GetWorkCenter(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wc)
}

// ---------------------------------------------------------------------------
// Routings
// ---------------------------------------------------------------------------

// routingOperationRequest is one step on a createRouting call. As with
// bomComponentRequest, the array position IS the operation's sequence —
// the store assigns sequence = (index + 1) — so the request omits a
// sequence field.
type routingOperationRequest struct {
	OperationName    string          `json:"operation_name"`
	WorkCenterID     uuid.UUID       `json:"work_center_id"`
	SetupTimeMinutes decimal.Decimal `json:"setup_time_minutes"`
	CycleTimeMinutes decimal.Decimal `json:"cycle_time_minutes"`
	Description      string          `json:"description,omitempty"`
}

type createRoutingRequest struct {
	ItemID     uuid.UUID                 `json:"item_id"`
	Version    string                    `json:"version"`
	Notes      string                    `json:"notes,omitempty"`
	Operations []routingOperationRequest `json:"operations"`
	Activate   bool                      `json:"activate,omitempty"`
}

func (h *manufacturingHandlers) createRouting(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	var req createRoutingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in := manufacturing.CreateRoutingInput{
		ItemID:   req.ItemID,
		Version:  req.Version,
		Notes:    req.Notes,
		Activate: req.Activate,
	}
	for _, op := range req.Operations {
		in.Operations = append(in.Operations, manufacturing.RoutingOperationInput{
			OperationName:    op.OperationName,
			WorkCenterID:     op.WorkCenterID,
			SetupTimeMinutes: op.SetupTimeMinutes,
			CycleTimeMinutes: op.CycleTimeMinutes,
			Description:      op.Description,
		})
	}
	routing, err := h.store.CreateRouting(r.Context(), t.ID, actor, in)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, routing)
}

func (h *manufacturingHandlers) listRoutings(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.ListRoutings(r.Context(), t.ID, r.URL.Query().Get("status"))
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getRouting(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid routing id", http.StatusBadRequest)
		return
	}
	routing, err := h.store.GetRouting(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, routing)
}

type setRoutingStatusRequest struct {
	Status string `json:"status"`
}

func (h *manufacturingHandlers) setRoutingStatus(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid routing id", http.StatusBadRequest)
		return
	}
	var req setRoutingStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.store.SetRoutingStatus(r.Context(), t.ID, id, req.Status); err != nil {
		writeManufacturingError(w, err)
		return
	}
	routing, err := h.store.GetRouting(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, routing)
}

// ---------------------------------------------------------------------------
// Capacity planning
// ---------------------------------------------------------------------------

// capacityPlan computes the finite-capacity utilisation grid for a date
// window. The window is supplied via ?start=YYYY-MM-DD&end=YYYY-MM-DD;
// both default to today (a single-day grid) when omitted.
func (h *manufacturingHandlers) capacityPlan(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	start, err := parseDateParam(r.URL.Query().Get("start"), today)
	if err != nil {
		http.Error(w, "invalid start date (want YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	end, err := parseDateParam(r.URL.Query().Get("end"), start)
	if err != nil {
		http.Error(w, "invalid end date (want YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	plan, err := manufacturing.NewCapacityPlanner(h.store).Plan(r.Context(), t.ID, manufacturing.DateRange{
		Start: start,
		End:   end,
	})
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// parseDateParam parses a YYYY-MM-DD query parameter, returning fallback
// when the parameter is empty.
func parseDateParam(s string, fallback time.Time) (time.Time, error) {
	if s == "" {
		return fallback, nil
	}
	return time.Parse("2006-01-02", s)
}

// ---------------------------------------------------------------------------
// Job cards (shop floor)
// ---------------------------------------------------------------------------

func (h *manufacturingHandlers) listJobCards(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	woID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid work order id", http.StatusBadRequest)
		return
	}
	out, err := h.store.ListJobCards(r.Context(), t.ID, woID)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getJobCard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "jid"))
	if err != nil {
		http.Error(w, "invalid job card id", http.StatusBadRequest)
		return
	}
	jc, err := h.store.GetJobCard(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jc)
}

func (h *manufacturingHandlers) startJobCard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "jid"))
	if err != nil {
		http.Error(w, "invalid job card id", http.StatusBadRequest)
		return
	}
	jc, err := h.store.StartJobCard(r.Context(), t.ID, id, actorOrDefault(r.Context()))
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jc)
}

// completeJobCardRequest is the operator-reported yield for a single
// shop-floor operation. An empty body completes the card with zero
// reported quantities (the card still flips to completed and, if it is
// the last open card, triggers the work-order completion with the
// work order's nominal yield).
type completeJobCardRequest struct {
	QtyProduced decimal.Decimal `json:"qty_produced,omitempty"`
	QtyRejected decimal.Decimal `json:"qty_rejected,omitempty"`
	Notes       string          `json:"notes,omitempty"`
}

func (h *manufacturingHandlers) completeJobCard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "jid"))
	if err != nil {
		http.Error(w, "invalid job card id", http.StatusBadRequest)
		return
	}
	var req completeJobCardRequest
	// Body optional — see completeWorkOrder for the chunked-encoding
	// rationale behind the ContentLength != 0 guard.
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	jc, err := h.store.CompleteJobCard(r.Context(), t.ID, id, manufacturing.CompleteJobCardInput{
		OperatorID:  actorOrDefault(r.Context()),
		QtyProduced: req.QtyProduced,
		QtyRejected: req.QtyRejected,
		Notes:       req.Notes,
	})
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jc)
}

// ---------------------------------------------------------------------------
// MRP runs
// ---------------------------------------------------------------------------

// mrpDemandRequest is one independent demand line on a runMRP call.
// due_date is a YYYY-MM-DD calendar date; source defaults to "manual".
type mrpDemandRequest struct {
	ItemID    uuid.UUID       `json:"item_id"`
	Qty       decimal.Decimal `json:"qty"`
	DueDate   string          `json:"due_date"`
	Source    string          `json:"source,omitempty"`
	SourceRef string          `json:"source_ref,omitempty"`
}

// runMRPRequest is the HTTP shape for executing an MRP run. The horizon
// bounds which demand due dates are planned; include_min_stock
// additionally tops up items below their reorder level.
type runMRPRequest struct {
	HorizonStart    string             `json:"horizon_start"`
	HorizonEnd      string             `json:"horizon_end"`
	IncludeMinStock bool               `json:"include_min_stock,omitempty"`
	BuyLeadTimeDays int                `json:"buy_lead_time_days,omitempty"`
	Notes           string             `json:"notes,omitempty"`
	Demand          []mrpDemandRequest `json:"demand,omitempty"`
}

func (h *manufacturingHandlers) runMRP(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	var req runMRPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	horizonStart, err := time.Parse("2006-01-02", req.HorizonStart)
	if err != nil {
		http.Error(w, "invalid horizon_start (want YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	horizonEnd, err := time.Parse("2006-01-02", req.HorizonEnd)
	if err != nil {
		http.Error(w, "invalid horizon_end (want YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	in := manufacturing.MRPRunInput{
		HorizonStart:    horizonStart,
		HorizonEnd:      horizonEnd,
		IncludeMinStock: req.IncludeMinStock,
		BuyLeadTimeDays: req.BuyLeadTimeDays,
		Notes:           req.Notes,
	}
	for _, d := range req.Demand {
		due, err := time.Parse("2006-01-02", d.DueDate)
		if err != nil {
			http.Error(w, "invalid demand due_date (want YYYY-MM-DD)", http.StatusBadRequest)
			return
		}
		in.Demand = append(in.Demand, manufacturing.MRPDemandInput{
			ItemID:    d.ItemID,
			Qty:       d.Qty,
			DueDate:   due,
			Source:    d.Source,
			SourceRef: d.SourceRef,
		})
	}
	run, err := h.store.RunMRP(r.Context(), t.ID, actor, in)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (h *manufacturingHandlers) listMRPRuns(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.ListMRPRuns(r.Context(), t.ID)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getMRPRun(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid mrp run id", http.StatusBadRequest)
		return
	}
	run, err := h.store.GetMRPRun(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// ---------------------------------------------------------------------------
// Subcontracting
// ---------------------------------------------------------------------------

type subcontractComponentRequest struct {
	ItemID uuid.UUID       `json:"item_id"`
	Qty    decimal.Decimal `json:"qty"`
}

type createSubcontractOrderRequest struct {
	WorkOrderID         *uuid.UUID                    `json:"work_order_id,omitempty"`
	RoutingOperationSeq *int                          `json:"routing_operation_seq,omitempty"`
	SupplierID          *uuid.UUID                    `json:"supplier_id,omitempty"`
	ItemID              uuid.UUID                     `json:"item_id"`
	WarehouseID         uuid.UUID                     `json:"warehouse_id"`
	Qty                 decimal.Decimal               `json:"qty"`
	ChargeAmount        decimal.Decimal               `json:"charge_amount,omitempty"`
	ChargeCurrency      string                        `json:"charge_currency,omitempty"`
	Notes               string                        `json:"notes,omitempty"`
	Components          []subcontractComponentRequest `json:"components"`
}

func (h *manufacturingHandlers) createSubcontractOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())
	var req createSubcontractOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	in := manufacturing.CreateSubcontractOrderInput{
		WorkOrderID:         req.WorkOrderID,
		RoutingOperationSeq: req.RoutingOperationSeq,
		SupplierID:          req.SupplierID,
		ItemID:              req.ItemID,
		WarehouseID:         req.WarehouseID,
		Qty:                 req.Qty,
		ChargeAmount:        req.ChargeAmount,
		ChargeCurrency:      req.ChargeCurrency,
		Notes:               req.Notes,
	}
	for _, c := range req.Components {
		in.Components = append(in.Components, manufacturing.SubcontractComponentInput{
			ItemID: c.ItemID,
			Qty:    c.Qty,
		})
	}
	order, err := h.store.CreateSubcontractOrder(r.Context(), t.ID, actor, in)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, order)
}

func (h *manufacturingHandlers) listSubcontractOrders(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	status := r.URL.Query().Get("status")
	out, err := h.store.ListSubcontractOrders(r.Context(), t.ID, status)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manufacturingHandlers) getSubcontractOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid subcontract order id", http.StatusBadRequest)
		return
	}
	order, err := h.store.GetSubcontractOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

// issueSubcontractOrderRequest carries optional lot/serial payloads for
// the component issue moves, keyed by component item id (UUID string).
type issueSubcontractOrderRequest struct {
	ComponentBatches map[string]string   `json:"component_batches,omitempty"`
	ComponentSerials map[string][]string `json:"component_serials,omitempty"`
}

func (h *manufacturingHandlers) issueSubcontractOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid subcontract order id", http.StatusBadRequest)
		return
	}
	var req issueSubcontractOrderRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	in := manufacturing.IssueSubcontractInput{}
	if len(req.ComponentBatches) > 0 {
		in.ComponentBatches = make(map[uuid.UUID]uuid.UUID, len(req.ComponentBatches))
		for itemStr, batchStr := range req.ComponentBatches {
			itemID, err := uuid.Parse(itemStr)
			if err != nil {
				http.Error(w, "invalid component_batches item id", http.StatusBadRequest)
				return
			}
			batchID, err := uuid.Parse(batchStr)
			if err != nil {
				http.Error(w, "invalid component_batches batch id", http.StatusBadRequest)
				return
			}
			in.ComponentBatches[itemID] = batchID
		}
	}
	if len(req.ComponentSerials) > 0 {
		in.ComponentSerials = make(map[uuid.UUID][]string, len(req.ComponentSerials))
		for itemStr, serials := range req.ComponentSerials {
			itemID, err := uuid.Parse(itemStr)
			if err != nil {
				http.Error(w, "invalid component_serials item id", http.StatusBadRequest)
				return
			}
			in.ComponentSerials[itemID] = serials
		}
	}
	order, err := h.store.IssueSubcontractOrder(r.Context(), t.ID, actorOrDefault(r.Context()), id, in)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

// receiveSubcontractOrderRequest carries the received quantity (defaults
// to the order quantity) and optional lot/serial payload for the
// finished item receipt move.
type receiveSubcontractOrderRequest struct {
	ActualQty       *decimal.Decimal `json:"actual_qty,omitempty"`
	FinishedBatchID *uuid.UUID       `json:"finished_batch_id,omitempty"`
	FinishedSerials []string         `json:"finished_serials,omitempty"`
}

func (h *manufacturingHandlers) receiveSubcontractOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid subcontract order id", http.StatusBadRequest)
		return
	}
	var req receiveSubcontractOrderRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	order, err := h.store.ReceiveSubcontractOrder(r.Context(), t.ID, actorOrDefault(r.Context()), id, manufacturing.ReceiveSubcontractInput{
		ActualQty:       req.ActualQty,
		FinishedBatchID: req.FinishedBatchID,
		FinishedSerials: req.FinishedSerials,
	})
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *manufacturingHandlers) closeSubcontractOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid subcontract order id", http.StatusBadRequest)
		return
	}
	order, err := h.store.CloseSubcontractOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *manufacturingHandlers) cancelSubcontractOrder(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid subcontract order id", http.StatusBadRequest)
		return
	}
	order, err := h.store.CancelSubcontractOrder(r.Context(), t.ID, id)
	if err != nil {
		writeManufacturingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}
