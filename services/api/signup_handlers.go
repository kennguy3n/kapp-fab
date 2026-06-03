package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// signupHandlers backs POST /api/v1/signup — the public,
// captcha-gated, IP-rate-limited self-service tenant creation
// endpoint. It is the only write surface a not-yet-existing tenant
// can reach, so it is mounted OUTSIDE the JWT gate but behind the
// same captcha + IP limiter the other public POST endpoints use.
//
// Signup does not weaken the fail-closed JWT/authz posture: it never
// issues a Kapp session itself. It verifies the caller's identity via
// the KChat code exchange and creates the tenant; the user then
// authenticates through the normal /api/v1/auth/sso flow, which is
// where the JWT (with the now-resolvable tenant membership) is
// minted.
type signupHandlers struct {
	svc    *tenant.SignupService
	logger *slog.Logger
}

type signupRequest struct {
	KChatCode    string `json:"kchat_code"`
	RedirectURI  string `json:"redirect_uri"`
	CompanyName  string `json:"company_name"`
	Slug         string `json:"slug"`
	Plan         string `json:"plan"`
	Country      string `json:"country"`
	CurrencyCode string `json:"currency_code"`
}

type signupResponse struct {
	TenantID string `json:"tenant_id"`
	Slug     string `json:"slug"`
	Plan     string `json:"plan"`
	UserID   string `json:"user_id"`
	// ProvisionComplete is false when the tenant row was created but
	// AutoProvision (seed / ZK / welcome DM) did not fully complete.
	// The tenant still exists and can be re-provisioned by an
	// operator; the frontend uses this to decide whether to show a
	// "finishing setup" state.
	ProvisionComplete bool `json:"provision_complete"`
}

// signup handles the self-service tenant-creation request.
func (h *signupHandlers) signup(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		http.Error(w, "signup not configured", http.StatusNotImplemented)
		return
	}
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	res, err := h.svc.Signup(r.Context(), tenant.SignupInput{
		KChatCode:    req.KChatCode,
		RedirectURI:  req.RedirectURI,
		CompanyName:  req.CompanyName,
		Slug:         req.Slug,
		Plan:         req.Plan,
		Country:      req.Country,
		CurrencyCode: req.CurrencyCode,
	})
	if err != nil {
		// A partial result (tenant created, provisioning failed) is
		// returned alongside the error. Surface the tenant id so the
		// caller can retry provisioning instead of orphaning the
		// tenant, but answer 500 so the failure is visible.
		if res != nil && res.Tenant != nil {
			if h.logger != nil {
				h.logger.Error("signup: autoprovision incomplete",
					slog.String("tenant_id", res.Tenant.ID.String()),
					slog.String("error", err.Error()),
				)
			}
			writeJSON(w, http.StatusInternalServerError, signupResponse{
				TenantID:          res.Tenant.ID.String(),
				Slug:              res.Tenant.Slug,
				Plan:              res.Tenant.Plan,
				UserID:            userIDString(res.User),
				ProvisionComplete: false,
			})
			return
		}
		h.writeSignupError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, signupResponse{
		TenantID:          res.Tenant.ID.String(),
		Slug:              res.Tenant.Slug,
		Plan:              res.Tenant.Plan,
		UserID:            userIDString(res.User),
		ProvisionComplete: true,
	})
}

func userIDString(u *tenant.User) string {
	if u == nil {
		return ""
	}
	return u.ID.String()
}

// writeSignupError maps signup sentinel errors onto status codes.
func (h *signupHandlers) writeSignupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrInvalidSignup):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, tenant.ErrSlugTaken):
		http.Error(w, "slug already taken", http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// kchatSignupVerifier adapts an auth.KChatClient into a
// tenant.SignupIdentityVerifier. It lives in the API layer (not the
// tenant package) so internal/tenant never imports internal/auth —
// auth already imports tenant, and the reverse edge would be an
// import cycle.
type kchatSignupVerifier struct {
	client auth.KChatClient
}

// VerifySignup exchanges the KChat OAuth code for the caller's
// verified profile and projects it onto tenant.SignupIdentity. A
// failed exchange propagates as an error so signup fails closed: no
// tenant is ever created for an unverified email.
func (v kchatSignupVerifier) VerifySignup(ctx context.Context, code, redirectURI string) (tenant.SignupIdentity, error) {
	if v.client == nil {
		return tenant.SignupIdentity{}, errors.New("signup: kchat client not configured")
	}
	profile, err := v.client.ExchangeCode(ctx, code, redirectURI)
	if err != nil {
		return tenant.SignupIdentity{}, err
	}
	if profile == nil {
		return tenant.SignupIdentity{}, errors.New("signup: empty kchat profile")
	}
	display := profile.DisplayName
	if display == "" {
		display = profile.Username
	}
	return tenant.SignupIdentity{
		KChatUserID: profile.ID,
		Email:       profile.Email,
		DisplayName: display,
	}, nil
}
