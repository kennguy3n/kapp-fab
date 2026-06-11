package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/ledger/bankfeed"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// bankfeedHandlers exposes the Session 15 bank-feed + smart-
// reconciliation HTTP surface (/api/v1/finance/bank-feeds/*). Tenant
// scope, feature gating (FeatureFinance + FeatureBankFeed), authz,
// rate-limit/quota and idempotency are all enforced by the middleware
// stack in routes.go; these handlers translate HTTP into the bankfeed
// stores / SyncHandler / SmartMatcher and never surface provider
// credentials (the response DTOs deliberately omit the token fields).
type bankfeedHandlers struct {
	conns    *bankfeed.ConnectionStore
	rules    *bankfeed.RuleStore
	registry *bankfeed.Registry
	sync     *bankfeed.SyncHandler
	matcher  *ledger.SmartMatcher
	csv      *bankfeed.CSVProvider
}

// maxCSVUploadBytes caps a statement upload so a hostile or accidental
// multi-gigabyte body cannot exhaust memory while parsing. A month of
// dense statement lines is well under a megabyte; 8 MiB leaves generous
// headroom for a full-year export.
const maxCSVUploadBytes = 8 << 20

// ---------------------------------------------------------------------------
// Response DTOs — credential fields are intentionally omitted so an
// AccessToken / RefreshToken can never leak over the wire.
// ---------------------------------------------------------------------------

