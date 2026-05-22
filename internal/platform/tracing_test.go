package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestInitTracing_NoEndpointInstallsNoopProvider pins the off-by-
// default behaviour: when KAPP_OTEL_ENDPOINT is unset (Endpoint
// empty), InitTracing installs a no-op TracerProvider and returns a
// noop shutdown func so callers can defer the shutdown
// unconditionally without an env var check.
func TestInitTracing_NoEndpointInstallsNoopProvider(t *testing.T) {
	t.Setenv("KAPP_OTEL_ENDPOINT", "")
	cfg := LoadTracingConfig("kapp-test", "test")
	if cfg.Endpoint != "" {
		t.Fatalf("Endpoint should be empty when env unset, got %q", cfg.Endpoint)
	}

	shutdown, err := InitTracing(context.Background(), cfg)
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	tracer := otel.Tracer("kapp/test")
	_, span := tracer.Start(context.Background(), "noop-test")
	defer span.End()
	if span.SpanContext().IsValid() {
		t.Error("expected no-op span (IsValid()==false), got recording span")
	}
}

// TestInitTracing_PropagatorAlwaysInstalled pins the contract that
// even when the exporter is disabled, the W3C TraceContext +
// Baggage propagator is installed so a sampled parent context
// arriving from an upstream service still produces a child span on
// this side. This is the half of the contract that keeps multi-
// service traces intact when one hop has the exporter disabled.
func TestInitTracing_PropagatorAlwaysInstalled(t *testing.T) {
	t.Setenv("KAPP_OTEL_ENDPOINT", "")
	_, err := InitTracing(context.Background(), LoadTracingConfig("kapp-test", "test"))
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}

	prop := otel.GetTextMapPropagator()
	// The composite propagator declares both fields via Fields().
	// Order is preserved across NewCompositeTextMapPropagator calls.
	fields := prop.Fields()
	hasTraceparent := false
	hasBaggage := false
	for _, f := range fields {
		switch f {
		case "traceparent", "tracestate":
			hasTraceparent = true
		case "baggage":
			hasBaggage = true
		}
	}
	if !hasTraceparent {
		t.Errorf("propagator missing traceparent/tracestate field; got %v", fields)
	}
	if !hasBaggage {
		t.Errorf("propagator missing baggage field; got %v", fields)
	}
}

