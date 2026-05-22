package main

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/auth"
)

// authHandlers owns the POST /api/v1/auth/sso and POST
// /api/v1/auth/refresh endpoints. Both paths are unauthenticated —
// SSO takes a KChat code, refresh takes a refresh token. The signer
// and SSO service are built at boot from environment variables.
type authHandlers struct {
	svc    *auth.SSOService
	signer *auth.Signer
}

// ssoRequest is the JSON payload POST /api/v1/auth/sso accepts.
type ssoRequest struct {
	Code            string    `json:"code"`
	RedirectURI     string    `json:"redirect_uri"`
	PreferredTenant uuid.UUID `json:"preferred_tenant,omitempty"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *authHandlers) sso(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		http.Error(w, "sso not configured", http.StatusServiceUnavailable)
		return
	}
	var req ssoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Code == "" || req.RedirectURI == "" {
		http.Error(w, "code and redirect_uri required", http.StatusBadRequest)
		return
	}
	res, err := h.svc.Exchange(
		r.Context(), req.Code, req.RedirectURI,
		req.PreferredTenant, r.UserAgent(), r.RemoteAddr,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *authHandlers) refresh(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		http.Error(w, "sso not configured", http.StatusServiceUnavailable)
		return
	}
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.RefreshToken == "" {
		http.Error(w, "refresh_token required", http.StatusBadRequest)
		return
	}
	res, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// newAuthSigner is the api gateway's thin wrapper around
// auth.SignerFromEnv. It exists for source-grep stability (older
// PRs reference newAuthSigner by name) and for any future api-
// specific signer-construction behaviour (e.g. asymmetric keys
// loaded from KMS) that wouldn't make sense to share with the
// sidecar services. The dev-placeholder guard, TTL parsing, and
// issuer / audience defaults all live in auth.SignerFromEnv so the
// importer + agent-tools sidecars get the same contract without
// copy-paste drift.
func newAuthSigner() (*auth.Signer, error) {
	return auth.SignerFromEnv()
}
