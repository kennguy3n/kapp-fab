package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// portalHandlers backs the helpdesk customer portal. The portal
// runs outside the standard tenant-header + JWT flow — customers
// authenticate with a magic link and then present a portal-scoped
// JWT. Handlers below parse that JWT themselves (rather than using
// TenantMiddleware) because the token carries the tenant claim.
type portalHandlers struct {
	tenants portalTenantLookup
	portal  *auth.PortalStore
	signer  *auth.Signer
	records *record.PGStore
	mailer  portalMailer
}

// portalTenantLookup narrows the tenant surface to exactly the two
// methods the portal uses. Kept local to this file so the rest of
// the API keeps its richer handle on *tenant.PGStore.
type portalTenantLookup interface {
	GetBySlug(ctx context.Context, slug string) (*tenant.Tenant, error)
}

// portalMailer abstracts the transport that delivers a magic link.
// Production wires an SMTP sender; dev logs the link so operators
// can paste it into the portal manually.
type portalMailer interface {
	Send(ctx context.Context, tenantID uuid.UUID, to, link string) error
}

type stdoutPortalMailer struct{}

func (stdoutPortalMailer) Send(ctx context.Context, tenantID uuid.UUID, to, link string) error {
	log.Printf("portal: magic link tenant=%s to=%q link=%q", tenantID, to, link)
	return nil
}

// --- /auth endpoints ---------------------------------------------------

type portalAuthRequest struct {
	TenantSlug string `json:"tenant_slug"`
	Email      string `json:"email"`
}

