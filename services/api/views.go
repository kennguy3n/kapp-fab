package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// viewHandlers serves /api/v1/views — per-user, per-KType saved
// filter/sort/column layouts that the RecordListPage uses to
// persist operator dashboards across sessions.
type viewHandlers struct {
	store *record.ViewStore
}

type upsertViewRequest struct {
	KType     string          `json:"ktype"`
	Name      string          `json:"name"`
	Filters   json.RawMessage `json:"filters"`
	Sort      string          `json:"sort"`
	Columns   json.RawMessage `json:"columns"`
	IsDefault bool            `json:"is_default"`
	Shared    bool            `json:"shared"`
}

// list returns the saved views visible to the caller for ?ktype=.
// Visibility: the caller's own views plus any view another user in
// the tenant has flagged `shared`.
func (h *viewHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	ktype := r.URL.Query().Get("ktype")
	if ktype == "" {
		http.Error(w, "ktype query parameter required", http.StatusBadRequest)
		return
	}
	views, err := h.store.List(r.Context(), t.ID, actorOrDefault(r.Context()), ktype)
	if err != nil {
		writeViewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// create persists a new saved view. The caller is always the owner.
func (h *viewHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req upsertViewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.KType == "" {
		http.Error(w, "ktype is required", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	out, err := h.store.Create(r.Context(), record.SavedView{
		TenantID:  t.ID,
		UserID:    actorOrDefault(r.Context()),
		KType:     req.KType,
		Name:      req.Name,
		Filters:   req.Filters,
		Sort:      req.Sort,
		Columns:   req.Columns,
		IsDefault: req.IsDefault,
		Shared:    req.Shared,
	})
	if err != nil {
		writeViewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// get returns one view by id, enforcing tenant + visibility via the
// underlying store (RLS + WHERE clause).
func (h *viewHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid view id", http.StatusBadRequest)
		return
	}
	v, err := h.store.Get(r.Context(), t.ID, actorOrDefault(r.Context()), id)
	if err != nil {
		writeViewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type patchViewRequest struct {
	Name      *string         `json:"name,omitempty"`
	Filters   json.RawMessage `json:"filters,omitempty"`
	Sort      *string         `json:"sort,omitempty"`
	Columns   json.RawMessage `json:"columns,omitempty"`
	IsDefault *bool           `json:"is_default,omitempty"`
	Shared    *bool           `json:"shared,omitempty"`
}

// update applies a partial patch; only the owner may mutate.
func (h *viewHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid view id", http.StatusBadRequest)
		return
	}
	var req patchViewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.store.Update(r.Context(), t.ID, actorOrDefault(r.Context()), id, record.ViewPatch{
		Name:      req.Name,
		Filters:   req.Filters,
		Sort:      req.Sort,
		Columns:   req.Columns,
		IsDefault: req.IsDefault,
		Shared:    req.Shared,
	})
	if err != nil {
		writeViewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// delete removes a view; owner-only, same argument as update.
func (h *viewHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid view id", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), t.ID, actorOrDefault(r.Context()), id); err != nil {
		writeViewError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeViewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, record.ErrViewNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
