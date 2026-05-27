package main

// Phase N9d — Cycle Count HTTP surface.
//
// Routes mounted at /api/v1/inventory/cycle-counts:
//
//   GET    /                              ListSessions (filter by status/warehouse)
//   POST   /                              CreateSession
//   GET    /{id}                          GetSession (with lines)
//   PUT    /{id}                          UpdateSession (metadata + status transition)
//   DELETE /{id}                          DeleteSession (draft only)
//   POST   /{id}/seed                     SeedExpectedFromStock (refresh expected_qty)
//   POST   /{id}/lines                    UpsertLine
//   DELETE /{id}/lines/{lid}              DeleteLine
//   POST   /{id}/post                     PostSession (write variance moves)

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

type cycleCountHandlers struct {
	store *inventory.CycleCountStore
}

// ---------------------------------------------------------------------------
// Session CRUD
// ---------------------------------------------------------------------------

type cycleCountSessionRequest struct {
	Code        string    `json:"code"`
	Description string    `json:"description"`
	WarehouseID uuid.UUID `json:"warehouse_id"`
	Status      string    `json:"status,omitempty"`
}

type cycleCountSessionResponse struct {
	Session inventory.CycleCountSession `json:"session"`
	Lines   []inventory.CycleCountLine  `json:"lines"`
}

func (h *cycleCountHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	q := r.URL.Query()
	filter := inventory.CycleCountFilter{
		Status: q.Get("status"),
	}
	if wh := q.Get("warehouse_id"); wh != "" {
		id, err := uuid.Parse(wh)
		if err != nil {
			http.Error(w, "invalid warehouse_id", http.StatusBadRequest)
			return
		}
		filter.WarehouseID = id
	}
	out, err := h.store.ListSessions(r.Context(), t.ID, filter)
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	if out == nil {
		out = []inventory.CycleCountSession{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *cycleCountHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := platform.UserIDFromContext(r.Context())
	var req cycleCountSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.CreateSession(r.Context(), inventory.CycleCountSession{
		TenantID:    t.ID,
		Code:        req.Code,
		Description: req.Description,
		WarehouseID: req.WarehouseID,
		CreatedBy:   actor,
	})
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *cycleCountHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	session, err := h.store.GetSession(r.Context(), t.ID, id)
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	lines, err := h.store.ListLines(r.Context(), t.ID, id)
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	if lines == nil {
		lines = []inventory.CycleCountLine{}
	}
	writeJSON(w, http.StatusOK, cycleCountSessionResponse{
		Session: *session,
		Lines:   lines,
	})
}

func (h *cycleCountHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	var req cycleCountSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.UpdateSession(r.Context(), inventory.CycleCountSession{
		TenantID:    t.ID,
		ID:          id,
		Code:        req.Code,
		Description: req.Description,
		WarehouseID: req.WarehouseID,
		Status:      req.Status,
	})
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *cycleCountHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteSession(r.Context(), t.ID, id); err != nil {
		writeCycleCountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *cycleCountHandlers) seed(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	if err := h.store.SeedExpectedFromStock(r.Context(), t.ID, id); err != nil {
		writeCycleCountError(w, err)
		return
	}
	lines, err := h.store.ListLines(r.Context(), t.ID, id)
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	if lines == nil {
		lines = []inventory.CycleCountLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}

// ---------------------------------------------------------------------------
// Lines
// ---------------------------------------------------------------------------

type cycleCountLineRequest struct {
	ID          uuid.UUID       `json:"id,omitempty"`
	ItemID      uuid.UUID       `json:"item_id"`
	ExpectedQty decimal.Decimal `json:"expected_qty"`
	CountedQty  decimal.Decimal `json:"counted_qty"`
	Notes       string          `json:"notes"`
}

func (h *cycleCountHandlers) upsertLine(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	var req cycleCountLineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.UpsertLine(r.Context(), inventory.CycleCountLine{
		TenantID:    t.ID,
		ID:          req.ID,
		SessionID:   sessionID,
		ItemID:      req.ItemID,
		ExpectedQty: req.ExpectedQty,
		CountedQty:  req.CountedQty,
		Notes:       req.Notes,
	})
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *cycleCountHandlers) deleteLine(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	lineID, err := uuid.Parse(chi.URLParam(r, "lid"))
	if err != nil {
		http.Error(w, "invalid line id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteLine(r.Context(), t.ID, sessionID, lineID); err != nil {
		writeCycleCountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Post
// ---------------------------------------------------------------------------

func (h *cycleCountHandlers) post(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := platform.UserIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid session id", http.StatusBadRequest)
		return
	}
	out, err := h.store.PostSession(r.Context(), t.ID, id, actor)
	if err != nil {
		writeCycleCountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeCycleCountError maps store sentinels to 4xx HTTP responses.
func writeCycleCountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inventory.ErrCycleCountNotFound),
		errors.Is(err, inventory.ErrCycleCountLineNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, inventory.ErrCycleCountAlreadyPosted),
		errors.Is(err, inventory.ErrCycleCountLineFrozen),
		errors.Is(err, inventory.ErrCycleCountDuplicateCode):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, inventory.ErrCycleCountBadStatus),
		errors.Is(err, inventory.ErrCycleCountNotReconciled),
		errors.Is(err, inventory.ErrCycleCountWarehouseEmpty),
		errors.Is(err, inventory.ErrCycleCountCodeEmpty):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		writeInventoryError(w, err)
	}
}
