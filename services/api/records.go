package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
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
	if err := h.store.Delete(r.Context(), t.ID, id, actorOrDefault(r.Context())); err != nil {
		writeRecordError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bulkRecordRequest is the payload accepted by POST /records/{ktype}/bulk.
// Action selects the operation applied to the selected ids; Payload
// carries the action-specific input — a merge-patch for status_change,
// an optional list of columns for export, or an empty object for
// delete.
type bulkRecordRequest struct {
	IDs     []string        `json:"ids"`
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// bulk dispatches the requested action over the selected records.
// Each action runs inside WithTenantTx so the batch commits as a unit
// — a partial failure rolls back the whole selection rather than
// leaving the list in a mixed state the user cannot easily undo.
func (h *recordHandlers) bulk(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	ktypeName := chi.URLParam(r, "ktype")
	var req bulkRecordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		http.Error(w, "ids required", http.StatusBadRequest)
		return
	}
	ids := make([]uuid.UUID, 0, len(req.IDs))
	for _, raw := range req.IDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid record id %q", raw), http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	actor := actorOrDefault(r.Context())
	switch req.Action {
	case "status_change":
		var payload struct {
			Status string          `json:"status"`
			Data   json.RawMessage `json:"data"`
		}
		if len(req.Payload) > 0 {
			if err := json.Unmarshal(req.Payload, &payload); err != nil {
				http.Error(w, "invalid payload", http.StatusBadRequest)
				return
			}
		}
		// Either {"status":"foo"} or {"data":{"status":"foo", ...}}.
		// We normalise to a JSONB patch the store shallow-merges onto
		// each record's data.
		patch := payload.Data
		if len(patch) == 0 {
			if payload.Status == "" {
				http.Error(w, "status required", http.StatusBadRequest)
				return
			}
			p, err := json.Marshal(map[string]string{"status": payload.Status})
			if err != nil {
				http.Error(w, "marshal patch: "+err.Error(), http.StatusInternalServerError)
				return
			}
			patch = p
		}
		res, err := h.store.BulkPatch(r.Context(), t.ID, ktypeName, ids, patch, actor)
		if err != nil {
			writeRecordError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "delete":
		res, err := h.store.BulkDelete(r.Context(), t.ID, ktypeName, ids, actor)
		if err != nil {
			writeRecordError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	case "export":
		rows, err := h.store.BulkFetch(r.Context(), t.ID, ktypeName, ids)
		if err != nil {
			writeRecordError(w, err)
			return
		}
		writeRecordCSV(w, ktypeName, rows)
	default:
		http.Error(w, fmt.Sprintf("unsupported action %q", req.Action), http.StatusBadRequest)
	}
}

// writeRecordCSV streams the selected records as a CSV file. The
// first row is the union of top-level keys across all records' data
// payloads so the export always surfaces every field the tenant has
// in play without requiring the caller to pre-declare columns.
func writeRecordCSV(w http.ResponseWriter, ktypeName string, rows []record.KRecord) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, ktypeName))
	cw := csv.NewWriter(w)
	defer cw.Flush()

	columnSet := map[string]struct{}{}
	parsed := make([]map[string]any, len(rows))
	for i, r := range rows {
		m := map[string]any{}
		if len(r.Data) > 0 {
			_ = json.Unmarshal(r.Data, &m)
		}
		parsed[i] = m
		for k := range m {
			columnSet[k] = struct{}{}
		}
	}
	columns := make([]string, 0, len(columnSet))
	for k := range columnSet {
		columns = append(columns, k)
	}
	sort.Strings(columns)
	header := append([]string{"id", "status", "version", "created_at", "updated_at"}, columns...)
	_ = cw.Write(header)
	for i, r := range rows {
		row := []string{
			r.ID.String(),
			r.Status,
			strconv.Itoa(r.Version),
			r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		}
		for _, col := range columns {
			v, ok := parsed[i][col]
			if !ok || v == nil {
				row = append(row, "")
				continue
			}
			switch val := v.(type) {
			case string:
				row = append(row, val)
			default:
				b, err := json.Marshal(val)
				if err != nil {
					row = append(row, "")
				} else {
					row = append(row, string(b))
				}
			}
		}
		_ = cw.Write(row)
	}
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

// phaseASystemActor is a deterministic non-nil UUID attributed to requests
// that reach the handler without an authenticated user. The record store
// rejects `uuid.Nil` as a created_by / updated_by value, so the handlers
// need a stable sentinel until a real auth middleware lands.
var phaseASystemActor = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// actorOrDefault returns the user id from context, or phaseASystemActor when
// the request did not carry a user identity.
func actorOrDefault(ctx context.Context) uuid.UUID {
	if id := platform.UserIDFromContext(ctx); id != uuid.Nil {
		return id
	}
	return phaseASystemActor
}
