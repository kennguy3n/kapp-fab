package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// tenantHandlers groups the control-plane tenant HTTP handlers against a
// tenant.PGStore implementation. These routes are control-plane: they manage
// the tenant registry itself and are not subject to TenantMiddleware.
type tenantHandlers struct {
	svc *tenant.PGStore
}

type createTenantRequest struct {
	Slug  string          `json:"slug"`
	Name  string          `json:"name"`
	Cell  string          `json:"cell"`
	Plan  string          `json:"plan"`
	Quota json.RawMessage `json:"quota"`
}

func (h *tenantHandlers) create(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	t, err := h.svc.Create(r.Context(), tenant.CreateInput{
		Slug:  req.Slug,
		Name:  req.Name,
		Cell:  req.Cell,
		Plan:  req.Plan,
		Quota: req.Quota,
	})
	if err != nil {
		if errors.Is(err, tenant.ErrSlugTaken) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *tenantHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	t, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// tenantOp abstracts the suspend/archive/delete operations which share the
// same (ctx, id) → error signature so that runTransition can dispatch to any
// of them.
type tenantOp func(ctx context.Context, id uuid.UUID) error

func (h *tenantHandlers) suspend(w http.ResponseWriter, r *http.Request) {
	h.runTransition(w, r, h.svc.Suspend)
}

func (h *tenantHandlers) archive(w http.ResponseWriter, r *http.Request) {
	h.runTransition(w, r, h.svc.Archive)
}

func (h *tenantHandlers) delete(w http.ResponseWriter, r *http.Request) {
	h.runTransition(w, r, h.svc.Delete)
}

func (h *tenantHandlers) runTransition(w http.ResponseWriter, r *http.Request, op tenantOp) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	if err := op(r.Context(), id); err != nil {
		h.writeTenantError(w, err)
		return
	}
	t, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.writeTenantError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *tenantHandlers) writeTenantError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrNotFound):
		http.Error(w, "tenant not found", http.StatusNotFound)
	case errors.Is(err, tenant.ErrInvalidTransition):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
