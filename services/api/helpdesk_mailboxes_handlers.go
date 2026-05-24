// Surface G (PR-7) — helpdesk mailbox CRUD HTTP handlers. Surfaces
// the mailboxes.Store interface as REST endpoints under
// /api/v1/helpdesk/mailboxes so tenant admins can attach + tune
// inbound IMAP mailboxes from the UI. The worker's IMAP supervisor
// picks up changes within one converge tick (60s default).
//
// Routing layout (mounted in routes.go):
//
//	GET    /api/v1/helpdesk/mailboxes              → list
//	POST   /api/v1/helpdesk/mailboxes              → create
//	GET    /api/v1/helpdesk/mailboxes/{id}         → get one
//	PUT    /api/v1/helpdesk/mailboxes/{id}         → replace
//	DELETE /api/v1/helpdesk/mailboxes/{id}         → delete
//
// Tenant scoping: every handler reads platform.TenantFromContext(r)
// (set by the tenant-resolution middleware) and forwards that to the
// store. The store's CRUD methods run under WithTenantTx so RLS is
// the final guarantor of cross-tenant isolation.
//
// Credentials: the imap_password_ref field is the SecretProvider
// key for the mailbox password, NEVER a plaintext value. The store
// rejects values that look like plaintext (see
// mailboxes.ErrPasswordRefLooksPlain) so a UI bug or curl typo
// surfaces a 400 instead of writing a secret to a logged column.
package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk/mailboxes"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// helpdeskMailboxHandlers is the chi-router-aware adapter on top of
// mailboxes.Store. One instance per process; goroutine-safe because
// the underlying PGStore is.
type helpdeskMailboxHandlers struct {
	store mailboxes.Store
}

// mailboxRequest is the wire shape for create/update requests. We do
// not accept TenantID / MailboxID / CreatedAt / UpdatedAt in the body
// — TenantID comes from the request context, MailboxID is server-
// assigned on create + URL-path-derived on update, and the timestamps
// are server-set on every Create + Update. Keeping the wire shape
// minimal also prevents tenant-spoofing via a body field.
type mailboxRequest struct {
	Name                string `json:"name"`
	IMAPHost            string `json:"imap_host"`
	IMAPPort            int    `json:"imap_port"`
	IMAPUsername        string `json:"imap_username"`
	IMAPPasswordRef     string `json:"imap_password_ref"`
	IMAPUseTLS          *bool  `json:"imap_use_tls,omitempty"`
	Folder              string `json:"folder,omitempty"`
	PollIntervalSeconds *int   `json:"poll_interval_seconds,omitempty"`
	MaxBackoffSeconds   *int   `json:"max_backoff_seconds,omitempty"`
	FetchBatchSize      *int   `json:"fetch_batch_size,omitempty"`
	Enabled             *bool  `json:"enabled,omitempty"`
}

// applyDefaults copies the request into a mailboxes.Mailbox with the
// scalar-default fields filled in. We default IMAPUseTLS to true,
// Folder to "INBOX", Enabled to true so the common case (a tenant
// admin pasting a new support-mailbox config into the form) is a
// minimal-body request.
func (req mailboxRequest) toMailbox(tenantID, mailboxID uuid.UUID) mailboxes.Mailbox {
	useTLS := true
	if req.IMAPUseTLS != nil {
		useTLS = *req.IMAPUseTLS
	}
	folder := req.Folder
	if folder == "" {
		folder = mailboxes.DefaultFolder
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return mailboxes.Mailbox{
		TenantID:            tenantID,
		MailboxID:           mailboxID,
		Name:                req.Name,
		IMAPHost:            req.IMAPHost,
		IMAPPort:            req.IMAPPort,
		IMAPUsername:        req.IMAPUsername,
		IMAPPasswordRef:     req.IMAPPasswordRef,
		IMAPUseTLS:          useTLS,
		Folder:              folder,
		PollIntervalSeconds: req.PollIntervalSeconds,
		MaxBackoffSeconds:   req.MaxBackoffSeconds,
		FetchBatchSize:      req.FetchBatchSize,
		Enabled:             enabled,
	}
}

func (h *helpdeskMailboxHandlers) list(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rows, err := h.store.List(r.Context(), t.ID)
	if err != nil {
		writeMailboxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": rows})
}

func (h *helpdeskMailboxHandlers) create(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req mailboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	mb := req.toMailbox(t.ID, uuid.New())
	saved, err := h.store.Create(r.Context(), mb)
	if err != nil {
		writeMailboxError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

func (h *helpdeskMailboxHandlers) get(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid mailbox id", http.StatusBadRequest)
		return
	}
	saved, err := h.store.Get(r.Context(), t.ID, id)
	if err != nil {
		writeMailboxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *helpdeskMailboxHandlers) update(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid mailbox id", http.StatusBadRequest)
		return
	}
	var req mailboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	mb := req.toMailbox(t.ID, id)
	saved, err := h.store.Update(r.Context(), mb)
	if err != nil {
		writeMailboxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (h *helpdeskMailboxHandlers) delete(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid mailbox id", http.StatusBadRequest)
		return
	}
	if err := h.store.Delete(r.Context(), t.ID, id); err != nil {
		writeMailboxError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeMailboxError translates mailbox sentinel errors into HTTP
// status codes. The sentinel layer is the source of truth — the
// admin UI parses the error message to highlight the offending
// form field, so the on-the-wire string must match the sentinel
// verbatim.
func writeMailboxError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mailboxes.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, mailboxes.ErrDuplicateNameForTenant):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, mailboxes.ErrInvalidTenantID),
		errors.Is(err, mailboxes.ErrInvalidMailboxID),
		errors.Is(err, mailboxes.ErrInvalidName),
		errors.Is(err, mailboxes.ErrInvalidHost),
		errors.Is(err, mailboxes.ErrInvalidPort),
		errors.Is(err, mailboxes.ErrInvalidUsername),
		errors.Is(err, mailboxes.ErrInvalidPasswordRef),
		errors.Is(err, mailboxes.ErrInvalidFolder),
		errors.Is(err, mailboxes.ErrInvalidPollInterval),
		errors.Is(err, mailboxes.ErrInvalidMaxBackoff),
		errors.Is(err, mailboxes.ErrInvalidFetchBatchSize),
		errors.Is(err, mailboxes.ErrPasswordRefLooksPlain):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
