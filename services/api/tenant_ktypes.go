package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// tenantKTypeHandlers implements the HTTP surface for Phase N8b
// (low-code) tenant-authored custom KTypes. The route group is
// tenant-scoped so the tenant GUC is set on every call by the
// middleware chain — `store.Upsert`/`Get`/`List` then enforce RLS
// at the DB layer.
type tenantKTypeHandlers struct {
	store *ktype.TenantStore
}

// upsertTenantKTypeRequest is the JSON shape the builder UI POSTs
// when saving a custom KType. The schema is opaque (we forward it
// to ktype.TenantStore.validateCustomSchema for the safe-subset
// check and the field cap) so adding a new safe field type is a
// single-line change in tenant_store.go without touching this
// handler.
type upsertTenantKTypeRequest struct {
	Name        string          `json:"name"`
	Version     int             `json:"version"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Status      string          `json:"status"`
}

// upsert handles POST /api/v1/tenant-ktypes (create OR replace a
// custom KType). The middleware stack has already authenticated
// the request and set app.tenant_id; the handler just shapes the
// payload, calls the store, and returns the persisted row.
func (h *tenantKTypeHandlers) upsert(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	tenantID := t.ID
	actorID := actorOrDefault(r.Context())
	var req upsertTenantKTypeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	saved, err := h.store.Upsert(r.Context(), ktype.TenantKType{
		TenantID:    tenantID,
		Name:        req.Name,
		Version:     req.Version,
		Title:       req.Title,
		Description: req.Description,
		Schema:      req.Schema,
		Status:      req.Status,
		CreatedBy:   actorID,
	})
	if err != nil {
		switch {
		case errors.Is(err, ktype.ErrInvalidCustomName),
			errors.Is(err, ktype.ErrTooManyFields),
			errors.Is(err, ktype.ErrUnsupportedFieldType):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// list handles GET /api/v1/tenant-ktypes. Returns every custom
// KType for the active tenant, ordered by name. Drafts /
// archived rows are included so the builder UI can render the
// full set.
func (h *tenantKTypeHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rows, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       rows,
		"field_limit": h.store.FieldLimit(),
	})
}

// get handles GET /api/v1/tenant-ktypes/{name}. Optional
// ?version=N pins the lookup to a specific version; omitting it
// returns the latest.
func (h *tenantKTypeHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	tenantID := t.ID
	name := chi.URLParam(r, "name")
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid version", http.StatusBadRequest)
			return
		}
		version = parsed
	}
	row, err := h.store.Get(r.Context(), tenantID, name, version)
	if err != nil {
		if errors.Is(err, ktype.ErrNotFound) {
			http.Error(w, "custom ktype not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// setStatusRequest carries the new status — POST body for
// /api/v1/tenant-ktypes/{name}/status?version=N.
type setStatusRequest struct {
	Status string `json:"status"`
}

// setStatus handles POST /api/v1/tenant-ktypes/{name}/status —
// transitions the KType to draft / active / archived. Surfaces a
// 400 on unknown statuses and a 404 when the named version
// doesn't exist.
func (h *tenantKTypeHandlers) setStatus(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	tenantID := t.ID
	name := chi.URLParam(r, "name")
	version := 0
	if v := r.URL.Query().Get("version"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid version", http.StatusBadRequest)
			return
		}
		version = parsed
	}
	if version == 0 {
		http.Error(w, "version required", http.StatusBadRequest)
		return
	}
	var req setStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := h.store.SetStatus(r.Context(), tenantID, name, version, req.Status); err != nil {
		if errors.Is(err, ktype.ErrNotFound) {
			http.Error(w, "custom ktype not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    name,
		"version": version,
		"status":  req.Status,
	})
}
