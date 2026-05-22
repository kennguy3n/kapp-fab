// HTTP request-id middleware: end-to-end correlation across services.
//
// Phase 4.2: every inbound HTTP request gets a request_id (either
// honoured from the incoming X-Request-ID header or freshly minted),
// surfaced back to the caller in the response header, attached to the
// ctx-scoped slog.Logger, and made available to NATS publish + DB
// query call sites via RequestIDFromContext.
//
// Why an explicit middleware instead of relying on chi/middleware.RequestID:
//
//   1. chi's RequestID stores the value under a private context key, so
//      we cannot retrieve it from non-chi paths (NATS handlers, scheduler
//      ticks). The platform-owned key exported here makes the value
//      reachable across the whole codebase.
//   2. chi's RequestID generates a "{nodeID}/{counter}" id that does NOT
//      survive cross-service hops cleanly — a downstream call gets a
//      fresh id rather than the original. The middleware in this file
//      honours an incoming X-Request-ID, so a single user-initiated
//      request flowing api → worker → kchat-bridge keeps one id end-to-end.
//   3. chi's middleware does not write the id into a ctx-scoped logger
//      or echo it back in the response. We need both for cross-service
//      correlation in production incident postmortems.
package platform

import (
	"log/slog"
	"net/http"
	"strings"
)

// RequestIDHeader is the canonical wire header name for the request id.
// HTTP header names are case-insensitive on the wire; we echo back in
// canonical case for compatibility with log-pipeline scrapers that key
// off the literal header.
const RequestIDHeader = "X-Request-ID"

// MaxIncomingRequestIDLen is the maximum length of an incoming
// X-Request-ID we will honour. Anything longer is silently replaced
// with a freshly minted id — an unbounded incoming header is a log-
// poisoning vector. 128 chars is enough for any reasonable UUID /
// nanoid / opentelemetry trace_id encoding.
const MaxIncomingRequestIDLen = 128

// RequestIDMiddleware installs a per-request id on the request
// context, derives a ctx-scoped slog.Logger pre-tagged with the id,
// and echoes the id back to the caller via the response header.
//
// Order: this middleware MUST run BEFORE any middleware that emits log
// lines (tenant lookup, auth, authz, idempotency, rate limit, etc.) so
// those lines carry the request_id attribute. In services/* main.go
// chains it is the first chi.Use call after Recoverer.
//
// The supplied base logger is the per-service logger created by
// NewLogger; the per-request logger is derived as
// base.With("request_id", id) so it carries both service-level attrs
// (service, env) and the per-request id.
func RequestIDMiddleware(base *slog.Logger) func(http.Handler) http.Handler {
	if base == nil {
		base = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := sanitizeIncomingRequestID(r.Header.Get(RequestIDHeader))
			if id == "" {
				id = NewRequestID()
			}
			// Echo back BEFORE WriteHeader so downstream handlers can
			// still mutate it via the ctx-scoped logger but the
			// response carries the canonical value even if the
			// handler panics (chi.middleware.Recoverer above
			// converts panics to 500s and the header is preserved).
			w.Header().Set(RequestIDHeader, id)

			ctx := r.Context()
			ctx = WithRequestID(ctx, id)
			logger := base.With(slog.String("request_id", id))
			ctx = WithLogger(ctx, logger)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sanitizeIncomingRequestID rejects values that are too long or
// contain non-printable / non-ASCII characters. A clean rejection
// (return "") triggers minting a fresh id; we do NOT silently
// truncate or rewrite the incoming value because that would mask
// caller misconfiguration.
//
// Allowed character set: ASCII printable (0x20–0x7E) minus whitespace
// boundary cases. Length cap MaxIncomingRequestIDLen. Empty input is
// treated as absent (mint fresh).
func sanitizeIncomingRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) > MaxIncomingRequestIDLen {
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x21 || c > 0x7E {
			return ""
		}
	}
	return raw
}
