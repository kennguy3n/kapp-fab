package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// breakGlassHandlers exposes the runtime break-glass flow on top of the
// admin_audit_log table (migrations/000103_admin_roles_split.sql) and
// the kapp_breakglass role. The endpoints are admin-only (gated by
// adminChain → auth.AdminMiddleware → IsPlatformAdmin) and provide:
//
//   - POST /api/v1/admin/break-glass/sessions
//     Open a time-boxed break-glass session with a mandatory reason
//     code and optional approver. The session is recorded in
//     admin_audit_log immediately.
//   - GET  /api/v1/admin/break-glass/sessions
//     List recent break-glass sessions (optionally filtered by target
//     tenant via ?tenant=<uuid>).
//   - GET  /api/v1/admin/break-glass/sessions/active
//     List only sessions that have not yet expired. This is the query
//     a runtime BYPASSRLS gateway calls before opening a
//     kapp_breakglass connection.
//
// The handlers themselves never connect as kapp_breakglass and never
// bypass RLS. They only record and surface who did, why, and when.
type breakGlassHandlers struct {
	store *tenant.BreakGlassStore
}

type openBreakGlassRequest struct {
	ReasonCode   string     `json:"reason_code"`
	TargetTenant *uuid.UUID `json:"target_tenant,omitempty"`
	TargetTable  string     `json:"target_table,omitempty"`
	ExpiresIn    string     `json:"expires_in"` // duration string e.g. "30m", "2h"
	ApprovedBy   *uuid.UUID `json:"approved_by,omitempty"`
	Metadata     any        `json:"metadata,omitempty"`
}

func (h *breakGlassHandlers) openSession(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "break-glass store not configured (ADMIN_DB_URL required)", http.StatusServiceUnavailable)
		return
	}
	var req openBreakGlassRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.ReasonCode == "" {
		http.Error(w, "reason_code is required", http.StatusBadRequest)
		return
	}
	// Validate target_table if provided — it must be a simple
	// identifier (letters, digits, underscores) to prevent SQL
	// injection via the audit log's target_table field.
	if req.TargetTable != "" && !isValidTableName(req.TargetTable) {
		http.Error(w, "target_table must be a simple identifier (letters, digits, underscores only)", http.StatusBadRequest)
		return
	}
	if req.ExpiresIn == "" {
		http.Error(w, "expires_in is required (e.g. \"30m\", \"2h\")", http.StatusBadRequest)
		return
	}
	dur, err := time.ParseDuration(req.ExpiresIn)
	if err != nil {
		http.Error(w, "invalid expires_in duration", http.StatusBadRequest)
		return
	}
	expiresAt := time.Now().UTC().Add(dur)

	operatorID := platform.UserIDFromContext(r.Context())
	var operatorIDPtr *uuid.UUID
	if operatorID != uuid.Nil {
		operatorIDPtr = &operatorID
	}

	var meta json.RawMessage
	if req.Metadata != nil {
		meta, _ = json.Marshal(req.Metadata)
	}

	session, err := h.store.OpenSession(r.Context(), tenant.BreakGlassEntry{
		OperatorID:   operatorIDPtr,
		OperatorKind: "user",
		Role:         "kapp_breakglass",
		ReasonCode:   req.ReasonCode,
		TargetTenant: req.TargetTenant,
		TargetTable:  req.TargetTable,
		ExpiresAt:    &expiresAt,
		ApprovedBy:   req.ApprovedBy,
		Metadata:     meta,
	})
	if err != nil {
		if errors.Is(err, tenant.ErrReasonRequired) || errors.Is(err, tenant.ErrExpiryRequired) || errors.Is(err, tenant.ErrExpiryTooFar) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "failed to open break-glass session", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (h *breakGlassHandlers) listSessions(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "break-glass store not configured", http.StatusServiceUnavailable)
		return
	}
	var targetTenant *uuid.UUID
	if tID := r.URL.Query().Get("tenant"); tID != "" {
		id, err := uuid.Parse(tID)
		if err != nil {
			http.Error(w, "invalid tenant uuid", http.StatusBadRequest)
			return
		}
		targetTenant = &id
	}
	sessions, err := h.store.ListSessions(r.Context(), targetTenant, 100)
	if err != nil {
		http.Error(w, "failed to list break-glass sessions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (h *breakGlassHandlers) listActive(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "break-glass store not configured", http.StatusServiceUnavailable)
		return
	}
	var targetTenant *uuid.UUID
	if tID := r.URL.Query().Get("tenant"); tID != "" {
		id, err := uuid.Parse(tID)
		if err != nil {
			http.Error(w, "invalid tenant uuid", http.StatusBadRequest)
			return
		}
		targetTenant = &id
	}
	sessions, err := h.store.ListActive(r.Context(), targetTenant)
	if err != nil {
		http.Error(w, "failed to list active break-glass sessions", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// isValidTableName checks that a table name is a simple SQL
// identifier (letters, digits, underscores, dots for schema-qualified
// names). This prevents SQL injection via the audit log's
// target_table field, which is stored as a free-text string.
func isValidTableName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '_':
		case c == '.' && i > 0:
		default:
			return false
		}
	}
	return true
}
