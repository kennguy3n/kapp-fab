package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/forms"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// formsHandlers wires the Forms KApp HTTP surface. The write/read
// handlers require an authenticated tenant (via TenantMiddleware); the
// public GET and public Submit handlers deliberately do NOT — they
// derive the tenant from the form configuration looked up by form id.
type formsHandlers struct {
	store    *forms.Store
	registry *ktype.PGRegistry
}

type createFormRequest struct {
	KType  string       `json:"ktype"`
	Config forms.Config `json:"config"`
}

func (h *formsHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req createFormRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	form, err := h.store.Create(r.Context(), t.ID, req.KType, req.Config, actorOrDefault(r.Context()))
	if err != nil {
		writeFormError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, form)
}

// public returns the form config + KType schema so the public renderer
// can build the form client-side. No tenant context is required. This
// endpoint must only return metadata the submitter already needs to see;
// sensitive form fields live on the KType schema, which is itself public.
func (h *formsHandlers) public(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid form id", http.StatusBadRequest)
		return
	}
	form, err := h.store.GetPublic(r.Context(), id)
	if err != nil {
		writeFormError(w, err)
		return
	}
	kt, err := h.registry.Get(r.Context(), form.KType, 0)
	if err != nil {
		writeFormError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"form":   form,
		"schema": json.RawMessage(kt.Schema),
	})
}

type submitFormRequest struct {
	Data map[string]any `json:"data"`
}

// submit accepts the public form payload and creates a KRecord under
// the form's tenant. Rate limiting by IP should be enforced by the
// reverse proxy / middleware — this handler itself does not re-enter
// the rate limiter because there is no tenant header to key on.
func (h *formsHandlers) submit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid form id", http.StatusBadRequest)
		return
	}
	var req submitFormRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	var submitter *uuid.UUID
	if id := platform.UserIDFromContext(r.Context()); id != uuid.Nil {
		submitter = &id
	}
	rec, err := h.store.Submit(r.Context(), id, req.Data, submitter)
	if err != nil {
		writeFormError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func writeFormError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, forms.ErrFormNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, forms.ErrFormDisabled):
		http.Error(w, err.Error(), http.StatusForbidden)
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
