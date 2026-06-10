package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/auth"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// oidcLoginStateTTL bounds the lifetime of the sealed login-state
// cookie (and the codec's own staleness check). A user must complete
// the authorize → callback round-trip within this window; it is long
// enough to survive a slow MFA/passkey prompt yet short enough that a
// captured cookie has little replay value.
const oidcLoginStateTTL = 10 * time.Minute

// Cookie names for the iam-core login flow. The state cookie is
// host-only, httpOnly, and scoped to the callback path so it is sent
// on the smallest possible surface. The refresh cookie is scoped to
// the auth route group so it never rides along on ordinary API calls.
const (
	oidcStateCookie   = "kapp_oidc_state"
	oidcRefreshCookie = "kapp_oidc_rt"
	oidcRefreshPath   = "/api/v1/auth"
)

// resolveOIDCStateKey picks the AES-256 key that seals the login-state
// cookie. Order: explicit base64 IAM_CORE_COOKIE_KEY → key derived
// from KAPP_JWT_SECRET → ephemeral random (dev only). See the field
// docs on platform.Config.IAMCoreCookieKey for the rationale.
func resolveOIDCStateKey(cfg *platform.Config) ([]byte, error) {
	if raw := strings.TrimSpace(cfg.IAMCoreCookieKey); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			// Tolerate URL-safe base64 too — operators paste either.
			key, err = base64.RawURLEncoding.DecodeString(raw)
		}
		if err != nil {
			return nil, fmt.Errorf("decode IAM_CORE_COOKIE_KEY: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("IAM_CORE_COOKIE_KEY must decode to 32 bytes, got %d", len(key))
		}
		return key, nil
	}
	if secret := jwtSecretForDerivation(); secret != "" {
		return auth.DeriveOIDCStateKey(secret)
	}
	// Dev fallback: ephemeral key. The production gate guarantees we
	// never reach here in production.
	log.Printf("api: WARN iam-core login-state key is ephemeral (no IAM_CORE_COOKIE_KEY / KAPP_JWT_SECRET); logins break across restarts and replicas — set IAM_CORE_COOKIE_KEY for any multi-instance deploy")
	return auth.GenerateOIDCStateKey()
}

// envIsHTTPS reports whether a configured URL is https, used to decide
// the Secure cookie attribute when the deployment env is not
// explicitly "production".
func envIsHTTPS(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	return err == nil && u.Scheme == "https"
}

