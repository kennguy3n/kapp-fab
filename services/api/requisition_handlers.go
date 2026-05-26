package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/sales"
)

// requisitionHandlers wires Phase N9b's procurement.purchase_requisition
// state-machine onto HTTP. CRUD on the requisition KRecord itself
// rides the generic /api/v1/records/procurement.purchase_requisition
// surface; this handler exposes the three non-CRUD lifecycle
// transitions (approve / convert / cancel). Every transition is
// idempotent on the underlying record (status check) and on the HTTP
// request (Idempotency-Key middleware), so retries from a flaky
// client cannot allocate duplicate purchase orders.
type requisitionHandlers struct {
	poster *sales.RequisitionPoster
}

// requisitionTransitionFn is the shared shape of every state-machine
// transition on RequisitionPoster. Lifting the lookup + auth context
// + JSON serialisation onto one helper keeps the URL/handler glue
// identical across approve / convert / cancel.
type requisitionTransitionFn func(ctx context.Context, tenantID, requisitionID, actorID uuid.UUID) (*record.KRecord, error)

func (h *requisitionHandlers) approve(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Approve)
}

func (h *requisitionHandlers) convert(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Convert)
}

func (h *requisitionHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Cancel)
}

func (h *requisitionHandlers) transition(w http.ResponseWriter, r *http.Request, fn requisitionTransitionFn) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid requisition id", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	rec, err := fn(r.Context(), t.ID, id, actor)
	if err != nil {
		writeRequisitionError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// writeRequisitionError translates RequisitionPoster sentinels into
// the appropriate HTTP status codes the web client expects. The
// contract is sentinel-only: a 422 ("Unprocessable Entity") response
// must be backed by a typed error declared in the sales package and
// matched by errors.Is here. Unmatched errors fall through to
// writeRecordError which handles record-layer sentinels and defaults
// to 500 — we deliberately do NOT inspect err.Error() text to
// classify errors. A string heuristic (e.g. strings.Contains("required"))
// can misfire on internal failures whose underlying message happens
// to contain a matching word, surfacing them to the client as 422
// when they should be 500. If a future poster validation needs to
// produce a 422, add a sentinel to sales/ and a case here.
func writeRequisitionError(w http.ResponseWriter, err error) {
	if err == nil {
		http.Error(w, "unknown error", http.StatusInternalServerError)
		return
	}
	switch {
	case errors.Is(err, sales.ErrRequisitionNotFound), errors.Is(err, record.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, record.ErrVersionConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, sales.ErrInvalidRequisitionState):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, sales.ErrNotRequisition):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, sales.ErrRequisitionNoLines),
		errors.Is(err, sales.ErrRequisitionNoSupplier):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		writeRecordError(w, err)
	}
}
