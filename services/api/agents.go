package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// agentHandlers wires the Phase B agent tools HTTP surface
// (ARCHITECTURE.md §10-§11). Tool invocations derive the tenant from
// the authenticated context and the actor from the request's user
// identity so the audit trail is attributable even when the caller
// does not explicitly populate the envelope.
type agentHandlers struct {
	executor *agents.Executor
}

// list returns the set of registered tool names. Useful for clients
// that need to render a tool palette or validate configuration.
func (h *agentHandlers) list(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"tools": h.executor.Tools(),
	})
}

// invoke decodes an agents.Invocation envelope and dispatches to the
// executor. Tenant and actor are stamped from the request context so
// external clients cannot forge identities; the tool name in the URL
// path is authoritative over the body so misrouted payloads fail fast.
func (h *agentHandlers) invoke(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var inv agents.Invocation
	if err := json.NewDecoder(r.Body).Decode(&inv); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	inv.TenantID = t.ID
	inv.ActorID = actorOrDefault(r.Context())
	inv.ToolName = chi.URLParam(r, "name")

	res, err := h.executor.Invoke(r.Context(), inv)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func writeAgentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agents.ErrMissingContext):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, agents.ErrUnknownTool):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, agents.ErrConfirmationRequired):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, agents.ErrInvalidMode):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
