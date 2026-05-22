// OpenTelemetry tracing bootstrap shared by every Kapp service.
//
// The Kapp request path crosses process boundaries on hot routes:
// /api/v1/records/:id?expand=... → records handler → pgx pool, NATS
// publish for the outbox, optional helpdesk inbound webhook fan-out,
// scheduler tick, agent-tool invoke, importer body parse. The Phase 4
// request_id middleware already correlates these hops via the
// X-Request-ID header + ctx-scoped slog logger, but a request_id is
// opaque to observability tooling (Tempo / Jaeger / Datadog) which
// expects W3C TraceContext (`traceparent` header + `trace_id` /
// `span_id` fields). Phase 6C adds the OTLP/gRPC exporter and the
// otelhttp + otelpgx wrappers so a single end-to-end trace can be
// reconstructed from the published span data without losing the
// existing log correlation.
//
// Design points:
//
//   1. Disabled by default. When KAPP_OTEL_ENDPOINT is empty the
//      package installs a no-op TracerProvider so handlers can call
//      otel.Tracer(...) unconditionally without paying any wire cost.
//      Local dev (`make dev`) keeps booting with zero OTel
//      configuration.
//
//   2. Endpoint format. KAPP_OTEL_ENDPOINT accepts host:port (no
//      scheme) for the OTLP/gRPC exporter; the gRPC client adds its
//      own framing. KAPP_OTEL_INSECURE controls TLS — defaults to
//      true so a local docker-compose Tempo just works without
//      certs. Production deployments MUST set
//      KAPP_OTEL_INSECURE=0 and point at a TLS-terminated collector
//      endpoint.
//
//   3. Sampling. The default is parent-based + ratio (10%) so a
//      sampled trace honours its parent's decision (preserving
//      distributed-trace integrity across api → worker → agent-tools)
//      while bounding the wire cost for un-parented root spans.
//      KAPP_OTEL_SAMPLE_RATIO accepts a float in [0.0, 1.0]; 0.0
//      drops every root span (still useful for receiving sampled
//      parent contexts) and 1.0 samples everything (useful for
//      pre-production debugging).
//
//   4. Resource attributes. Every span carries `service.name`,
//      `service.version`, `deployment.environment` so the collector
//      can distinguish api vs worker vs importer and prod vs staging.
//      service.name defaults to the supplied serviceName arg;
//      service.version defaults to KAPP_OTEL_SERVICE_VERSION (empty
//      acceptable); deployment.environment defaults to Config.Env
//      (which itself defaults to "dev").
package platform

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracingConfig captures the operator-facing tuning knobs for the OTel
// exporter. Populated from environment via LoadTracingConfig; callers
// can also construct it programmatically (the integration test suite
// does this to inject an in-memory exporter without env-var coupling).
type TracingConfig struct {
	// Endpoint is the OTLP/gRPC collector endpoint as host:port. An
	// empty value disables the exporter entirely (InitTracing
	// returns a no-op shutdown func and installs the no-op
	// TracerProvider).
	Endpoint string

	// Insecure controls whether the gRPC client uses TLS. Default
	// true so local docker-compose Tempo / Jaeger work without
	// certs. Production deployments MUST set this to false and
	// terminate TLS at the collector.
	Insecure bool

	// SampleRatio is the head-based sampling ratio in [0.0, 1.0]
	// applied to root spans. Inherited (non-root) spans honour
	// their parent's decision via ParentBased so a distributed
	// trace is not chopped mid-flight. Default 0.1 (10%).
	SampleRatio float64

	// ServiceName identifies the service emitting spans (api,
	// worker, importer, agent-tools, kchat-bridge). Must be set;
	// LoadTracingConfig sources it from the caller (not env).
	ServiceName string

	// ServiceVersion is an optional version tag, sourced from
	// KAPP_OTEL_SERVICE_VERSION. Empty is acceptable; a future
	// release tag injected at build time will fill this in.
	ServiceVersion string

	// Environment marks the deployment ("dev", "staging",
	// "production"). Defaults to Config.Env so it picks up the
	// same operator-supplied value the structured logger uses.
	Environment string
}

// LoadTracingConfig reads operator-facing OTel env vars and returns a
// TracingConfig. serviceName must be supplied by the caller because
// only the service binary itself knows its identity; everything else
// has an env-var default.
func LoadTracingConfig(serviceName string, env string) TracingConfig {
	return TracingConfig{
		Endpoint:       getenv("KAPP_OTEL_ENDPOINT", ""),
		Insecure:       getenvBool("KAPP_OTEL_INSECURE", true),
		SampleRatio:    parseSampleRatio(getenv("KAPP_OTEL_SAMPLE_RATIO", "0.1")),
		ServiceName:    serviceName,
		ServiceVersion: getenv("KAPP_OTEL_SERVICE_VERSION", ""),
		Environment:    env,
	}
}

// parseSampleRatio reads a float in [0.0, 1.0]. Out-of-range or
// unparseable values fall back to 0.1 (the default) rather than
// returning an error — the calling service can still ship traces,
// just at the default rate. The fallback is intentional: a typo in
// KAPP_OTEL_SAMPLE_RATIO should not crash a production boot.
func parseSampleRatio(raw string) float64 {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0.1
	}
	if v < 0 || v > 1 {
		return 0.1
	}
	return v
}

// InitTracing wires the OTLP/gRPC exporter, the resource attributes,
// and the global TracerProvider. Returns a shutdown func the caller
// MUST defer so buffered spans are flushed on SIGTERM. When the
// endpoint is empty, installs a no-op provider and returns a noop
// shutdown func so callers can defer unconditionally.
//
// Why the no-op fallback instead of returning nil: every handler
// calls otel.Tracer("kapp/api").Start(ctx, ...). If we left the global
// provider unset, those calls would still succeed via the SDK's
// built-in no-op, but the global propagator would also be unset and
// otelhttp.NewHandler would fail to extract incoming traceparent
// headers. Installing a no-op provider + the W3C TraceContext
// propagator explicitly keeps both paths consistent.
func InitTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	// Always install the W3C TraceContext + Baggage propagator
	// pair, even when the exporter is disabled. Receiving a
	// sampled parent context from an upstream service must still
	// produce a child span — the local TracerProvider's no-op
	// honours parent decisions correctly only when the propagator
	// is installed.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Endpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.ServiceVersion),
		semconv.DeploymentEnvironment(cfg.Environment),
		attribute.String("kapp.tier", "core"),
	))
	if err != nil {
		return nil, fmt.Errorf("otel: build resource: %w", err)
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otel: build OTLP exporter (endpoint=%s, insecure=%t): %w", cfg.Endpoint, cfg.Insecure, err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithMaxExportBatchSize(512),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// TraceIDFromContext returns the W3C trace_id of the active span on
// the supplied context, or empty string when no span is recording.
// Used by the slog handler bridge below to attach trace_id /
// span_id attributes to every log line emitted inside a span — a
// log-trace correlation join that does not require the operator to
// parse OTel context out of slog records by hand.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanIDFromContext returns the span_id of the active span on the
// supplied context, or empty string when no span is recording. Used
// alongside TraceIDFromContext.
func SpanIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}
