// HTTP tracing middleware: wraps otelhttp + threads trace_id /
// span_id into the existing ctx-scoped slog.Logger so structured
// logs join trace data without an external collector pipeline.
//
// Order in the middleware chain. TracingMiddleware MUST run AFTER
// RequestIDMiddleware so the ctx-scoped logger already exists when we
// attach the trace_id attribute. The shape is:
//
//   r.Use(middleware.RealIP)
//   r.Use(middleware.Recoverer)
//   r.Use(platform.RequestIDMiddleware(logger))  // installs ctx logger
//   r.Use(platform.TracingMiddleware(serviceName))  // starts the span,
//                                                    // augments logger
//   r.Use(platform.MetricsMiddleware(metrics))
//   ...
//
// The middleware uses otelhttp.NewHandler under the hood so the W3C
// TraceContext header extraction, span kind annotation, and
// otelhttp.RequestSize/ResponseSize metrics are all picked up from
// the standard contrib package. We add only the slog bridge: pulling
// trace_id + span_id off the post-extraction span context and
// re-deriving the per-request logger so every log line inside the
// handler carries the trace attributes.
package platform

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// TracingMiddleware returns a chi-compatible middleware that wraps
// otelhttp.NewHandler around the next handler. serviceName is
// embedded into the span name as the operation prefix so the
// collector can distinguish api spans from worker spans even when
// they share a URL path. A non-empty serviceName is required; an
// empty value falls back to "kapp" so the panic-on-empty contract
// of otelhttp does not bubble up to handler code.
func TracingMiddleware(serviceName string) func(http.Handler) http.Handler {
	if serviceName == "" {
		serviceName = "kapp"
	}
	return func(next http.Handler) http.Handler {
		// Span naming is a two-phase dance because otelhttp's
		// WithSpanNameFormatter runs at span-start time — BEFORE
		// chi has matched the route. If we used r.URL.Path there
		// every concrete UUID / numeric id (e.g.
		// /api/v1/records/abc-123-def) would produce a distinct
		// span name and blow up the cardinality of the span-name
		// index in Tempo/Jaeger/Datadog. Instead we:
		//
		//   1. WithSpanNameFormatter returns a generic
		//      "HTTP <method>" placeholder at span-start time —
		//      bounded cardinality (one per HTTP verb).
		//   2. Inside the bridge, AFTER next.ServeHTTP returns,
		//      we read the chi RoutePattern (now populated by
		//      chi's routing pass) and rewrite the span name to
		//      "<method> <pattern>" — the same low-cardinality
		//      label MetricsMiddleware uses on the route_pattern
		//      attribute (see internal/platform/metrics.go).
		//
		// This is the same pattern MetricsMiddleware uses to avoid
		// unbounded /api/v1/records/{id} label values on the
		// Prometheus histogram.
		bridge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			sc := trace.SpanContextFromContext(ctx)
			if sc.IsValid() {
				// Re-derive the ctx logger so every line
				// emitted by the handler carries trace
				// attributes. The base logger was installed
				// by RequestIDMiddleware; here we extend it.
				base := LoggerFromContext(ctx)
				if base != nil {
					augmented := base.With(
						slog.String("trace_id", sc.TraceID().String()),
						slog.String("span_id", sc.SpanID().String()),
					)
					ctx = WithLogger(ctx, augmented)
					r = r.WithContext(ctx)
				}
			}

			next.ServeHTTP(w, r)

			// AFTER routing: rename the active span using the chi
			// RoutePattern so the collector sees
			// "GET /api/v1/records/{id}" instead of
			// "GET /api/v1/records/abc-123". If chi never matched a
			// route (404 / catch-all) the pattern is empty and we
			// leave the placeholder name in place; this keeps 404s
			// bucketed under one span name rather than one per
			// junk URL a scanner probes.
			span := trace.SpanFromContext(r.Context())
			if span.SpanContext().IsValid() {
				if rctx := chi.RouteContext(r.Context()); rctx != nil {
					if pattern := rctx.RoutePattern(); pattern != "" {
						span.SetName(r.Method + " " + pattern)
					}
				}
			}
		})
		return otelhttp.NewHandler(bridge, serviceName,
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				// Span-start placeholder: bounded by HTTP verb
				// count (~9), not URL cardinality. The bridge
				// above rewrites this once chi has matched.
				return "HTTP " + r.Method
			}),
		)
	}
}