// TestParseSampleRatio_BoundsAndFallback exercises the float parser
// for KAPP_OTEL_SAMPLE_RATIO. The contract: in-range floats are
// returned as-is; out-of-range and unparseable values fall back to
// 0.1 (the default) rather than crashing the service boot.
func TestParseSampleRatio_BoundsAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want float64
	}{
		{"empty falls back", "", 0.1},
		{"zero is valid", "0", 0},
		{"one is valid", "1", 1},
		{"half is valid", "0.5", 0.5},
		{"too big falls back", "2.0", 0.1},
		{"negative falls back", "-0.5", 0.1},
		{"garbage falls back", "not-a-number", 0.1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSampleRatio(tc.in)
			if got != tc.want {
				t.Errorf("parseSampleRatio(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestTracingMiddleware_AttachesTraceIDToLogger pins the slog
// bridge: when a span is recording, the per-request logger
// installed by RequestIDMiddleware is re-derived with trace_id +
// span_id attributes so every log line emitted inside the handler
// carries the trace correlation fields.
//
// Uses an in-memory exporter so the test is deterministic and does
// not require an OTLP collector.
func TestTracingMiddleware_AttachesTraceIDToLogger(t *testing.T) {
	// Install a recording tracer provider so the span is sampled.
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTracerProvider(noop.NewTracerProvider())

	mw := TracingMiddleware("kapp-test")

	var (
		gotTraceID string
		gotSpanID  string
	)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceID = TraceIDFromContext(r.Context())
		gotSpanID = SpanIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if gotTraceID == "" {
		t.Error("expected non-empty trace_id from context inside handler")
	}
	if gotSpanID == "" {
		t.Error("expected non-empty span_id from context inside handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

// TestTracingMiddleware_SpanNameUsesChiRoutePattern pins the span-name
// cardinality fix. Without this guard, every concrete UUID / numeric id
// in a URL would produce a distinct span name and explode the cardinality
// of the span-name index in Tempo / Jaeger / Datadog. The middleware
// must rewrite the span name to the chi RoutePattern (e.g.
// "GET /api/v1/records/{id}") AFTER routing, not the concrete URL path
// (e.g. "GET /api/v1/records/abc-123").
func TestTracingMiddleware_SpanNameUsesChiRoutePattern(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTracerProvider(noop.NewTracerProvider())

	r := chi.NewRouter()
	r.Use(TracingMiddleware("kapp-test"))
	r.Get("/api/v1/records/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/records/abc-123-def-456", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("expected at least one span, got 0")
	}
	// The outermost span is the one otelhttp produced; its final
	// name should be the templated route, NOT the concrete path.
	got := spans[len(spans)-1].Name
	want := "GET /api/v1/records/{id}"
	if got != want {
		t.Errorf("span name = %q, want %q (concrete URL would explode cardinality in Tempo / Jaeger)", got, want)
	}
}

// TestTracingMiddleware_SpanName404KeepsPlaceholder pins the 404 /
// catch-all path: when chi never matches a route the RoutePattern is
// empty, and the middleware must leave the placeholder name in place
// rather than fall through to r.URL.Path (which would re-introduce the
// cardinality blow-up via attacker / scanner traffic probing junk URLs).
func TestTracingMiddleware_SpanName404KeepsPlaceholder(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTracerProvider(noop.NewTracerProvider())

	r := chi.NewRouter()
	r.Use(TracingMiddleware("kapp-test"))
	// Register one route so the chi tree compiles + middlewares wrap;
	// the test request probes a DIFFERENT path that 404s.
	r.Get("/known/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/junk/some-attacker-probe-uuid", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}

	spans := exporter.GetSpans()
	if len(spans) == 0 {
		t.Fatalf("expected at least one span, got 0")
	}
	got := spans[len(spans)-1].Name
	// Placeholder is bounded by HTTP-verb cardinality (~9).
	want := "HTTP GET"
	if got != want {
		t.Errorf("span name = %q, want %q (404 must keep placeholder to prevent attacker-driven cardinality blow-up)", got, want)
	}
}

// TestLoadTracingConfig_ReadsEnv pins the env var contract.
// Operators tune endpoint / insecure / sample ratio / version
// without touching code; the loader is the single seam.
func TestLoadTracingConfig_ReadsEnv(t *testing.T) {
	t.Setenv("KAPP_OTEL_ENDPOINT", "tempo:4317")
	t.Setenv("KAPP_OTEL_INSECURE", "false")
	t.Setenv("KAPP_OTEL_SAMPLE_RATIO", "0.5")
	t.Setenv("KAPP_OTEL_SERVICE_VERSION", "v1.2.3")

	cfg := LoadTracingConfig("kapp-api", "staging")
	if cfg.Endpoint != "tempo:4317" {
		t.Errorf("Endpoint = %q, want tempo:4317", cfg.Endpoint)
	}
	if cfg.Insecure {
		t.Error("Insecure = true, want false")
	}
	if cfg.SampleRatio != 0.5 {
		t.Errorf("SampleRatio = %v, want 0.5", cfg.SampleRatio)
	}
	if cfg.ServiceName != "kapp-api" {
		t.Errorf("ServiceName = %q, want kapp-api", cfg.ServiceName)
	}
	if cfg.ServiceVersion != "v1.2.3" {
		t.Errorf("ServiceVersion = %q, want v1.2.3", cfg.ServiceVersion)
	}
	if cfg.Environment != "staging" {
		t.Errorf("Environment = %q, want staging", cfg.Environment)
	}
}

