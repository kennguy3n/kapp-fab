package main

// Phase N9c — Landed Cost HTTP surface.
//
// Routes mounted at /api/v1/finance/landed-costs:
//
//   GET    /                      ListVouchers (filter by status)
//   POST   /                      CreateVoucher (header only)
//   GET    /{id}                  GetVoucher (with charges + targets)
//   PUT    /{id}                  UpdateVoucher (metadata only)
//   DELETE /{id}                  DeleteVoucher (draft only)
//   POST   /{id}/charges          UpsertCharge
//   DELETE /{id}/charges/{cid}    DeleteCharge
//   POST   /{id}/targets          UpsertTarget
//   DELETE /{id}/targets/{tid}    DeleteTarget
//   POST   /{id}/allocate         AllocateVoucher (compute shares)
//   POST   /{id}/post             PostVoucher (write moves + JE)

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

type landedCostHandlers struct {
	store *finance.LandedCostStore
}

// ---------------------------------------------------------------------------
// Voucher CRUD
// ---------------------------------------------------------------------------

type voucherRequest struct {
	VoucherNumber    string `json:"voucher_number"`
	Description      string `json:"description"`
	AllocationMethod string `json:"allocation_method"`
}

type voucherResponse struct {
	Voucher finance.LandedCostVoucher  `json:"voucher"`
	Charges []finance.LandedCostCharge `json:"charges"`
	Targets []finance.LandedCostTarget `json:"targets"`
}

func (h *landedCostHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	filter := finance.LandedCostFilter{
		Status: r.URL.Query().Get("status"),
	}
	out, err := h.store.ListVouchers(r.Context(), t.ID, filter)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	if out == nil {
		out = []finance.LandedCostVoucher{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *landedCostHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req voucherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.VoucherNumber == "" {
		http.Error(w, "voucher_number is required", http.StatusBadRequest)
		return
	}
	out, err := h.store.CreateVoucher(r.Context(), finance.LandedCostVoucher{
		TenantID:         t.ID,
		VoucherNumber:    req.VoucherNumber,
		Description:      req.Description,
		AllocationMethod: req.AllocationMethod,
		CreatedBy:        actorOrDefault(r.Context()),
	})
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *landedCostHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	voucher, err := h.store.GetVoucher(r.Context(), t.ID, id)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	charges, err := h.store.ListCharges(r.Context(), t.ID, id)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	targets, err := h.store.ListTargets(r.Context(), t.ID, id)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, voucherResponse{
		Voucher: *voucher,
		Charges: charges,
		Targets: targets,
	})
}

func (h *landedCostHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req voucherRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.VoucherNumber == "" {
		http.Error(w, "voucher_number is required", http.StatusBadRequest)
		return
	}
	out, err := h.store.UpdateVoucher(r.Context(), finance.LandedCostVoucher{
		TenantID:         t.ID,
		ID:               id,
		VoucherNumber:    req.VoucherNumber,
		Description:      req.Description,
		AllocationMethod: req.AllocationMethod,
	})
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *landedCostHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := h.store.DeleteVoucher(r.Context(), t.ID, id); err != nil {
		writeLandedCostError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Charges
// ---------------------------------------------------------------------------

type chargeRequest struct {
	ID          *uuid.UUID      `json:"id,omitempty"`
	Description string          `json:"description"`
	Amount      decimal.Decimal `json:"amount"`
	AccountCode string          `json:"account_code"`
}

func (h *landedCostHandlers) upsertCharge(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	voucherID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req chargeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	c := finance.LandedCostCharge{
		TenantID:    t.ID,
		VoucherID:   voucherID,
		Description: req.Description,
		Amount:      req.Amount,
		AccountCode: req.AccountCode,
	}
	if req.ID != nil {
		c.ID = *req.ID
	}
	out, err := h.store.UpsertCharge(r.Context(), c)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *landedCostHandlers) deleteCharge(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	voucherID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	chargeID, ok := parseUUIDParam(w, r, "cid")
	if !ok {
		return
	}
	if err := h.store.DeleteCharge(r.Context(), t.ID, voucherID, chargeID); err != nil {
		writeLandedCostError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

type targetRequest struct {
	ID          *uuid.UUID      `json:"id,omitempty"`
	SourceKType string          `json:"source_ktype"`
	SourceID    uuid.UUID       `json:"source_id"`
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitCost    decimal.Decimal `json:"unit_cost"`
	Weight      decimal.Decimal `json:"weight"`
}

func (h *landedCostHandlers) upsertTarget(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	voucherID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req targetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	target := finance.LandedCostTarget{
		TenantID:    t.ID,
		VoucherID:   voucherID,
		SourceKType: req.SourceKType,
		SourceID:    req.SourceID,
		ItemID:      req.ItemID,
		WarehouseID: req.WarehouseID,
		Qty:         req.Qty,
		UnitCost:    req.UnitCost,
		Weight:      req.Weight,
	}
	if req.ID != nil {
		target.ID = *req.ID
	}
	out, err := h.store.UpsertTarget(r.Context(), target)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *landedCostHandlers) deleteTarget(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	voucherID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	targetID, ok := parseUUIDParam(w, r, "tid")
	if !ok {
		return
	}
	if err := h.store.DeleteTarget(r.Context(), t.ID, voucherID, targetID); err != nil {
		writeLandedCostError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Allocate + Post
// ---------------------------------------------------------------------------

func (h *landedCostHandlers) allocate(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	voucherID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	out, err := h.store.AllocateVoucher(r.Context(), t.ID, voucherID)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type postResponse struct {
	Voucher *finance.LandedCostVoucher       `json:"voucher"`
	JE      *finance.LandedCostJournalEntry  `json:"journal_entry"`
}

func (h *landedCostHandlers) post(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	voucherID, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	actor := actorOrDefault(r.Context())
	voucher, je, err := h.store.PostVoucher(r.Context(), t.ID, voucherID, actor)
	if err != nil {
		writeLandedCostError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, postResponse{Voucher: voucher, JE: je})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, name+" must be a valid UUID", http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

func writeLandedCostError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, finance.ErrLandedCostNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, finance.ErrLandedCostAlreadyPosted),
		errors.Is(err, finance.ErrLandedCostNotAllocated),
		errors.Is(err, finance.ErrLandedCostNoCharges),
		errors.Is(err, finance.ErrLandedCostNoTargets),
		errors.Is(err, finance.ErrLandedCostBadMethod),
		errors.Is(err, finance.ErrLandedCostZeroWeightTotal):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