type connectionResponse struct {
	ID            uuid.UUID `json:"id"`
	BankAccountID uuid.UUID `json:"bank_account_id"`
	Provider      string    `json:"provider"`
	Status        string    `json:"status"`
	Cursor        string    `json:"cursor,omitempty"`
	ExternalID    string    `json:"external_id,omitempty"`
	LastSyncAt    *string   `json:"last_sync_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     string    `json:"created_at"`
	UpdatedAt     string    `json:"updated_at"`
}

func toConnectionResponse(c *bankfeed.Connection) connectionResponse {
	resp := connectionResponse{
		ID:            c.ID,
		BankAccountID: c.BankAccountID,
		Provider:      c.Provider,
		Status:        c.Status,
		Cursor:        c.Cursor,
		ExternalID:    c.ExternalID,
		LastError:     c.LastError,
		CreatedAt:     c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:     c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if c.LastSyncAt != nil {
		s := c.LastSyncAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		resp.LastSyncAt = &s
	}
	return resp
}

func toConnectionResponses(cs []bankfeed.Connection) []connectionResponse {
	out := make([]connectionResponse, 0, len(cs))
	for i := range cs {
		out = append(out, toConnectionResponse(&cs[i]))
	}
	return out
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

// listProviders advertises the provider names the registry was built
// with so the frontend renders only the connect buttons that are
// actually configured (CSV is always present; Plaid/GoCardless appear
// only when their credentials are set). Fail-closed by construction:
// an unconfigured provider is simply absent.
func (h *bankfeedHandlers) listProviders(w http.ResponseWriter, _ *http.Request) {
	names := h.registry.Names()
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": names})
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// listConnections returns the tenant's connections. With a
// ?bank_account_id= filter it returns every connection (any status) for
// that account; without one it returns all active connections.
func (h *bankfeedHandlers) listConnections(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("bank_account_id")); raw != "" {
		acct, err := uuid.Parse(raw)
		if err != nil {
			http.Error(w, "bank_account_id must be a valid UUID", http.StatusBadRequest)
			return
		}
		conns, err := h.conns.ListConnectionsByAccount(r.Context(), t.ID, acct)
		if err != nil {
			writeBankFeedError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toConnectionResponses(conns))
		return
	}
	conns, err := h.conns.ListActiveConnections(r.Context(), t.ID)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toConnectionResponses(conns))
}

type initiateConnectRequest struct {
	Provider      string    `json:"provider"`
	BankAccountID uuid.UUID `json:"bank_account_id"`
	RedirectURI   string    `json:"redirect_uri,omitempty"`
}

// initiateConnect starts a provider's link handshake and returns the URL
// / link token the frontend hands to the provider widget. CSV returns an
// empty link (the UI shows a file picker instead).
func (h *bankfeedHandlers) initiateConnect(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req initiateConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.BankAccountID == uuid.Nil {
		http.Error(w, "bank_account_id required", http.StatusUnprocessableEntity)
		return
	}
	provider, err := h.registry.Get(req.Provider)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	link, err := provider.InitiateConnect(r.Context(), t.ID, req.BankAccountID, req.RedirectURI)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": req.Provider, "link": link})
}

type completeConnectRequest struct {
	Provider      string    `json:"provider"`
	BankAccountID uuid.UUID `json:"bank_account_id"`
	Code          string    `json:"code,omitempty"`
}

// completeConnect exchanges the provider's post-consent code for durable
// credentials and persists the connection (tokens field-encrypted by the
// store). For CSV this just creates the credential-less connection row so
// the account is marked CSV-connected and statement uploads have a row to
// advance. The response omits all token material.
func (h *bankfeedHandlers) completeConnect(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req completeConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.BankAccountID == uuid.Nil {
		http.Error(w, "bank_account_id required", http.StatusUnprocessableEntity)
		return
	}
	provider, err := h.registry.Get(req.Provider)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	conn, err := provider.CompleteConnect(r.Context(), t.ID, req.Code)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	// The provider populates only the credential/cursor fields; the
	// tenant and bank account are authoritative from request context so a
	// caller can never link a feed onto another tenant's account.
	conn.TenantID = t.ID
	conn.BankAccountID = req.BankAccountID
	saved, err := h.conns.UpsertConnection(r.Context(), *conn)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toConnectionResponse(saved))
}

// disconnect revokes the link at the provider (best-effort) and marks the
// local connection revoked so the scheduler stops syncing it. A provider-
// side failure does not block the local revoke — a stale grant is
// harmless once we stop pulling it — but is surfaced in the response.
func (h *bankfeedHandlers) disconnect(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	conn, err := h.conns.GetConnection(r.Context(), t.ID, id)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	var providerErr string
	if provider, perr := h.registry.Get(conn.Provider); perr == nil {
		if derr := provider.Disconnect(r.Context(), conn); derr != nil {
			// Sanitized: provider errors never embed tokens, but they may
			// embed URLs — keep the message generic for the client.
			providerErr = "provider revoke failed; connection marked revoked locally"
		}
	}
	if err := h.conns.SetStatus(r.Context(), t.ID, id, bankfeed.StatusRevoked); err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	resp := map[string]any{"id": id, "status": bankfeed.StatusRevoked}
	if providerErr != "" {
		resp["provider_warning"] = providerErr
	}
	writeJSON(w, http.StatusOK, resp)
}

// syncNow runs an on-demand sync for one connection, reusing the exact
// pipeline the hourly scheduler drives, and returns the per-connection
// counts so the operator sees what landed.
func (h *bankfeedHandlers) syncNow(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	conn, err := h.conns.GetConnection(r.Context(), t.ID, id)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	res, err := h.sync.SyncOne(r.Context(), t.ID, conn)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSyncResultResponse(res))
}

// syncResultResponse is the snake_case projection of a
// bankfeed.SyncResult. The domain struct carries no JSON tags, and its
// Cursor field is a provider-internal pagination token, so the DTO
// exposes only the operator-facing line counts.
type syncResultResponse struct {
	Fetched     int `json:"fetched"`
	Skipped     int `json:"skipped"`
	Inserted    int `json:"inserted"`
	Updated     int `json:"updated"`
	Voided      int `json:"voided"`
	Unwound     int `json:"unwound"`
	Suggested   int `json:"suggested"`
	AutoMatched int `json:"auto_matched"`
}

func toSyncResultResponse(res *bankfeed.SyncResult) syncResultResponse {
	return syncResultResponse{
		Fetched:     res.Fetched,
		Skipped:     res.Skipped,
		Inserted:    res.Inserted,
		Updated:     res.Updated,
		Voided:      res.Voided,
		Unwound:     res.Unwound,
		Suggested:   res.Suggested,
		AutoMatched: res.AutoMatched,
	}
}

// ---------------------------------------------------------------------------
// CSV statement upload
// ---------------------------------------------------------------------------

// uploadCSV ingests a CSV statement for a bank account through the same
// ingest → categorize → match pipeline as a live feed. The CSV body is
// the raw request body (Content-Type text/csv); ?currency= sets the
// default currency for rows that omit one. A CSV connection for the
// account is created on demand so re-uploads dedupe against the same row
// and the upload is fully idempotent. A body larger than
// maxCSVUploadBytes is rejected with 413 rather than ingesting a
// truncated prefix.
func (h *bankfeedHandlers) uploadCSV(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	acct, ok := parseUUIDParam(w, r, "bank_account_id")
	if !ok {
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("currency")))

	// Read at most one byte past the cap so an oversized body is rejected
	// outright (413) instead of being silently truncated at the limit and
	// ingesting only a prefix of the statement. Memory stays bounded to
	// maxCSVUploadBytes+1 — the same ceiling the previous LimitReader
	// enforced — because we stop reading as soon as the overflow byte
	// arrives.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCSVUploadBytes+1))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxCSVUploadBytes {
		http.Error(w, "CSV upload exceeds maximum size", http.StatusRequestEntityTooLarge)
		return
	}

	raw, err := h.csv.Ingest(bytes.NewReader(body), currency)
	if err != nil {
		// Parse errors are the caller's bad input → 422.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	conn, err := h.ensureCSVConnection(r, t.ID, acct)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	res, err := h.sync.IngestRaw(r.Context(), t.ID, conn, raw)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSyncResultResponse(res))
}

// ensureCSVConnection returns the account's existing CSV connection or
// creates one. Reusing the row keeps each account to a single CSV feed so
// the cursor / last_synced_at telemetry stays coherent across uploads.
// A CSV upload is push-based and an explicit re-activation intent, so if
// the existing CSV feed was previously revoked/expired it is reactivated
// here — otherwise the UI would show a revoked connection that is in fact
// actively receiving data.
func (h *bankfeedHandlers) ensureCSVConnection(r *http.Request, tenantID, bankAccountID uuid.UUID) (*bankfeed.Connection, error) {
	existing, err := h.conns.ListConnectionsByAccount(r.Context(), tenantID, bankAccountID)
	if err != nil {
		return nil, err
	}
	for i := range existing {
		if existing[i].Provider != bankfeed.ProviderCSV {
			continue
		}
		conn := &existing[i]
		if conn.Status != bankfeed.StatusActive {
			if err := h.conns.SetStatus(r.Context(), tenantID, conn.ID, bankfeed.StatusActive); err != nil {
				return nil, err
			}
			conn.Status = bankfeed.StatusActive
		}
		return conn, nil
	}
	return h.conns.UpsertConnection(r.Context(), bankfeed.Connection{
		TenantID:      tenantID,
		BankAccountID: bankAccountID,
		Provider:      bankfeed.ProviderCSV,
		Status:        bankfeed.StatusActive,
	})
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

// ruleResponse is the snake_case projection of a bankfeed.Rule. The
// domain struct carries no JSON tags (it predates this HTTP surface), so
// a dedicated DTO keeps the wire contract consistent with the rest of
// the API rather than leaking Go PascalCase field names to the client.
type ruleResponse struct {
	ID                uuid.UUID  `json:"id"`
	Priority          int        `json:"priority"`
	ConditionType     string     `json:"condition_type"`
	ConditionValue    string     `json:"condition_value"`
	TargetAccountCode string     `json:"target_account_code,omitempty"`
	TargetCostCenter  string     `json:"target_cost_center,omitempty"`
	AutoApprove       bool       `json:"auto_approve"`
	BankAccountID     *uuid.UUID `json:"bank_account_id,omitempty"`
	Enabled           bool       `json:"enabled"`
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
}

func toRuleResponse(rule *bankfeed.Rule) ruleResponse {
	return ruleResponse{
		ID:                rule.ID,
		Priority:          rule.Priority,
		ConditionType:     rule.ConditionType,
		ConditionValue:    rule.ConditionValue,
		TargetAccountCode: rule.TargetAccountCode,
		TargetCostCenter:  rule.TargetCostCenter,
		AutoApprove:       rule.AutoApprove,
		BankAccountID:     rule.BankAccountID,
		Enabled:           rule.Enabled,
		CreatedAt:         rule.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:         rule.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func toRuleResponses(rules []bankfeed.Rule) []ruleResponse {
	out := make([]ruleResponse, 0, len(rules))
	for i := range rules {
		out = append(out, toRuleResponse(&rules[i]))
	}
	return out
}

// suggestionResponse is the explicit wire projection of a
// ledger.Suggestion. The domain struct already carries snake_case json
// tags, but projecting through a dedicated DTO — as connections and rules
// do — means a field added to ledger.Suggestion later (e.g. an internal
// scoring detail) cannot silently leak to a tenant. The exposed set is
// the current published contract.
type suggestionResponse struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	TransactionID  uuid.UUID `json:"transaction_id"`
	JournalEntryID uuid.UUID `json:"journal_entry_id"`
	Confidence     float64   `json:"confidence"`
	MatchReason    string    `json:"match_reason"`
	Status         string    `json:"status"`
	CreatedAt      string    `json:"created_at"`
}

func toSuggestionResponse(s *ledger.Suggestion) suggestionResponse {
	return suggestionResponse{
		ID:             s.ID,
		TenantID:       s.TenantID,
		TransactionID:  s.TransactionID,
		JournalEntryID: s.JournalEntryID,
		Confidence:     s.Confidence,
		MatchReason:    s.MatchReason,
		Status:         s.Status,
		CreatedAt:      s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func toSuggestionResponses(sugs []ledger.Suggestion) []suggestionResponse {
	out := make([]suggestionResponse, 0, len(sugs))
	for i := range sugs {
		out = append(out, toSuggestionResponse(&sugs[i]))
	}
	return out
}

type ruleRequest struct {
	Priority          int        `json:"priority"`
	ConditionType     string     `json:"condition_type"`
	ConditionValue    string     `json:"condition_value"`
	TargetAccountCode string     `json:"target_account_code,omitempty"`
	TargetCostCenter  string     `json:"target_cost_center,omitempty"`
	AutoApprove       bool       `json:"auto_approve"`
	BankAccountID     *uuid.UUID `json:"bank_account_id,omitempty"`
	Enabled           bool       `json:"enabled"`
}

func (h *bankfeedHandlers) listRules(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	rules, err := h.rules.ListAllRules(r.Context(), t.ID)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuleResponses(rules))
}

func (h *bankfeedHandlers) createRule(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var req ruleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.rules.UpsertRule(r.Context(), bankfeed.Rule{
		TenantID:          t.ID,
		Priority:          req.Priority,
		ConditionType:     req.ConditionType,
		ConditionValue:    req.ConditionValue,
		TargetAccountCode: req.TargetAccountCode,
		TargetCostCenter:  req.TargetCostCenter,
		AutoApprove:       req.AutoApprove,
		BankAccountID:     req.BankAccountID,
		Enabled:           req.Enabled,
	})
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRuleResponse(out))
}

func (h *bankfeedHandlers) updateRule(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	var req ruleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	out, err := h.rules.UpsertRule(r.Context(), bankfeed.Rule{
		ID:                id,
		TenantID:          t.ID,
		Priority:          req.Priority,
		ConditionType:     req.ConditionType,
		ConditionValue:    req.ConditionValue,
		TargetAccountCode: req.TargetAccountCode,
		TargetCostCenter:  req.TargetCostCenter,
		AutoApprove:       req.AutoApprove,
		BankAccountID:     req.BankAccountID,
		Enabled:           req.Enabled,
	})
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRuleResponse(out))
}

func (h *bankfeedHandlers) deleteRule(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := h.rules.DeleteRule(r.Context(), t.ID, id); err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Match suggestions
// ---------------------------------------------------------------------------

// listSuggestions returns the open (status=suggested) match suggestions
// for a bank account, highest confidence first — the operator's review
// inbox. bank_account_id is required so the query stays account-scoped.
func (h *bankfeedHandlers) listSuggestions(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("bank_account_id"))
	if raw == "" {
		http.Error(w, "bank_account_id query parameter required", http.StatusBadRequest)
		return
	}
	acct, err := uuid.Parse(raw)
	if err != nil {
		http.Error(w, "bank_account_id must be a valid UUID", http.StatusBadRequest)
		return
	}
	sugs, err := h.matcher.ListSuggestions(r.Context(), t.ID, acct)
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSuggestionResponses(sugs))
}

// acceptSuggestion reconciles the line against the suggested journal
// entry, collapses the sibling suggestions, and feeds the learner. The
// actor is attributed in the audit trail.
func (h *bankfeedHandlers) acceptSuggestion(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	out, err := h.matcher.AcceptSuggestion(r.Context(), t.ID, id, actorOrDefault(r.Context()))
	if err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toSuggestionResponse(out))
}

// rejectSuggestion marks a single suggestion rejected without touching
// the transaction, so the operator can dismiss a weak candidate.
func (h *bankfeedHandlers) rejectSuggestion(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	id, ok := parseUUIDParam(w, r, "id")
	if !ok {
		return
	}
	if err := h.matcher.RejectSuggestion(r.Context(), t.ID, id, actorOrDefault(r.Context())); err != nil {
		writeBankFeedError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeBankFeedError maps the bankfeed / matcher errors to HTTP status
// codes consistent with the rest of the API surface. Unknown / unconfigured
// providers are a client-addressable 4xx whose sentinel message is safe to
// echo. An unrecognised error is an internal fault: its detail (which may
// embed SQL / connection strings) is logged server-side via the request
// logger and the client receives only a generic message, so provider
// internals can never leak to a tenant.
func writeBankFeedError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, bankfeed.ErrUnknownProvider):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, bankfeed.ErrNotFound), errors.Is(err, ledger.ErrSuggestionNotFound):
		// A referenced connection / rule / suggestion does not exist under
		// the tenant's scope — a routine 404, not a server fault, so it is
		// not logged as an internal error.
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, ledger.ErrSuggestionConflict):
		// The suggestion exists but is no longer actionable (already
		// decided, or its transaction has since been reconciled). 409.
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, bankfeed.ErrProviderNotConfigured):
		// Provider selected but credentials absent — the operator must
		// configure it (or the deployment is fail-closed). 503-class.
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	case errors.Is(err, bankfeed.ErrUnsupported):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		platform.LoggerFromContext(r.Context()).Error("bankfeed: internal error", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
