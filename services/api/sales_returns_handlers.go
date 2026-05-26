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

// salesReturnsHandlers wires Phase N9a's sales.return state-machine
// onto HTTP. CRUD on the return KRecord itself rides the generic
// /api/v1/records/sales.return surface; this handler exposes the
// four non-CRUD lifecycle transitions (approve / receive / refund /
// cancel). Every transition is idempotent on the underlying record
// (status check) and on the HTTP request (Idempotency-Key
// middleware), so retries from a flaky client cannot double-post
// inventory moves or credit notes.
type salesReturnsHandlers struct {
	poster *sales.ReturnPoster
}

// transitionFn is the shared shape of every state-machine transition
// on ReturnPoster. Lifting the lookup + auth context + JSON
// serialisation onto one helper keeps the URL/handler glue identical
// across approve / receive / refund / cancel.
type transitionFn func(ctx context.Context, tenantID, returnID, actorID uuid.UUID) (*record.KRecord, error)

func (h *salesReturnsHandlers) approve(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Approve)
}

func (h *salesReturnsHandlers) receive(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Receive)
}

func (h *salesReturnsHandlers) refund(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Refund)
}

func (h *salesReturnsHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, h.poster.Cancel)
}

func (h *salesReturnsHandlers) transition(w http.ResponseWriter, r *http.Request, fn transitionFn) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid return id", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	rec, err := fn(r.Context(), t.ID, id, actor)
	if err != nil {
		writeReturnError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// writeReturnError translates ReturnPoster sentinels into the
// appropriate HTTP status codes the web client expects.
func writeReturnError(w http.ResponseWriter, err error) {
	if err == nil {
		http.Error(w, "unknown error", http.StatusInternalServerError)
		return
	}
	switch {
	case errors.Is(err, sales.ErrReturnNotFound), errors.Is(err, record.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, record.ErrVersionConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, sales.ErrInvalidReturnState):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, sales.ErrNotReturn):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, sales.ErrReturnAmountZero):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, sales.ErrReturnInvalidInput):
		// Validation failures inside ReturnPoster (missing
		// warehouse_id, empty lines, missing original_invoice_id,
		// etc.) all wrap ErrReturnInvalidInput. The sentinel-based
		// dispatch replaces an earlier strings.Contains(msg,
		// "required") heuristic that could mis-classify a
		// transient DB error whose message happened to contain
		// "required" as a 422 client error.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		writeRecordError(w, err)
	}
}
