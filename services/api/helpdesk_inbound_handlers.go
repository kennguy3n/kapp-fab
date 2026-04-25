package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
)

// helpdeskInboundHandlers backs POST /api/v1/helpdesk/inbound-email.
//
// The endpoint accepts a structured InboundEmail payload (an upstream
// SMTP relay or webhook is responsible for MIME parsing). The handler
// resolves the tenant from the recipient host, then opens a ticket
// under the resolved tenant's RLS context. The route lives outside
// the JWT-tenant middleware because the inbound mail relay is
// unauthenticated by design — instead we authenticate by the static
// shared secret in `X-Helpdesk-Inbound-Token` and rate-limit per
// recipient host.
type helpdeskInboundHandlers struct {
	handler *helpdesk.InboundEmailHandler
	// secret is the shared secret the relay attaches as
	// X-Helpdesk-Inbound-Token. Empty disables auth so local dev
	// can post curl requests without setup; production deployments
	// should always set it.
	secret string
}

func (h *helpdeskInboundHandlers) post(w http.ResponseWriter, r *http.Request) {
	if h.handler == nil {
		http.Error(w, "inbound email handler not wired", http.StatusServiceUnavailable)
		return
	}
	if h.secret != "" && r.Header.Get("X-Helpdesk-Inbound-Token") != h.secret {
		http.Error(w, "invalid inbound token", http.StatusUnauthorized)
		return
	}
	var email helpdesk.InboundEmail
	if err := json.NewDecoder(r.Body).Decode(&email); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	rec, err := h.handler.Process(r.Context(), email)
	if err != nil {
		switch {
		case errors.Is(err, helpdesk.ErrUnknownRecipient):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, helpdesk.ErrInvalidEmail):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}
