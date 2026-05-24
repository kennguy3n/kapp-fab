// Package csrf provides cross-site request forgery protections for
// HTTP requests. The package implements two complementary defences
// that an HTTP gateway can compose in front of mutating endpoints:
//
//  1. Origin / Referer allowlist (always-on, zero-state).  Mutating
//     requests must carry an Origin or Referer header whose host
//     matches the configured allowlist.  This single check defeats
//     the standard same-site-cookie CSRF attack class because
//     browsers will not lie about Origin to a different origin.
//     For Kapp's current bearer-token-only deployment, this is the
//     load-bearing defence: an attacker on evil.com cannot get a
//     victim's browser to send the Authorization header to api
//     .kapp.com (browsers don't auto-attach Authorization across
//     origins), so adding the Origin check gives an additional
//     belt-and-braces guarantee for any future cookie-auth surface.
//
//  2. Double-submit cookie (opt-in via config). The middleware
//     issues a non-HttpOnly random-token cookie on GET responses
//     and requires the same value to be echoed back in the X-CSRF-
//     Token header on mutating requests. The two values are
//     compared with crypto/subtle. This defeats the case where an
//     attacker controls a same-site subdomain that can read
//     cookies but not headers (uncommon, but a possible state if
//     Kapp ever serves customer-controlled subdomains).
//
// # Threat model
//
// The package defends a server that authenticates either via:
//
//   - Cookie-based session (vulnerable to classic CSRF — browser
//     auto-attaches cookies cross-origin)
//   - Authorization header / Bearer token (NOT vulnerable to
//     classic CSRF — browsers won't auto-attach Authorization
//     cross-origin from a different page's fetch())
//
// Bearer-token requests bypass the Verify middleware (no cookie
// to forge, so the threat doesn't apply); cookie-auth requests
// must pass both Origin and double-submit checks.
//
// We deliberately do NOT use Synchronizer-Token pattern (server
// stores per-session token in DB / Redis) because:
//
//   - It requires session state, which clashes with Kapp's
//     stateless JWT design.
//   - It doesn't add a meaningful defence over double-submit +
//     Origin check on real-world threat trees.
//   - It introduces a state-mutation surface on every read
//     request, which has its own scaling cost.
//
// # Safe-method bypass
//
// GET, HEAD, OPTIONS, TRACE skip Verify because they're idempotent
// per RFC 7231 §4.2.1 and shouldn't carry side effects an attacker
// can weaponise. Any handler that mutates state under a safe
// method is broken in a way CSRF middleware can't fix — the right
// fix is to move the handler to POST / PATCH / DELETE.
package csrf

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Config is the operator-supplied configuration the middleware
// consumes. Empty AllowedOrigins disables the Origin check (NOT
// recommended for production); empty CookieName disables the
// double-submit cookie path (Origin check still runs).
type Config struct {
	// AllowedOrigins is the list of origins (scheme://host[:port])
	// considered safe for mutating requests. Compared verbatim
	// against the request's Origin header. The Referer header is
	// used as a fallback when Origin is absent (older browsers,
	// some privacy-extension shapes).
	//
	// MUST contain at least one entry for production; a deployment
	// behind a CDN should list the public origin, NOT the internal
	// load-balancer hostname. Wildcards are not supported — if you
	// need multi-tenant SaaS where each tenant has its own
	// subdomain, list them all (or move to a tenant-aware origin
	// resolver, not implemented here).
	AllowedOrigins []string

	// CookieName is the name of the double-submit cookie. Empty
	// disables the double-submit check. Best practice is to use a
	// __Host- prefixed name (e.g. "__Host-kapp-csrf") in
	// production so the cookie is bound to the exact origin and
	// can't be set by a subdomain.
	CookieName string

	// CookieDomain limits the cookie to a specific domain. Leave
	// empty for the request's host (matches __Host- prefix
	// requirements).
	CookieDomain string

	// CookiePath limits the cookie to a path prefix. Default "/".
	CookiePath string

	// CookieSecure controls the Secure flag. SHOULD be true in
	// production (HTTPS); set false only for local-dev HTTP.
	CookieSecure bool

	// CookieSameSite controls the SameSite flag. Default Lax;
	// Strict locks out same-site link clicks (often surprising
	// for login flows); None requires Secure=true.
	CookieSameSite http.SameSite

	// HeaderName is the request header name carrying the echoed
	// token. Default "X-CSRF-Token".
	HeaderName string

	// SkipBearerAuth, when true, bypasses CSRF verification for
	// requests carrying an "Authorization: Bearer ..." header.
	// This is safe because browsers do not auto-attach
	// Authorization cross-origin (the threat model CSRF defends
	// against does not apply to bearer-token requests).
	//
	// The zero value is false (CSRF runs for bearer-auth requests
	// too). Construct csrf.Config{...SkipBearerAuth: true} when
	// you want bearer requests to bypass the check — there is no
	// implicit default set by withDefaults() because the safe
	// posture depends on the deployment shape and a silent flip
	// to true would be a security regression for cookie-auth
	// surfaces.
	SkipBearerAuth bool

	// Skipper, when set, is consulted before every other check.
	// Returning true causes the middleware to pass the request
	// through unconditionally. Used by gateways that mount the
	// CSRF middleware globally but need to exempt deliberately
	// embeddable public endpoints (e.g. POST /forms/{id}/submit,
	// signed webhook receivers) from the Origin allowlist. Nil
	// disables the hook (no requests are skipped on this path).
	Skipper func(*http.Request) bool
}

