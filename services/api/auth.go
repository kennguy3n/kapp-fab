package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

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

// newAuthSigner reads KAPP_JWT_SECRET / KAPP_JWT_TTL and returns a
// configured HS256 signer. Returns a nil signer when the secret is
// absent so local dev without auth keeps working; callers must guard
// against nil before wiring the signer into middleware.
func newAuthSigner() (*auth.Signer, error) {
	secret := os.Getenv("KAPP_JWT_SECRET")
	if secret == "" {
		return nil, errors.New("KAPP_JWT_SECRET unset")
	}
	access := parseDurationOr("KAPP_JWT_ACCESS_TTL", 15*time.Minute)
	refresh := parseDurationOr("KAPP_JWT_REFRESH_TTL", 24*time.Hour)
	issuer := envOr("KAPP_JWT_ISSUER", "kapp")
	audience := envOr("KAPP_JWT_AUDIENCE", "kapp")
	return auth.NewSigner(auth.SignerConfig{
		Algorithm:  auth.AlgHS256,
		HMACKey:    []byte(secret),
		Issuer:     issuer,
		Audience:   audience,
		AccessTTL:  access,
		RefreshTTL: refresh,
		Leeway:     30 * time.Second,
	})
}

func parseDurationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	log.Printf("auth: %s=%q not parseable as duration; using %s", key, v, def)
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
