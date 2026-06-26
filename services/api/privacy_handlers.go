package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// privacyHandlers exposes the tenant privacy dashboard API under
// /api/v1/privacy. The dashboard surfaces:
//   - Encryption summary: per-KType field classification and
//     encryption status, so tenants can audit which fields are
//     protected.
//   - Break-glass sessions: active and recent time-boxed access
//     sessions, so tenants can monitor emergency access.
//   - Key versions: per-tenant DEK versions (when envelope encryption
//     is enabled), so tenants can verify rotation history.
//   - Audit summary: recent sensitive operations (exports, agent-tool
//     invocations, break-glass actions).
//
// All endpoints are tenant-scoped (the tenant is derived from the
// request context). Admin operators access cross-tenant views via
// the /api/v1/admin surface.
type privacyHandlers struct {
	ktypeRegistry *ktype.PGRegistry
	breakGlass    *tenant.BreakGlassStore
	envelopeKM    *tenant.EnvelopeKeyManager // nil when envelope encryption is disabled
	auditPool     *pgxpool.Pool              // nil when audit summary is disabled
}

type encryptionFieldSummary struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Encrypted      bool   `json:"encrypted"`
	Indexed        bool   `json:"indexed,omitempty"`
	Classification string `json:"classification,omitempty"`
	Path           string `json:"path,omitempty"`
}

type encryptionKTypeSummary struct {
	Name      string                   `json:"name"`
	Version   int                      `json:"version"`
	Fields    []encryptionFieldSummary `json:"fields"`
	Encrypted int                      `json:"encrypted_field_count"`
	Indexed   int                      `json:"indexed_field_count"`
}

type encryptionSummaryResponse struct {
	KTypes             []encryptionKTypeSummary `json:"ktypes"`
	TotalKTypes        int                      `json:"total_ktypes"`
	TotalEncrypted     int                      `json:"total_encrypted_fields"`
	TotalIndexed       int                      `json:"total_indexed_fields"`
	EnvelopeEncryption bool                     `json:"envelope_encryption_enabled"`
}

// encryptionSummary returns the per-KType encryption classification.
// GET /api/v1/privacy/encryption
func (h *privacyHandlers) encryptionSummary(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	kts, err := h.ktypeRegistry.List(r.Context())
	if err != nil {
		http.Error(w, "failed to list ktypes", http.StatusInternalServerError)
		return
	}
	resp := encryptionSummaryResponse{
		EnvelopeEncryption: h.envelopeKM != nil,
	}
	for _, kt := range kts {
		summary := encryptionKTypeSummary{
			Name:    kt.Name,
			Version: kt.Version,
		}
		var schema ktype.Schema
		if err := json.Unmarshal(kt.Schema, &schema); err != nil {
			// Skip unparseable schemas rather than failing the whole
			// dashboard — a corrupt schema shouldn't block visibility
			// into the rest.
			continue
		}
		for _, f := range schema.Fields {
			fs := encryptionFieldSummary{
				Name:           f.Name,
				Type:           f.Type,
				Encrypted:      f.Encrypted,
				Indexed:        f.Indexed,
				Classification: f.Classification,
				Path:           f.Path,
			}
			summary.Fields = append(summary.Fields, fs)
			if f.Encrypted {
				summary.Encrypted++
				resp.TotalEncrypted++
			}
			if f.Indexed {
				summary.Indexed++
				resp.TotalIndexed++
			}
		}
		resp.KTypes = append(resp.KTypes, summary)
	}
	resp.TotalKTypes = len(resp.KTypes)
	writeJSON(w, http.StatusOK, resp)
}

type breakGlassSummary struct {
	ActiveSessions []tenant.BreakGlassSession `json:"active_sessions"`
	RecentSessions []tenant.BreakGlassSession `json:"recent_sessions"`
	ActiveCount    int                        `json:"active_count"`
	RecentCount    int                        `json:"recent_count"`
}

