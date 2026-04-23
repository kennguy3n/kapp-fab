package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/base"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// baseHandlers exposes the Phase F Base KApp surface — ad-hoc tables
// plus the rows that live inside them. Every handler pulls the tenant
// from request context so there is no way for a caller to address
// another tenant's tables; the Base store additionally runs every
// query under `SET LOCAL app.tenant_id` so the RLS policy denies
// cross-tenant rows even if a handler bug leaked an id.
type baseHandlers struct {
	store *base.Store
}

type baseTableRequest struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Columns     []base.Column   `json:"columns,omitempty"`
	SharedView  json.RawMessage `json:"shared_view,omitempty"`
}

func (h *baseHandlers) createTable(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req baseTableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	tbl, err := h.store.CreateTable(r.Context(), base.Table{
		TenantID:    t.ID,
		Slug:        req.Slug,
		Name:        req.Name,
		Description: req.Description,
		Columns:     req.Columns,
		SharedView:  req.SharedView,
		CreatedBy:   actorOrDefault(r.Context()),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, tbl)
}

func (h *baseHandlers) listTables(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	tbls, err := h.store.ListTables(r.Context(), t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tbls == nil {
		tbls = []base.Table{}
	}
	writeJSON(w, http.StatusOK, tbls)
}

func (h *baseHandlers) getTable(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid table id", http.StatusBadRequest)
		return
	}
	tbl, err := h.store.GetTable(r.Context(), t.ID, id)
	if err != nil {
		writeBaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tbl)
}

func (h *baseHandlers) updateTable(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid table id", http.StatusBadRequest)
		return
	}
	var req baseTableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	tbl, err := h.store.UpdateTable(r.Context(), base.Table{
		TenantID:    t.ID,
		ID:          id,
		Name:        req.Name,
		Description: req.Description,
		Columns:     req.Columns,
		SharedView:  req.SharedView,
	})
	if err != nil {
		writeBaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tbl)
}

type baseRowRequest struct {
	Data json.RawMessage `json:"data"`
}

func (h *baseHandlers) createRow(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	tableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid table id", http.StatusBadRequest)
		return
	}
	var req baseRowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	row, err := h.store.CreateRow(r.Context(), base.Row{
		TenantID:  t.ID,
		TableID:   tableID,
		Data:      req.Data,
		CreatedBy: actorOrDefault(r.Context()),
	})
	if err != nil {
		writeBaseError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *baseHandlers) listRows(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	tableID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid table id", http.StatusBadRequest)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	rows, err := h.store.ListRows(r.Context(), t.ID, tableID, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []base.Row{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (h *baseHandlers) updateRow(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rowID, err := uuid.Parse(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "invalid row id", http.StatusBadRequest)
		return
	}
	var req baseRowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	row, err := h.store.UpdateRow(r.Context(), base.Row{
		TenantID: t.ID,
		ID:       rowID,
		Data:     req.Data,
	})
	if err != nil {
		writeBaseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (h *baseHandlers) deleteRow(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rowID, err := uuid.Parse(chi.URLParam(r, "rowID"))
	if err != nil {
		http.Error(w, "invalid row id", http.StatusBadRequest)
		return
	}
	if err := h.store.DeleteRow(r.Context(), t.ID, rowID); err != nil {
		writeBaseError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeBaseError(w http.ResponseWriter, err error) {
	if errors.Is(err, base.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
