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

// devPlaceholderJWTSecret is the literal dev-only KAPP_JWT_SECRET
// shipped in .env.example so `make dev` / docker-compose boot
// without manual setup. It is intentionally a recognisable string
// (not a high-entropy random value) so it stands out in log review.
//
// Any deployment that runs with this exact value AND has not opted
// in via KAPP_ALLOW_DEV_JWT_SECRET=1 is almost certainly a
// misconfiguration — somebody copied .env.example to production
// without rotating the secret. newAuthSigner refuses to boot in
// that state so the misconfiguration surfaces immediately instead
// of after the first forged admin JWT shows up in the audit log.
//
// Keep this constant in sync with the KAPP_JWT_SECRET line in
// .env.example. The string is exported nowhere else; it exists
// solely as the lookup key for the dev-mode gate below.
const devPlaceholderJWTSecret = "dev-only-kapp-jwt-secret-do-not-use-outside-localhost-fLPXuVqo9wKn"

// newAuthSigner reads KAPP_JWT_SECRET / KAPP_JWT_TTL and returns a
// configured HS256 signer. Returns a nil signer when the secret is
// absent so local dev without auth keeps working; callers must guard
// against nil before wiring the signer into middleware.
//
// As a defence-in-depth against operators copying .env.example to a
// real deployment, the function refuses to construct a signer keyed
// on the literal devPlaceholderJWTSecret unless KAPP_ALLOW_DEV_JWT_SECRET=1
// is also set. The dev .env.example sets the opt-in flag explicitly;
// production .env files never should. The check is keyed on string
// equality of the secret value (case-sensitive, exact match) so a
// developer who rotates the value to anything else passes through
// without needing to clear the opt-in.
func newAuthSigner() (*auth.Signer, error) {
	secret := os.Getenv("KAPP_JWT_SECRET")
	if secret == "" {
		return nil, errors.New("KAPP_JWT_SECRET unset")
	}
	if secret == devPlaceholderJWTSecret && os.Getenv("KAPP_ALLOW_DEV_JWT_SECRET") != "1" {
		return nil, errors.New(
			"KAPP_JWT_SECRET is the literal dev-only placeholder from .env.example; " +
				"rotate it to a freshly generated value (e.g. `openssl rand -base64 48`) " +
				"or, for local development against the dev compose stack, explicitly " +
				"opt in by setting KAPP_ALLOW_DEV_JWT_SECRET=1 — the placeholder is the " +
				"same in every checkout of the repository, so anyone with a copy of " +
				".env.example can mint admin-looking tokens against this deployment",
		)
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
