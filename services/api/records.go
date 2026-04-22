package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

type recordHandlers struct {
	store *record.PGStore
}

type createRecordRequest struct {
	Data json.RawMessage `json:"data"`
}

func (h *recordHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	created, err := h.store.Create(r.Context(), record.KRecord{
		TenantID:  t.ID,
		KType:     chi.URLParam(r, "ktype"),
		Data:      req.Data,
		CreatedBy: actorOrDefault(r.Context()),
	})
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *recordHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	records, err := h.store.List(r.Context(), t.ID, record.ListFilter{
		KType:  chi.URLParam(r, "ktype"),
		Status: r.URL.Query().Get("status"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

func (h *recordHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid record id", http.StatusBadRequest)
		return
	}
	rec, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

type updateRecordRequest struct {
	Data    json.RawMessage `json:"data"`
	Version int             `json:"version"`
}

func (h *recordHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid record id", http.StatusBadRequest)
		return
	}
	var req updateRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	updated, err := h.store.Update(r.Context(), record.KRecord{
		TenantID:  t.ID,
		ID:        id,
		Data:      req.Data,
		Version:   req.Version,
		UpdatedBy: &actor,
	})
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *recordHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid record id", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), t.ID, id); err != nil {
		writeRecordError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeRecordError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, record.ErrNotFound), errors.Is(err, ktype.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, record.ErrVersionConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		var verrs ktype.ValidationErrors
		if errors.As(err, &verrs) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":  "validation failed",
				"fields": verrs,
			})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// actorOrDefault returns the user id from context or a zero-uuid sentinel so
// Phase A handlers work without a real auth layer. A later work unit will
// install a real auth middleware that sets a real user id on the context.
func actorOrDefault(ctx context.Context) uuid.UUID {
	if id := platform.UserIDFromContext(ctx); id != uuid.Nil {
		return id
	}
	return uuid.UUID{}
}