// login begins the Authorization-Code + PKCE flow: it mints a fresh
// state/nonce/verifier, seals them into a short-lived host-only cookie,
// and 302-redirects the browser to iam-core's authorize endpoint. An
// optional ?return_to=<app-relative-path> is preserved across the
// round-trip (validated to be same-site before the callback uses it).
func (h *authHandlers) login(w http.ResponseWriter, r *http.Request) {
	if h.iam == nil || h.iam.OAuth2() == nil || h.stateCodec == nil {
		http.Error(w, "iam-core login not configured", http.StatusServiceUnavailable)
		return
	}
	areq, err := h.iam.OAuth2().NewAuthRequest()
	if err != nil {
		log.Printf("api: oidc login: build auth request: %v", err)
		http.Error(w, "login unavailable", http.StatusBadGateway)
		return
	}
	sealed, err := h.stateCodec.Seal(auth.OIDCLoginState{
		State:    areq.State,
		Nonce:    areq.Nonce,
		Verifier: areq.Verifier,
		ReturnTo: sanitizeReturnTo(r.URL.Query().Get("return_to")),
	})
	if err != nil {
		log.Printf("api: oidc login: seal state: %v", err)
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    sealed,
		Path:     oidcRefreshPath + "/callback",
		MaxAge:   int(oidcLoginStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, areq.URL, http.StatusFound)
}

// callback completes the flow: it validates the state cookie against
// the returned state, exchanges the code (with the PKCE verifier) for
// tokens, verifies the id_token's nonce, sets the refresh token in an
// httpOnly cookie, and redirects to the SPA with the access token in
// the URL fragment for the front-end to pick up.
func (h *authHandlers) callback(w http.ResponseWriter, r *http.Request) {
	if h.iam == nil || h.iam.OAuth2() == nil || h.stateCodec == nil {
		http.Error(w, "iam-core login not configured", http.StatusServiceUnavailable)
		return
	}
	// Always clear the one-shot state cookie, success or failure.
	h.clearCookie(w, oidcStateCookie, oidcRefreshPath+"/callback")

	q := r.URL.Query()
	if errCode := q.Get("error"); errCode != "" {
		// iam-core declined (consent denied, login_required, …). Surface
		// it without leaking the raw description into logs verbatim.
		log.Printf("api: oidc callback: provider error %q", errCode)
		http.Error(w, "login failed: "+sanitizeErrorCode(errCode), http.StatusUnauthorized)
		return
	}

	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil {
		http.Error(w, "login session expired; retry", http.StatusBadRequest)
		return
	}
	st, err := h.stateCodec.Open(cookie.Value)
	if err != nil {
		log.Printf("api: oidc callback: open state: %v", err)
		http.Error(w, "login session invalid; retry", http.StatusBadRequest)
		return
	}
	// Constant-time-ish state match (defeats CSRF on the callback).
	if got := q.Get("state"); got == "" || got != st.State {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := h.iam.OAuth2().Exchange(ctx, code, st.Verifier)
	if err != nil {
		log.Printf("api: oidc callback: token exchange: %v", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	// Verify the id_token and its nonce binding. A token response with
	// no id_token, or a nonce that does not match the one we minted,
	// means the response was injected/replayed — reject it.
	if _, err := h.iam.OAuth2().VerifyIDToken(tok.IDToken, st.Nonce); err != nil {
		log.Printf("api: oidc callback: id_token verify: %v", err)
		http.Error(w, "id_token validation failed", http.StatusUnauthorized)
		return
	}

	// Stash the refresh token (if granted) in an httpOnly cookie scoped
	// to the auth route group. It is never exposed to JS; the SPA calls
	// POST /api/v1/auth/oidc/refresh to rotate it for a fresh access
	// token. iam-core rotates refresh tokens, so each refresh replaces
	// this cookie.
	if tok.RefreshToken != "" {
		h.setRefreshCookie(w, tok.RefreshToken)
	}

	// Deliver the access token (and id_token, for the logout hint) to
	// the SPA via the URL fragment — it never hits the server or logs,
	// and the front-end clears it from the address bar on load.
	frag := url.Values{}
	frag.Set("access_token", tok.AccessToken)
	frag.Set("token_type", orDefault(tok.TokenType, "Bearer"))
	if tok.ExpiresIn > 0 {
		frag.Set("expires_in", fmt.Sprintf("%d", tok.ExpiresIn))
	}
	if tok.IDToken != "" {
		frag.Set("id_token", tok.IDToken)
	}
	// The access token rides in the URL *fragment*, which only the SPA
	// callback page knows how to read — so the fragment must always be
	// delivered to that page, never to the user's final destination
	// (which would silently drop the tokens). The user's intended
	// landing path (ReturnTo) is forwarded as a same-site query param
	// the SPA callback navigates to once it has persisted the tokens.
	dest := h.spaCallbackPath()
	if rt := sanitizeReturnTo(st.ReturnTo); rt != "" {
		sep := "?"
		if strings.Contains(dest, "?") {
			sep = "&"
		}
		dest += sep + "return_to=" + url.QueryEscape(rt)
	}
	dest += "#" + frag.Encode()
	http.Redirect(w, r, dest, http.StatusFound)
}

// oidcRefresh rotates the httpOnly refresh-token cookie for a fresh
// access token. This is the SPA's silent-renewal path: the browser
// holds no refresh token in JS, so it POSTs here (the cookie rides
// along on the /api/v1/auth scope) and gets back a new access token.
// iam-core rotates the refresh token, so we overwrite the cookie with
// the returned one. Kept distinct from the legacy /refresh route,
// which serves Kapp HS256 tokens and stays unchanged.
func (h *authHandlers) oidcRefresh(w http.ResponseWriter, r *http.Request) {
	if h.iam == nil || h.iam.OAuth2() == nil {
		http.Error(w, "iam-core login not configured", http.StatusServiceUnavailable)
		return
	}
	cookie, err := r.Cookie(oidcRefreshCookie)
	if err != nil || cookie.Value == "" {
		http.Error(w, "no refresh session", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := h.iam.OAuth2().Refresh(ctx, cookie.Value)
	if err != nil {
		// A failed refresh (expired / rotated-token reuse / revoked
		// family) means the session is dead — clear the cookie so the
		// SPA falls back to a full /login rather than retrying.
		h.clearCookie(w, oidcRefreshCookie, oidcRefreshPath)
		log.Printf("api: oidc refresh: %v", err)
		http.Error(w, "refresh failed", http.StatusUnauthorized)
		return
	}
	if tok.RefreshToken != "" {
		h.setRefreshCookie(w, tok.RefreshToken)
	}
	resp := map[string]any{
		"access_token": tok.AccessToken,
		"token_type":   orDefault(tok.TokenType, "Bearer"),
	}
	if tok.ExpiresIn > 0 {
		resp["expires_in"] = tok.ExpiresIn
	}
	if tok.IDToken != "" {
		resp["id_token"] = tok.IDToken
	}
	writeJSON(w, http.StatusOK, resp)
}

// logout clears the local refresh cookie, best-effort revokes the
// refresh token at iam-core, and returns iam-core's end-session URL so
// the SPA can complete single-logout by navigating to it. The id_token
// (sent by the SPA as a hint) lets iam-core skip its logout prompt.
func (h *authHandlers) logout(w http.ResponseWriter, r *http.Request) {
	if h.iam == nil || h.iam.OAuth2() == nil {
		http.Error(w, "iam-core login not configured", http.StatusServiceUnavailable)
		return
	}
	// Revoke the refresh token if we hold one, then clear the cookie
	// regardless of the revocation result (RFC 7009 guidance).
	if cookie, err := r.Cookie(oidcRefreshCookie); err == nil && cookie.Value != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if rerr := h.iam.OAuth2().Revoke(ctx, cookie.Value, "refresh_token"); rerr != nil {
			log.Printf("api: oidc logout: revoke refresh token: %v", rerr)
		}
		cancel()
	}
	h.clearCookie(w, oidcRefreshCookie, oidcRefreshPath)

	idTokenHint := strings.TrimSpace(r.URL.Query().Get("id_token_hint"))
	logoutURL := h.iam.OAuth2().LogoutURL(h.logoutRedirectTarget(), idTokenHint)
	writeJSON(w, http.StatusOK, map[string]any{"logout_url": logoutURL})
}

// userinfo proxies the OIDC userinfo endpoint using the caller's
// bearer access token. It is an optional convenience for the SPA to
// fetch the canonical profile without decoding the id_token itself.
func (h *authHandlers) userinfo(w http.ResponseWriter, r *http.Request) {
	if h.iam == nil || h.iam.OAuth2() == nil {
		http.Error(w, "iam-core login not configured", http.StatusServiceUnavailable)
		return
	}
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "bearer token required", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	info, err := h.iam.OAuth2().UserInfo(ctx, token)
	if err != nil {
		log.Printf("api: oidc userinfo: %v", err)
		http.Error(w, "userinfo unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// --- helpers ---------------------------------------------------------

func (h *authHandlers) setRefreshCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcRefreshCookie,
		Value:    value,
		Path:     oidcRefreshPath,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		// Session cookie (no Max-Age): the refresh token's real
		// lifetime is enforced by iam-core, and a browser-session
		// cookie avoids persisting it to disk.
	})
}

func (h *authHandlers) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// spaCallbackPath is the front-end route that parses the login token
// fragment (access_token/id_token in location.hash) and persists it
// into the SPA's storage. The callback redirect must always target
// this page; pointing it elsewhere drops the tokens because no other
// route reads the fragment. It defaults to "/callback" so a zero-config
// deployment works out of the box, and stays overridable via
// IAM_CORE_POST_LOGIN_REDIRECT for SPAs that mount the handler on a
// different path.
func (h *authHandlers) spaCallbackPath() string {
	if p := sanitizeReturnTo(h.postLoginRedirect); p != "" {
		return p
	}
	return "/callback"
}

func (h *authHandlers) logoutRedirectTarget() string {
	if t := strings.TrimSpace(h.postLogoutRedirect); t != "" {
		return t
	}
	return "/"
}

// sanitizeReturnTo accepts only same-site absolute paths ("/foo"),
// rejecting absolute URLs, protocol-relative ("//evil"), and
// backslash-smuggled variants so the post-login redirect cannot be
// turned into an open redirect. Returns "" when the input is unsafe.
func sanitizeReturnTo(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return ""
	}
	if strings.ContainsAny(p, "\\") {
		return ""
	}
	// Reject anything that parses with a scheme or host.
	if u, err := url.Parse(p); err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	return p
}

// sanitizeErrorCode allows only the RFC 6749 error-code charset so a
// crafted ?error= value cannot inject markup into the plain-text error
// response.
func sanitizeErrorCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 64 {
		code = code[:64]
	}
	for _, r := range code {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "invalid_request"
		}
	}
	if code == "" {
		return "invalid_request"
	}
	return code
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// bearerToken extracts the token from an Authorization: Bearer header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// jwtSecretForDerivation returns the symmetric JWT secret used to
// derive a stable login-state key when no dedicated cookie key is set.
// It reads the same env var the config loader uses; an empty result
// pushes resolveOIDCStateKey to its dev-only ephemeral fallback.
func jwtSecretForDerivation() string {
	return strings.TrimSpace(os.Getenv("KAPP_JWT_SECRET"))
}