// breakGlassSummary returns active and recent break-glass sessions.
// GET /api/v1/privacy/break-glass
func (h *privacyHandlers) breakGlassSummary(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if h.breakGlass == nil {
		writeJSON(w, http.StatusOK, breakGlassSummary{})
		return
	}
	// Tenant-scoped: filter to the requesting tenant. Admin operators
	// access cross-tenant views via the /api/v1/admin surface.
	tenantID := &t.ID
	active, err := h.breakGlass.ListActive(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "failed to list active sessions", http.StatusInternalServerError)
		return
	}
	recent, err := h.breakGlass.ListSessions(r.Context(), tenantID, 20)
	if err != nil {
		http.Error(w, "failed to list recent sessions", http.StatusInternalServerError)
		return
	}
	resp := breakGlassSummary{
		ActiveSessions: active,
		RecentSessions: recent,
		ActiveCount:    len(active),
		RecentCount:    len(recent),
	}
	writeJSON(w, http.StatusOK, resp)
}

type keyVersionSummary struct {
	Enabled   bool             `json:"envelope_encryption_enabled"`
	Versions  []tenant.KeyInfo `json:"key_versions,omitempty"`
	ActiveVer int              `json:"active_version,omitempty"`
}

// keyVersions returns the tenant's DEK version history (envelope
// encryption only). When envelope encryption is disabled, returns
// a stub indicating the feature is off.
// GET /api/v1/privacy/keys
func (h *privacyHandlers) keyVersions(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if h.envelopeKM == nil {
		writeJSON(w, http.StatusOK, keyVersionSummary{Enabled: false})
		return
	}
	versions, err := h.envelopeKM.ListKeyVersions(r.Context(), t.ID)
	if err != nil {
		http.Error(w, "failed to list key versions", http.StatusInternalServerError)
		return
	}
	active, _ := h.envelopeKM.KeyVersion(r.Context(), t.ID)
	writeJSON(w, http.StatusOK, keyVersionSummary{
		Enabled:   true,
		Versions:  versions,
		ActiveVer: active,
	})
}

type auditSummaryEntry struct {
	Action    string    `json:"action"`
	ActorID   string    `json:"actor_id"`
	ActorKind string    `json:"actor_kind"`
	Count     int       `json:"count"`
	LastAt    time.Time `json:"last_at"`
}

type auditSummaryResponse struct {
	Entries     []auditSummaryEntry `json:"entries"`
	GeneratedAt time.Time           `json:"generated_at"`
}

