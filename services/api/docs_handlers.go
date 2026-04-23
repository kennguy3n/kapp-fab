package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/docs"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// docsHandlers exposes the Phase F Docs KApp — artifact documents with
// append-only version history. Create + SaveVersion + Restore all
// write a new history row under tenant context; nothing is ever
// deleted so the audit trail stays intact across revisions and
// restore-from-history operations.
type docsHandlers struct {
	store *docs.Store
}

type createDocRequest struct {
	Title   string          `json:"title"`
	DocType string          `json:"doc_type,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

func (h *docsHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createDocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	d, err := h.store.Create(r.Context(), docs.Document{
		TenantID:  t.ID,
		Title:     req.Title,
		DocType:   req.DocType,
		Content:   req.Content,
		CreatedBy: actorOrDefault(r.Context()),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *docsHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	ds, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ds == nil {
		ds = []docs.Document{}
	}
	writeJSON(w, http.StatusOK, ds)
}

func (h *docsHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid document id", http.StatusBadRequest)
		return
	}
	d, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeDocsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type saveVersionRequest struct {
	Title         string          `json:"title,omitempty"`
	Content       json.RawMessage `json:"content"`
	Diff          json.RawMessage `json:"diff,omitempty"`
	ChangeSummary string          `json:"change_summary,omitempty"`
}

func (h *docsHandlers) saveVersion(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid document id", http.StatusBadRequest)
		return
	}
	var req saveVersionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	d, err := h.store.SaveVersion(
		r.Context(), t.ID, id, actorOrDefault(r.Context()),
		req.Title, req.Content, req.Diff, req.ChangeSummary,
	)
	if err != nil {
		writeDocsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *docsHandlers) versions(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid document id", http.StatusBadRequest)
		return
	}
	vs, err := h.store.Versions(r.Context(), t.ID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if vs == nil {
		vs = []docs.Version{}
	}
	writeJSON(w, http.StatusOK, vs)
}

type restoreRequest struct {
	Version int `json:"version"`
}

func (h *docsHandlers) restore(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid document id", http.StatusBadRequest)
		return
	}
	var req restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	// Allow `?version=` as a fallback for the query-string form the
	// wizard emits when the UI builds a link-style restore URL.
	if req.Version == 0 {
		if q := r.URL.Query().Get("version"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				req.Version = n
			}
		}
	}
	if req.Version <= 0 {
		http.Error(w, "version required", http.StatusBadRequest)
		return
	}
	d, err := h.store.Restore(r.Context(), t.ID, id, actorOrDefault(r.Context()), req.Version)
	if err != nil {
		writeDocsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func writeDocsError(w http.ResponseWriter, err error) {
	if errors.Is(err, docs.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
