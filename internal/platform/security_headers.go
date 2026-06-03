package platform

import "net/http"

// Header names set by SecurityHeadersMiddleware. Defined as constants
// so the middleware and its tests reference one spelling.
const (
	headerCSP                = "Content-Security-Policy"
	headerFrameOptions       = "X-Frame-Options"
	headerContentTypeOptions = "X-Content-Type-Options"
	headerHSTS               = "Strict-Transport-Security"
)

// Default header values applied when SecurityHeadersConfig leaves a
// field empty. X-Frame-Options=DENY blocks framing (clickjacking),
// X-Content-Type-Options=nosniff stops MIME-sniffing content-type
// confusion attacks, and the HSTS default pins HTTPS for a year
// including subdomains.
const (
	DefaultFrameOptions       = "DENY"
	DefaultContentTypeOptions = "nosniff"
	DefaultHSTS               = "max-age=31536000; includeSubDomains"
)

// SecurityHeadersConfig is the resolved set of response-security
// headers SecurityHeadersMiddleware emits. Zero-value fields fall back
// to the Default* constants for CSP / frame / content-type; HSTS is
// the exception — an empty HSTS string DISABLES the header so local
// dev served over plain HTTP does not pin a browser to HTTPS (which
// would make http://localhost unreachable until the max-age expires).
type SecurityHeadersConfig struct {
	// CSP is the Content-Security-Policy value. Empty falls back to
	// DefaultCSPHeader.
	CSP string
	// FrameOptions is the X-Frame-Options value. Empty falls back to
	// DefaultFrameOptions ("DENY").
	FrameOptions string
	// ContentTypeOptions is the X-Content-Type-Options value. Empty
	// falls back to DefaultContentTypeOptions ("nosniff").
	ContentTypeOptions string
	// HSTS is the Strict-Transport-Security value. Empty DISABLES the
	// header (see type doc) — production wiring sets it explicitly.
	HSTS string
}

// SecurityHeadersConfigFromConfig builds the middleware config from the
// loaded platform Config. CSP comes from cfg.CSPHeader (operator
// override) or DefaultCSPHeader. HSTS is enabled only in production:
// emitting it over the plain-HTTP dev listener would pin browsers to
// HTTPS for the configured max-age and break local development.
func SecurityHeadersConfigFromConfig(cfg *Config) SecurityHeadersConfig {
	csp := DefaultCSPHeader
	if cfg != nil && cfg.CSPHeader != "" {
		csp = cfg.CSPHeader
	}
	hsts := ""
	if cfg != nil && cfg.IsProduction() {
		hsts = DefaultHSTS
	}
	return SecurityHeadersConfig{
		CSP:                csp,
		FrameOptions:       DefaultFrameOptions,
		ContentTypeOptions: DefaultContentTypeOptions,
		HSTS:               hsts,
	}
}

// withDefaults returns a copy of the config with empty CSP / frame /
// content-type fields populated from the Default* constants. HSTS is
// intentionally left as-is so an empty value keeps the header off.
func (c SecurityHeadersConfig) withDefaults() SecurityHeadersConfig {
	if c.CSP == "" {
		c.CSP = DefaultCSPHeader
	}
	if c.FrameOptions == "" {
		c.FrameOptions = DefaultFrameOptions
	}
	if c.ContentTypeOptions == "" {
		c.ContentTypeOptions = DefaultContentTypeOptions
	}
	return c
}

// SecurityHeadersMiddleware sets a baseline set of response-security
// headers on every response: Content-Security-Policy, X-Frame-Options,
// X-Content-Type-Options and (in production) Strict-Transport-Security.
//
// Headers are written to the response header map BEFORE next.ServeHTTP
// so they are flushed even when a downstream handler calls
// WriteHeader. Following the package convention (RequestIDMiddleware,
// TenantMiddleware), the constructor returns a
// func(http.Handler) http.Handler so it composes with chi's Use().
func SecurityHeadersMiddleware(cfg SecurityHeadersConfig) func(http.Handler) http.Handler {
	cfg = cfg.withDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set(headerCSP, cfg.CSP)
			h.Set(headerFrameOptions, cfg.FrameOptions)
			h.Set(headerContentTypeOptions, cfg.ContentTypeOptions)
			if cfg.HSTS != "" {
				h.Set(headerHSTS, cfg.HSTS)
			}
			next.ServeHTTP(w, r)
		})
	}
}
