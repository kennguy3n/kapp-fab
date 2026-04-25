package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/exporter"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// exportHandlers exposes the data-export queue under /api/v1/exports.
// Submission writes a `pending` row; the worker picks it up, runs
// the export, and stores the result back on the same row. Download
// streams the payload column.
type exportHandlers struct {
	store *exporter.Store
}

type createExportRequest struct {
	KType  string `json:"ktype"`
	Format string `json:"format"`
}

func (h *exportHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	actor := actorOrDefault(r.Context())
	job, err := h.store.Enqueue(r.Context(), exporter.ExportJob{
		TenantID:  t.ID,
		KType:     req.KType,
		Format:    req.Format,
		CreatedBy: &actor,
	})
	if err != nil {
		writeExportError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *exportHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	out, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		writeExportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (h *exportHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeExportError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// download streams the export payload back to the user. Status must
// be `completed`; pending / running / failed jobs return 409 so the
// client polls instead of treating an empty body as success.
func (h *exportHandlers) download(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}
	job, err := h.store.GetWithPayload(r.Context(), t.ID, id)
	if err != nil {
		writeExportError(w, err)
		return
	}
	if job.Status != exporter.StatusCompleted || len(job.Payload) == 0 {
		http.Error(w, exporter.ErrJobNotReady.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", job.ContentType)
	// RFC 6266 quoted-string: escape backslash first, then double
	// quotes, so a hostile filename can't inject CRLFs / break out
	// of the header value. sanitizeKType currently produces only
	// [A-Za-z0-9._-], but defence-in-depth at the point of use is
	// cheap and keeps the guarantee if a future code path writes
	// directly to export_jobs.file_name.
	safeName := strings.ReplaceAll(strings.ReplaceAll(job.FileName, `\`, `\\`), `"`, `\"`)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeName))
	_, _ = w.Write(job.Payload)
}

func writeExportError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, exporter.ErrJobNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, exporter.ErrJobNotReady):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, exporter.ErrInvalidInput):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
