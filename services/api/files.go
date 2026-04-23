package main

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// filesHandlers exposes the Phase F attachment surface. Upload stores
// the raw bytes in the object store keyed by SHA-256 so a second upload
// of the same content dedups at the storage layer, then writes a
// per-tenant metadata row under RLS so each tenant owns its own view.
type filesHandlers struct {
	store *files.Store
}

// maxUploadBytes caps a single upload at 32 MiB. Larger artifacts should
// be streamed through a pre-signed URL (future); this keeps the naive
// multipart path predictable for the wizard's attachment-rehosting flow.
const maxUploadBytes = 32 << 20

// upload accepts either multipart/form-data (`file` field) or a raw
// body upload with the Content-Type preserved. The response carries
// the hydrated File metadata so the client can link it from a record.
func (h *filesHandlers) upload(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	actor := actorOrDefault(r.Context())

	var blob files.Blob
	ctype := r.Header.Get("Content-Type")
	if ctype != "" && len(ctype) >= len("multipart/") && ctype[:len("multipart/")] == "multipart/" {
		if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
			http.Error(w, "invalid multipart body", http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing `file` form field", http.StatusBadRequest)
			return
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, maxUploadBytes+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(data)) > maxUploadBytes {
			http.Error(w, "upload exceeds maximum size", http.StatusRequestEntityTooLarge)
			return
		}
		mime := hdr.Header.Get("Content-Type")
		if mime == "" {
			mime = "application/octet-stream"
		}
		blob = files.Blob{ContentType: mime, Data: data}
	} else {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if int64(len(data)) > maxUploadBytes {
			http.Error(w, "upload exceeds maximum size", http.StatusRequestEntityTooLarge)
			return
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		blob = files.Blob{ContentType: ctype, Data: data}
	}

	f, err := h.store.Upload(r.Context(), t.ID, actor, blob)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// get returns the metadata row for a file id.
func (h *filesHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}
	f, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// download streams the stored bytes back to the caller.
func (h *filesHandlers) download(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid file id", http.StatusBadRequest)
		return
	}
	meta, rc, err := h.store.Read(r.Context(), t.ID, id)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", meta.ContentType)
	_, _ = io.Copy(w, rc)
}