// withDefaults fills in zero values with sensible defaults.
//
// SkipBearerAuth and Skipper are intentionally not defaulted here:
// both fields encode security-sensitive policy ("should bearer-auth
// requests bypass CSRF", "which paths should bypass CSRF") and a
// silent default flip from the zero value to true would be a
// regression for cookie-auth deployments. Callers must set them
// explicitly when the gateway needs the bypass.
func (c Config) withDefaults() Config {
	if c.CookiePath == "" {
		c.CookiePath = "/"
	}
	if c.CookieSameSite == 0 {
		c.CookieSameSite = http.SameSiteLaxMode
	}
	if c.HeaderName == "" {
		c.HeaderName = "X-CSRF-Token"
	}
	return c
}

// IsSafeMethod reports whether the HTTP method is idempotent per
// RFC 7231. The CSRF middleware bypasses verification for safe
// methods because they shouldn't carry side effects an attacker
// can weaponise via a forged request.
func IsSafeMethod(m string) bool {
	switch strings.ToUpper(m) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// IssueCookie writes a fresh CSRF cookie on the response. Call
// this on response to a GET / HEAD that returns HTML or otherwise
// initialises a frontend session. The middleware does NOT issue
// cookies automatically because not every safe request is a
// session bootstrap — issuing on every GET would mean cookie
// churn on API calls that don't need it.
func IssueCookie(w http.ResponseWriter, _ *http.Request, cfg Config) (string, error) {
	cfg = cfg.withDefaults()
	if cfg.CookieName == "" {
		return "", errors.New("csrf: cookie name not configured")
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	cookie := &http.Cookie{
		Name:     cfg.CookieName,
		Value:    token,
		Path:     cfg.CookiePath,
		Domain:   cfg.CookieDomain,
		Secure:   cfg.CookieSecure,
		SameSite: cfg.CookieSameSite,
		// Deliberately NOT HttpOnly — the JS client needs to
		// read this cookie to echo its value in the X-CSRF-
		// Token header. The double-submit defence relies on
		// the cookie being readable by same-origin JS.
		HttpOnly: false,
	}
	http.SetCookie(w, cookie)
	return token, nil
}

// generateToken returns a 32-byte cryptographically-random token
// base64-encoded. 32 bytes gives 192 bits of entropy after base64
// expansion (the encoding rounds 32 bytes to 44 chars including
// padding; we strip padding with RawURLEncoding).
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("csrf: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Verify runs the CSRF checks for a request. Returns nil when the
// request is safe-method, bearer-token (and SkipBearerAuth is set),
// or passes both Origin and double-submit checks. Returns an error
// describing which check failed otherwise.
//
// Callers (typically the chi middleware below) should treat a
// non-nil return as a 403 Forbidden response. The error message is
// deliberately generic at the HTTP layer to avoid leaking which
// check failed to an attacker; the verbose form is suitable for
// structured-log diagnostics.
func Verify(r *http.Request, cfg Config) error {
	cfg = cfg.withDefaults()
	if IsSafeMethod(r.Method) {
		return nil
	}
	if cfg.Skipper != nil && cfg.Skipper(r) {
		return nil
	}
	if cfg.SkipBearerAuth {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return nil
		}
	}
	if err := checkOrigin(r, cfg.AllowedOrigins); err != nil {
		return err
	}
	if cfg.CookieName == "" {
		// Double-submit disabled — Origin check is the only
		// defence; we already passed it, so accept.
		return nil
	}
	cookie, err := r.Cookie(cfg.CookieName)
	if err != nil || cookie.Value == "" {
		return fmt.Errorf("csrf: missing %s cookie", cfg.CookieName)
	}
	got := r.Header.Get(cfg.HeaderName)
	if got == "" {
		return fmt.Errorf("csrf: missing %s header", cfg.HeaderName)
	}
	// Constant-time compare so the verifier doesn't leak the
	// token via timing side-channels.
	if subtle.ConstantTimeCompare([]byte(got), []byte(cookie.Value)) != 1 {
		return errors.New("csrf: header / cookie token mismatch")
	}
	return nil
}

// checkOrigin verifies that the request's Origin (or Referer
// fallback) is in the allowlist. When the allowlist is empty the
// check is disabled — production deployments MUST populate it.
func checkOrigin(r *http.Request, allowed []string) error {
	if len(allowed) == 0 {
		return nil
	}
	// Prefer Origin: it's set on all CORS-relevant fetches and
	// some same-origin POSTs. Fall back to Referer (older clients,
	// some privacy-extension shapes that strip Origin but leave
	// Referer in place).
	origin := r.Header.Get("Origin")
	if origin == "" {
		ref := r.Header.Get("Referer")
		if ref == "" {
			return errors.New("csrf: missing Origin and Referer headers")
		}
		// Parse the referer to extract scheme://host[:port]
		u, err := url.Parse(ref)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return errors.New("csrf: malformed Referer header")
		}
		origin = u.Scheme + "://" + u.Host
	}
	for _, a := range allowed {
		if origin == a {
			return nil
		}
	}
	return fmt.Errorf("csrf: origin %q not in allowlist", origin)
}

// Middleware returns an HTTP middleware that runs Verify and
// rejects mismatched requests with 403 Forbidden. Construct once
// at startup and mount in the router middleware chain. A non-
// matching CSRF check writes a generic "forbidden" body to avoid
// telling an attacker which check failed; the underlying error is
// surfaced through the optional Logger hook so operators can see
// it in structured logs.
func Middleware(cfg Config, logger func(r *http.Request, err error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := Verify(r, cfg); err != nil {
				if logger != nil {
					logger(r, err)
				}
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