// auditSummary returns a summary of recent sensitive audit events
// (exports, agent-tool invocations, break-glass actions) grouped by
// action + actor. This gives the privacy dashboard a quick view of
// who is accessing sensitive data and how often.
// GET /api/v1/privacy/audit-summary
func (h *privacyHandlers) auditSummary(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if h.auditPool == nil {
		writeJSON(w, http.StatusOK, auditSummaryResponse{
			GeneratedAt: time.Now().UTC(),
		})
		return
	}
	// Query the audit_log for the last 7 days, grouped by action +
	// actor_kind. This is a read-only summary; full audit records
	// are available via the admin audit surface.
	rows, err := h.auditPool.Query(r.Context(),
		`SELECT action, actor_id::text, actor_kind, COUNT(*) as cnt, MAX(created_at) as last_at
		   FROM audit_log
		  WHERE tenant_id = $1
		    AND created_at > now() - interval '7 days'
		    AND action IN ('export.create', 'export.complete', 'agent.invoke', 'breakglass.open', 'breakglass.extend', 'breakglass.close')
		  GROUP BY action, actor_id, actor_kind
		  ORDER BY cnt DESC
		  LIMIT 50`,
		t.ID,
	)
	if err != nil {
		http.Error(w, "failed to query audit log", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var entries []auditSummaryEntry
	for rows.Next() {
		var e auditSummaryEntry
		if err := rows.Scan(&e.Action, &e.ActorID, &e.ActorKind, &e.Count, &e.LastAt); err != nil {
			http.Error(w, "failed to scan audit entries", http.StatusInternalServerError)
			return
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "failed to read audit rows", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, auditSummaryResponse{
		Entries:     entries,
		GeneratedAt: time.Now().UTC(),
	})
}

// dashboard is the aggregate endpoint that returns all privacy
// dashboard sections in a single response. This is the main entry
// point for the UI.
// GET /api/v1/privacy/dashboard
func (h *privacyHandlers) dashboard(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	type dashboardResponse struct {
		TenantID    uuid.UUID                 `json:"tenant_id"`
		GeneratedAt time.Time                 `json:"generated_at"`
		Encryption  encryptionSummaryResponse `json:"encryption"`
		BreakGlass  breakGlassSummary         `json:"break_glass"`
		Keys        keyVersionSummary         `json:"keys"`
		Audit       auditSummaryResponse      `json:"audit_summary"`
	}

	// Build each section. Errors in one section don't fail the whole
	// dashboard — the UI renders what it can and shows error states
	// for sections that failed.
	resp := dashboardResponse{
		TenantID:    t.ID,
		GeneratedAt: time.Now().UTC(),
	}

	// Encryption summary
	kts, err := h.ktypeRegistry.List(r.Context())
	if err == nil {
		for _, kt := range kts {
			summary := encryptionKTypeSummary{Name: kt.Name, Version: kt.Version}
			var schema ktype.Schema
			if err := json.Unmarshal(kt.Schema, &schema); err != nil {
				continue
			}
			for _, f := range schema.Fields {
				fs := encryptionFieldSummary{
					Name:           f.Name,
					Type:           f.Type,
					Encrypted:      f.Encrypted,
					Indexed:        f.Indexed,
					Classification: f.Classification,
					Path:           f.Path,
				}
				summary.Fields = append(summary.Fields, fs)
				if f.Encrypted {
					summary.Encrypted++
					resp.Encryption.TotalEncrypted++
				}
				if f.Indexed {
					summary.Indexed++
					resp.Encryption.TotalIndexed++
				}
			}
			resp.Encryption.KTypes = append(resp.Encryption.KTypes, summary)
		}
		resp.Encryption.TotalKTypes = len(resp.Encryption.KTypes)
	}
	resp.Encryption.EnvelopeEncryption = h.envelopeKM != nil

	// Break-glass summary
	if h.breakGlass != nil {
		tenantID := &t.ID
		if active, err := h.breakGlass.ListActive(r.Context(), tenantID); err == nil {
			resp.BreakGlass.ActiveSessions = active
			resp.BreakGlass.ActiveCount = len(active)
		}
		if recent, err := h.breakGlass.ListSessions(r.Context(), tenantID, 20); err == nil {
			resp.BreakGlass.RecentSessions = recent
			resp.BreakGlass.RecentCount = len(recent)
		}
	}

	// Key versions
	if h.envelopeKM != nil {
		resp.Keys.Enabled = true
		if versions, err := h.envelopeKM.ListKeyVersions(r.Context(), t.ID); err == nil {
			resp.Keys.Versions = versions
		}
		if v, err := h.envelopeKM.KeyVersion(r.Context(), t.ID); err == nil {
			resp.Keys.ActiveVer = v
		}
	}

	// Audit summary
	if h.auditPool != nil {
		rows, err := h.auditPool.Query(r.Context(),
			`SELECT action, actor_id::text, actor_kind, COUNT(*) as cnt, MAX(created_at) as last_at
			   FROM audit_log
			  WHERE tenant_id = $1
			    AND created_at > now() - interval '7 days'
			    AND action IN ('export.create', 'export.complete', 'agent.invoke', 'breakglass.open', 'breakglass.extend', 'breakglass.close')
			  GROUP BY action, actor_id, actor_kind
			  ORDER BY cnt DESC
			  LIMIT 50`,
			t.ID,
		)
		if err == nil {
			for rows.Next() {
				var e auditSummaryEntry
				if err := rows.Scan(&e.Action, &e.ActorID, &e.ActorKind, &e.Count, &e.LastAt); err != nil {
					break
				}
				resp.Audit.Entries = append(resp.Audit.Entries, e)
			}
			rows.Close()
		}
	}
	resp.Audit.GeneratedAt = time.Now().UTC()

	writeJSON(w, http.StatusOK, resp)
}
