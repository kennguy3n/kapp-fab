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
	// HoneypotURL / HoneypotWebsite / HoneypotHomepage are three
	// invisible decoy fields the public form template renders as
	// CSS-hidden inputs alongside the real data payload. They sit
	// at the JSON envelope's top level — NOT inside the Data map —
	// so they cannot collide with KType schema fields that happen
	// to be named url/website/homepage (extremely common in CRM
	// and contact-form schemas). Real users never see the decoys,
	// real templates never write them, so any non-empty value here
	// indicates a scraper-style bot that submitted every visible
	// input it could find.
	//
	// CONSTRAINT: do NOT add new top-level fields named url, website,
	// or homepage to this envelope. Those three names are reserved
	// for the honeypot triplet. If a future surface needs a
	// top-level redirect URL or any other generic-named value, pick
	// a non-conflicting name (e.g. redirect_url, callback) or
	// rename the honeypot fields under a namespaced prefix (e.g.
	// _h_*) — and update the public form template at the same time
	// so real submissions don't suddenly look like bots.
	HoneypotURL      string `json:"url,omitempty"`
	HoneypotWebsite  string `json:"website,omitempty"`
	HoneypotHomepage string `json:"homepage,omitempty"`
}

// isHoneypotTripped reports whether any of the decoy fields carry a
// value. Any non-empty value — including whitespace — counts as a
// trip: real templates never emit these fields, so a bot that wrote
// " " into one of them is still a bot.
func (r submitFormRequest) isHoneypotTripped() bool {
	return r.HoneypotURL != "" || r.HoneypotWebsite != "" || r.HoneypotHomepage != ""
}

// submit accepts the public form payload and creates a KRecord under
// the form's tenant. IP-based rate limiting is enforced by the
// IPRateLimitMiddleware mounted on the route; the honeypot check
// below catches drive-by bots that pass the rate limit but fill in
// any of the invisible decoy fields the template emits as CSS-hidden.
//
// Successful spam is silently absorbed (202 Accepted with no body)
// so the bot cannot tell its submission was rejected and adapt.
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
	// Honeypot trips when any of the three top-level decoy fields
	// carries a non-empty value. We return 202 Accepted with no
	// body to look indistinguishable from a successful drop —
	// bots that see 4xx adapt; bots that see 2xx without verifying
	// do not.
	//
	// NOTE: we intentionally do NOT inspect req.Data for honeypot
	// keys. req.Data carries the real KType schema payload, and a
	// schema field named url/website/homepage (common in CRM
	// contact forms) would otherwise be silently dropped. The
	// top-level decoys are sufficient because the template renders
	// them as siblings of the data envelope, so a legitimate
	// submission never populates them.
	if req.isHoneypotTripped() {
		w.WriteHeader(http.StatusAccepted)
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
