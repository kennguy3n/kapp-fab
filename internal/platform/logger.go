// Structured logging glue for kapp-fab.
//
// Phase 4.1: replace the patchwork of stdlib log.Printf calls (108 at
// migration time) with a single slog-based logger that emits JSON in
// production and human-readable text in dev, attaches every log line
// to the current request_id + tenant_id automatically, and lets a
// scrape-time SRE filter by level/severity without grep gymnastics.
//
// Design choices:
//
//  1. Use the stdlib log/slog package, not a third-party logger. slog
//     reached GA in Go 1.21 and we already require 1.22 elsewhere in
//     the tree; introducing zap / zerolog would add a transitive
//     dependency surface for no functional gain.
//  2. The handler is selected from KAPP_LOG_FORMAT (json | text;
//     default text in dev, json in production-flagged builds). The
//     level is read from KAPP_LOG_LEVEL (debug | info | warn | error;
//     default info). Out-of-band values fall back to defaults rather
//     than blocking boot — this is a logging-config knob, not a
//     security gate.
//  3. The request-scoped logger lives on the request context. Callers
//     that have a context use LoggerFromContext(ctx); callers without
//     fall back to slog.Default() which is wired by InstallDefault at
//     boot. This keeps the request_id / tenant_id propagation
//     automatic and avoids the "log without context" footgun that
//     usually produces unattributable lines in incident postmortems.
//  4. log.Default's writer is *also* pointed at the slog handler via
//     SetSlogDefault, so legacy log.Printf call-sites we haven't
//     migrated yet still flow into the same exposition pipeline. The
//     downside is that they lose structured-key formatting; we accept
//     that during the gradual migration window.
package platform

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"
)

// LoggerCtxKey identifies the request-scoped *slog.Logger stored on a
// context by RequestIDMiddleware. Exported so authz / tenant / domain
// middleware can layer additional attributes on top.
type LoggerCtxKey struct{}

// RequestIDCtxKey identifies the per-request UUID stored on a context
// by RequestIDMiddleware. Exported so non-HTTP paths (NATS consumers,
// scheduler ticks) that want to honour an upstream request_id can
// store it explicitly.
type RequestIDCtxKey struct{}

// LoggerConfig is the inputs to NewLogger. Fields default safely so a
// zero-value config still produces a usable logger.
type LoggerConfig struct {
	// Format selects the slog handler. "json" emits one JSON object
	// per line (machine-parseable, expected in production). "text" or
	// any other value emits human-readable key=value lines with ANSI
	// colors when stderr is a tty.
	Format string

	// Level is the minimum severity that will be emitted. Accepted
	// values: "debug", "info", "warn", "error" (case-insensitive).
	// Unknown values default to info.
	Level string

	// Service is a free-form label written into every log line
	// (e.g. "api", "worker", "importer"). Lets a multi-service log
	// pipeline filter by origin without parsing process names.
	Service string

	// Env is a free-form deployment marker (e.g. "dev", "staging",
	// "production"). Written into every log line so an SRE looking at
	// the aggregated pipeline can quickly partition by environment.
	Env string
}

// LoadLoggerConfig builds a LoggerConfig from environment variables.
// Mirrors the env-var precedence used by LoadConfig — caller passes
// in service name + env explicitly because those are usually known at
// the call site, not at the platform layer.
func LoadLoggerConfig(service, env string) LoggerConfig {
	return LoggerConfig{
		Format:  os.Getenv("KAPP_LOG_FORMAT"),
		Level:   os.Getenv("KAPP_LOG_LEVEL"),
		Service: service,
		Env:     env,
	}
}

// NewLogger constructs a *slog.Logger from the supplied config and
// the supplied writer (stderr in production, possibly a tee writer
// in tests). The writer is parameterised so tests can capture output
// without monkey-patching os.Stderr.
func NewLogger(cfg LoggerConfig, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	level := parseLevel(cfg.Level)
	handlerOpts := &slog.HandlerOptions{
		Level: level,
		// AddSource adds {file, line, function} attrs. Useful for
		// Warn/Error in production where we want the call site
		// without a full stack trace. Skip for Debug/Info to keep
		// the per-line cost low.
		AddSource: level >= slog.LevelWarn,
	}
	var handler slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "json":
		handler = slog.NewJSONHandler(w, handlerOpts)
	default:
		handler = slog.NewTextHandler(w, handlerOpts)
	}
	attrs := []slog.Attr{}
	if cfg.Service != "" {
		attrs = append(attrs, slog.String("service", cfg.Service))
	}
	if cfg.Env != "" {
		attrs = append(attrs, slog.String("env", cfg.Env))
	}
	if len(attrs) > 0 {
		handler = handler.WithAttrs(attrs)
	}
	return slog.New(handler)
}

// InstallDefault sets the supplied logger as both slog.Default and
// the writer for the stdlib log package, so log.Printf calls from
// not-yet-migrated code still flow through the structured pipeline.
//
// The stdlib log adapter writes every log.Printf line at slog.LevelInfo
// with msg=<the line>; structured-key formatting is lost. This is the
// transitional bridge while we migrate ~108 log.Printf call sites to
// slog.Info / slog.Warn / slog.Error.
func InstallDefault(logger *slog.Logger) {
	slog.SetDefault(logger)
	log.SetFlags(0)
	log.SetOutput(&slogWriter{logger: logger})
}

// slogWriter is an io.Writer adapter that forwards each Write call to
// slog at Info level. It strips a trailing newline so the JSON output
// stays one-line-per-event.
type slogWriter struct {
	logger *slog.Logger
}

func (sw *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg == "" {
		return len(p), nil
	}
	sw.logger.Info(msg)
	return len(p), nil
}

// LoggerFromContext returns the request-scoped logger installed on ctx
// by RequestIDMiddleware, or slog.Default() if no logger is attached.
// The fallback lets non-HTTP code paths (boot, schedulers, NATS
// consumers) use the same call signature without a separate "no
// request id" branch.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return slog.Default()
	}
	if v, ok := ctx.Value(LoggerCtxKey{}).(*slog.Logger); ok && v != nil {
		return v
	}
	return slog.Default()
}

// RequestIDFromContext returns the request_id stored on ctx by
// RequestIDMiddleware, or the empty string if none is attached.
// Useful for non-log call sites (NATS publish headers, downstream
// HTTP propagation, DB pgx tracer).
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(RequestIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithLogger returns a derived context carrying the supplied logger.
// Lets middleware layer additional attributes (tenant_id, user_id,
// authz decision) on top of the request_id baseline.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, LoggerCtxKey{}, logger)
}

// WithRequestID returns a derived context carrying the supplied
// request_id. Internal callers should normally use the middleware
// which sets this automatically.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if requestID == "" {
		return ctx
	}
	return context.WithValue(ctx, RequestIDCtxKey{}, requestID)
}

// NewRequestID returns a fresh UUID-v4 request id formatted as a hex
// string. We use v4 (random) rather than v7 (time-ordered) so
// request_ids do not leak request ordering information into log
// aggregators or third-party telemetry pipelines.
func NewRequestID() string {
	return uuid.NewString()
}

// parseLevel maps a string to a slog.Level. Unknown / empty values
// default to Info.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