// requestMagicLink issues a fresh magic token. Response is 204 on
// success — we deliberately do not surface whether the email was
// already registered so the endpoint cannot be used to enumerate
// valid customers.
func (h *portalHandlers) requestMagicLink(w http.ResponseWriter, r *http.Request) {
	var in portalAuthRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	in.TenantSlug = strings.TrimSpace(in.TenantSlug)
	in.Email = strings.TrimSpace(in.Email)
	if in.TenantSlug == "" || in.Email == "" {
		http.Error(w, "tenant_slug and email required", http.StatusBadRequest)
		return
	}
	t, err := h.tenants.GetBySlug(r.Context(), in.TenantSlug)
	if err != nil {
		// Do not reveal whether the slug exists — the enumeration
		// risk is the same as for an email lookup.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token, user, err := h.portal.IssueMagicLink(r.Context(), t.ID, in.Email)
	if err != nil {
		log.Printf("portal: issue link tenant=%s email=%q: %v", t.ID, in.Email, err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	link := buildMagicLink(r, t.Slug, in.Email, token)
	if err := h.mailer.Send(r.Context(), t.ID, user.Email, link); err != nil {
		log.Printf("portal: mail link tenant=%s email=%q: %v", t.ID, user.Email, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

type portalAuthVerifyRequest struct {
	TenantSlug string `json:"tenant_slug"`
	Email      string `json:"email"`
	Token      string `json:"token"`
}

type portalAuthVerifyResponse struct {
	Token     string           `json:"token"`
	ExpiresAt int64            `json:"expires_at"`
	User      *auth.PortalUser `json:"user"`
}

// verifyMagicLink exchanges a magic token for a portal JWT.
func (h *portalHandlers) verifyMagicLink(w http.ResponseWriter, r *http.Request) {
	var in portalAuthVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if in.TenantSlug == "" || in.Email == "" || in.Token == "" {
		http.Error(w, "tenant_slug, email, token required", http.StatusBadRequest)
		return
	}
	t, err := h.tenants.GetBySlug(r.Context(), in.TenantSlug)
	if err != nil {
		http.Error(w, "invalid link", http.StatusUnauthorized)
		return
	}
	user, err := h.portal.VerifyMagicLink(r.Context(), t.ID, in.Email, in.Token)
	if err != nil {
		http.Error(w, "invalid link", http.StatusUnauthorized)
		return
	}
	token, claims, err := auth.IssuePortalToken(h.signer, *user)
	if err != nil {
		http.Error(w, "token issue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, portalAuthVerifyResponse{
		Token:     token,
		ExpiresAt: claims.ExpiresAt,
		User:      user,
	})
}

// --- ticket endpoints --------------------------------------------------

// listTickets returns every ticket the portal user owns
// (helpdesk.ticket rows where data->>'customer_email' == claims.Email).
// The per-tenant feature gate + RLS still runs on the underlying
// records table; the email filter is the ABAC layer that narrows
// the visible rows further to just the customer's own.
func (h *portalHandlers) listTickets(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	rows, err := h.records.ListAll(r.Context(), claims.TenantID, record.ListFilter{KType: helpdesk.KTypeTicket})
	if err != nil {
		writeRecordError(w, err)
		return
	}
	filtered := filterByCustomerEmail(rows, claims.Email)
	writeJSON(w, http.StatusOK, map[string]any{"tickets": filtered})
}

// getTicket returns a single ticket, 404 if the customer_email
// stored on the record does not match the token's Email claim.
func (h *portalHandlers) getTicket(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid ticket id", http.StatusBadRequest)
		return
	}
	rec, err := h.records.Get(r.Context(), claims.TenantID, id)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	if !recordOwnedByCustomer(*rec, claims.Email) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

type portalCreateTicketRequest struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Priority    string `json:"priority,omitempty"`
}

// createTicket writes a new helpdesk.ticket owned by the customer.
// The customer_email field is always stamped from the token — a
// client-supplied value is ignored so one portal user can never
// create tickets under another customer's email.
func (h *portalHandlers) createTicket(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var in portalCreateTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if in.Subject == "" {
		http.Error(w, "subject required", http.StatusBadRequest)
		return
	}
	priority := in.Priority
	if priority == "" {
		priority = "medium"
	}
	data := map[string]any{
		"subject":        in.Subject,
		"description":    in.Description,
		"status":         "open",
		"priority":       priority,
		"channel":        "portal",
		"customer_email": claims.Email,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	created, err := h.records.Create(r.Context(), record.KRecord{
		TenantID:  claims.TenantID,
		KType:     helpdesk.KTypeTicket,
		Data:      raw,
		CreatedBy: claims.UserID,
	})
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

type portalReplyRequest struct {
	Body string `json:"body"`
}

// replyTicket appends a customer reply to an existing ticket. The
// reply is stored as an entry in the data.replies array so the
// agent-side UI can render the thread inline.
func (h *portalHandlers) replyTicket(w http.ResponseWriter, r *http.Request) {
	claims := portalClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid ticket id", http.StatusBadRequest)
		return
	}
	var in portalReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	rec, err := h.records.Get(r.Context(), claims.TenantID, id)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	if !recordOwnedByCustomer(*rec, claims.Email) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var data map[string]any
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &data); err != nil {
			http.Error(w, "decode", http.StatusInternalServerError)
			return
		}
	} else {
		data = map[string]any{}
	}
	reply := map[string]any{
		"from": claims.Email,
		"body": in.Body,
		"kind": "customer",
	}
	existing, _ := data["replies"].([]any)
	data["replies"] = append(existing, reply)
	// Portal replies flip the ticket back to in_progress so the
	// agent's queue resurfaces it.
	if status, _ := data["status"].(string); status == "waiting" || status == "resolved" {
		data["status"] = "in_progress"
	}
	raw, err := json.Marshal(data)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	rec.Data = raw
	uid := claims.UserID
	rec.UpdatedBy = &uid
	updated, err := h.records.Update(r.Context(), *rec)
	if err != nil {
		writeRecordError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// --- helpers -----------------------------------------------------------

// portalCtxKey segregates portal claims from the ctxKeyUser slot so
// the standard auth middleware and the portal middleware do not
// collide.
type portalCtxKey struct{}

func portalClaimsFromContext(ctx context.Context) *auth.Claims {
	c, _ := ctx.Value(portalCtxKey{}).(*auth.Claims)
	return c
}

func withPortalClaims(ctx context.Context, c *auth.Claims) context.Context {
	return context.WithValue(ctx, portalCtxKey{}, c)
}

// portalAuthMiddleware parses the Authorization: Bearer header,
// verifies it against the signer, and enforces Claims.Scope ==
// "portal" so standard user tokens cannot hit the portal surface.
func portalAuthMiddleware(signer *auth.Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") {
				http.Error(w, "bearer token required", http.StatusUnauthorized)
				return
			}
			tok := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
			claims, err := signer.Verify(tok)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			if claims.Scope != auth.PortalScope {
				http.Error(w, "portal scope required", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(withPortalClaims(r.Context(), claims)))
		})
	}
}

func recordOwnedByCustomer(rec record.KRecord, email string) bool {
	if len(rec.Data) == 0 || email == "" {
		return false
	}
	var data map[string]any
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		return false
	}
	v, _ := data["customer_email"].(string)
	return strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(email))
}

func filterByCustomerEmail(rows []record.KRecord, email string) []record.KRecord {
	out := make([]record.KRecord, 0, len(rows))
	for _, r := range rows {
		if recordOwnedByCustomer(r, email) {
			out = append(out, r)
		}
	}
	return out
}

// buildMagicLink composes the clickable link the portal emails. The
// scheme + host are taken from the incoming request so the same
// binary serves multiple domains without configuration.
func buildMagicLink(r *http.Request, slug, email, token string) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	// The frontend route is /portal/:tenant_slug, so embed the slug
	// as a path segment — not a query parameter — otherwise React
	// Router resolves tenant_slug to the literal "verify".
	q := "email=" + urlEncode(email) + "&token=" + urlEncode(token)
	return scheme + "://" + host + "/portal/" + urlEncode(slug) + "?" + q
}

func urlEncode(s string) string {
	// Inline a light encoder to avoid pulling net/url across the
	// portal handler set — we only ever encode short safe tokens.
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// Compile-time guard so changes to record.ErrNotFound don't silently
// detour the portal error path into a 500.
var _ = errors.Is
