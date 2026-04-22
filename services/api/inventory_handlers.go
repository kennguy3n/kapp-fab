package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// inventoryHandlers exposes the Phase D inventory HTTP surface: item
// and warehouse masters, the append-only stock-move ledger, derived
// stock levels, and the valuation report. Tenant scope is enforced by
// the middleware stack wired in main.go; the handlers translate HTTP
// into inventory calls and map sentinel errors to the status codes the
// web client expects.
type inventoryHandlers struct {
	store *inventory.PGStore
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

type upsertItemRequest struct {
	ID     *uuid.UUID `json:"id,omitempty"`
	SKU    string     `json:"sku"`
	Name   string     `json:"name"`
	UOM    string     `json:"uom"`
	Active *bool      `json:"active"`
}

func (h *inventoryHandlers) upsertItem(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req upsertItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.SKU == "" {
		http.Error(w, "sku is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.UOM == "" {
		http.Error(w, "uom is required", http.StatusBadRequest)
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	in := inventory.Item{
		TenantID: t.ID,
		SKU:      req.SKU,
		Name:     req.Name,
		UOM:      req.UOM,
		Active:   active,
	}
	if req.ID != nil {
		in.ID = *req.ID
	}
	out, err := h.store.UpsertItem(r.Context(), in)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *inventoryHandlers) listItems(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	filter := inventory.ItemFilter{Limit: limit, Offset: offset}
	if raw := r.URL.Query().Get("active"); raw != "" {
		if v, err := strconv.ParseBool(raw); err == nil {
			filter.Active = &v
		}
	}
	items, err := h.store.ListItems(r.Context(), t.ID, filter)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *inventoryHandlers) getItem(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid item id", http.StatusBadRequest)
		return
	}
	it, err := h.store.GetItem(r.Context(), t.ID, id)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// ---------------------------------------------------------------------------
// Warehouses
// ---------------------------------------------------------------------------

type upsertWarehouseRequest struct {
	ID   *uuid.UUID `json:"id,omitempty"`
	Code string     `json:"code"`
	Name string     `json:"name"`
}

func (h *inventoryHandlers) upsertWarehouse(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req upsertWarehouseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	in := inventory.Warehouse{
		TenantID: t.ID,
		Code:     req.Code,
		Name:     req.Name,
	}
	if req.ID != nil {
		in.ID = *req.ID
	}
	out, err := h.store.UpsertWarehouse(r.Context(), in)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *inventoryHandlers) listWarehouses(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	whs, err := h.store.ListWarehouses(r.Context(), t.ID)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, whs)
}

// ---------------------------------------------------------------------------
// Moves
// ---------------------------------------------------------------------------

type recordMoveRequest struct {
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitCost    decimal.Decimal `json:"unit_cost"`
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID      `json:"source_id,omitempty"`
	MovedAt     *time.Time      `json:"moved_at,omitempty"`
}

func (h *inventoryHandlers) recordMove(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req recordMoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	m := inventory.Move{
		TenantID:    t.ID,
		ItemID:      req.ItemID,
		WarehouseID: req.WarehouseID,
		Qty:         req.Qty,
		UnitCost:    req.UnitCost,
		SourceKType: req.SourceKType,
		SourceID:    req.SourceID,
		CreatedBy:   actorOrDefault(r.Context()),
	}
	if req.MovedAt != nil {
		m.MovedAt = *req.MovedAt
	}
	out, err := h.store.RecordMove(r.Context(), m)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type recordTransferRequest struct {
	ItemID        uuid.UUID       `json:"item_id"`
	FromWarehouse uuid.UUID       `json:"from_warehouse_id"`
	ToWarehouse   uuid.UUID       `json:"to_warehouse_id"`
	Qty           decimal.Decimal `json:"qty"`
	UnitCost      decimal.Decimal `json:"unit_cost"`
	MovedAt       *time.Time      `json:"moved_at,omitempty"`
	Memo          string          `json:"memo,omitempty"`
}

func (h *inventoryHandlers) recordTransfer(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req recordTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	tr := inventory.Transfer{
		TenantID:      t.ID,
		ItemID:        req.ItemID,
		FromWarehouse: req.FromWarehouse,
		ToWarehouse:   req.ToWarehouse,
		Qty:           req.Qty,
		UnitCost:      req.UnitCost,
		Memo:          req.Memo,
		CreatedBy:     actorOrDefault(r.Context()),
	}
	if req.MovedAt != nil {
		tr.MovedAt = *req.MovedAt
	}
	moves, err := h.store.RecordTransfer(r.Context(), tr)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, moves)
}

func (h *inventoryHandlers) listMoves(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	filter := inventory.MoveFilter{Limit: limit, Offset: offset}
	if raw := r.URL.Query().Get("item_id"); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			filter.ItemID = &id
		}
	}
	if raw := r.URL.Query().Get("warehouse_id"); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			filter.WarehouseID = &id
		}
	}
	if raw := r.URL.Query().Get("source_ktype"); raw != "" {
		filter.SourceKType = raw
	}
	if raw := r.URL.Query().Get("source_id"); raw != "" {
		if id, err := uuid.Parse(raw); err == nil {
			filter.SourceID = &id
		}
	}
	moves, err := h.store.ListMoves(r.Context(), t.ID, filter)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, moves)
}

// ---------------------------------------------------------------------------
// Stock levels + valuation
// ---------------------------------------------------------------------------

func (h *inventoryHandlers) listStockLevels(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	levels, err := h.store.ListStockLevels(r.Context(), t.ID, nil)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, levels)
}

func (h *inventoryHandlers) stockLevelsByItem(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid item id", http.StatusBadRequest)
		return
	}
	levels, err := h.store.ListStockLevels(r.Context(), t.ID, &id)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, levels)
}

func (h *inventoryHandlers) valuation(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	asOf := parseEndOfDayParam(r.URL.Query().Get("as_of"), time.Now().UTC())
	rep, err := h.store.Valuation(r.Context(), t.ID, asOf)
	if err != nil {
		writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// writeInventoryError translates inventory sentinel errors into the
// HTTP status codes the web client keys off for user-facing messaging.
func writeInventoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inventory.ErrItemNotFound),
		errors.Is(err, inventory.ErrWarehouseNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, inventory.ErrDuplicateSourceMove):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, inventory.ErrMoveInvalid),
		errors.Is(err, inventory.ErrTransferUnbalanced):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeRecordError(w, err)
	}
}
