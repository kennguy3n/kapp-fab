package main

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/print"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

type printHandlers struct {
	records  *record.PGStore
	renderer *print.Renderer
}

// pdf renders the record as a printable PDF. The endpoint is a GET
// so it can be linked directly from the UI ("Download PDF").
func (h *printHandlers) pdf(w http.ResponseWriter, r *http.Request) {
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
	rec, err := h.records.Get(r.Context(), t.ID, id)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	doc, err := h.renderer.RenderPDF(r.Context(), *rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", doc.ContentType)
	w.Header().Set(
		"Content-Disposition",
		fmt.Sprintf(`inline; filename="%s-%s.pdf"`, chi.URLParam(r, "ktype"), id.String()),
	)
	_, _ = w.Write(doc.Bytes)
}

// html renders the record as a printable HTML page. Useful for the
// UI's in-app "Print preview" so the operator can see the layout
// before committing a PDF fetch.
func (h *printHandlers) html(w http.ResponseWriter, r *http.Request) {
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
	rec, err := h.records.Get(r.Context(), t.ID, id)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	body, err := h.renderer.RenderHTML(r.Context(), *rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}
