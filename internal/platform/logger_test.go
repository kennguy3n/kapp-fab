package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"strings"
	"testing"
)

// TestNewLogger_JSONFormat verifies the JSON handler emits one object
// per log line with the expected fixed attrs and the per-call attrs
// merged at the top level (slog's default behavior, but we want a
// guard test so a future handler swap can't silently change exposition).
func TestNewLogger_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LoggerConfig{
		Format:  "json",
		Level:   "info",
		Service: "test-svc",
		Env:     "test-env",
	}, &buf)
	logger.Info("hello", slog.String("k", "v"))

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("json unmarshal: %v; raw=%q", err, buf.String())
	}
	if got["msg"] != "hello" {
		t.Errorf("msg: want hello, got %v", got["msg"])
	}
	if got["k"] != "v" {
		t.Errorf("k: want v, got %v", got["k"])
	}
	if got["service"] != "test-svc" {
		t.Errorf("service: want test-svc, got %v", got["service"])
	}
	if got["env"] != "test-env" {
		t.Errorf("env: want test-env, got %v", got["env"])
	}
	if got["level"] != "INFO" {
		t.Errorf("level: want INFO, got %v", got["level"])
	}
}

// TestNewLogger_TextFormat verifies non-json values produce text output
// with the service/env fixed attrs.
func TestNewLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LoggerConfig{
		Format:  "text",
		Level:   "info",
		Service: "api",
		Env:     "dev",
	}, &buf)
	logger.Info("hi")
	out := buf.String()
	if !strings.Contains(out, "msg=hi") {
		t.Errorf("expected msg=hi in text output, got: %s", out)
	}
	if !strings.Contains(out, "service=api") {
		t.Errorf("expected service=api in text output, got: %s", out)
	}
}

// TestNewLogger_LevelFilter verifies a higher minimum level drops
// lower-severity events.
func TestNewLogger_LevelFilter(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LoggerConfig{Format: "json", Level: "warn"}, &buf)
	logger.Info("info-event")
	logger.Warn("warn-event")
	out := buf.String()
	if strings.Contains(out, "info-event") {
		t.Errorf("info-event should be filtered out at warn level; got: %s", out)
	}
	if !strings.Contains(out, "warn-event") {
		t.Errorf("warn-event should be emitted at warn level; got: %s", out)
	}
}

// TestNewLogger_UnknownLevelDefaultsInfo verifies typo'd KAPP_LOG_LEVEL
// values fall back to info rather than blocking boot.
func TestNewLogger_UnknownLevelDefaultsInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(LoggerConfig{Format: "json", Level: "VERBOSE"}, &buf)
	logger.Debug("debug-event")
	logger.Info("info-event")
	if strings.Contains(buf.String(), "debug-event") {
		t.Errorf("unknown level should default to info; debug should be filtered")
	}
	if !strings.Contains(buf.String(), "info-event") {
		t.Errorf("unknown level should default to info; info should be emitted")
	}
}

// TestLoggerFromContext_FallbackToDefault verifies the helper returns
// slog.Default() when no logger is attached to ctx.
func TestLoggerFromContext_FallbackToDefault(t *testing.T) {
	got := LoggerFromContext(context.Background())
	if got == nil {
		t.Fatal("LoggerFromContext(ctx-without-logger) returned nil; want slog.Default()")
	}
}

// TestLoggerFromContext_NilContext verifies the helper does not panic
// on a nil context.
func TestLoggerFromContext_NilContext(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LoggerFromContext(nil) panicked: %v", r)
		}
	}()
	got := LoggerFromContext(nil) //nolint:staticcheck // intentional nil ctx test
	if got == nil {
		t.Fatal("LoggerFromContext(nil) returned nil; want slog.Default()")
	}
}

// TestWithLogger_RoundTrip verifies that a logger placed on ctx with
// WithLogger is retrievable via LoggerFromContext.
func TestWithLogger_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	original := NewLogger(LoggerConfig{Format: "json"}, &buf)
	ctx := WithLogger(context.Background(), original)
	got := LoggerFromContext(ctx)
	got.Info("payload")
	if !strings.Contains(buf.String(), "payload") {
		t.Errorf("retrieved logger did not match placed logger; output: %s", buf.String())
	}
}

// TestRequestIDFromContext_RoundTrip verifies the request_id placed on
// ctx with WithRequestID is retrievable.
func TestRequestIDFromContext_RoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "test-id-123")
	if got := RequestIDFromContext(ctx); got != "test-id-123" {
		t.Errorf("RequestIDFromContext: want test-id-123, got %q", got)
	}
}

// TestRequestIDFromContext_Empty verifies the helper returns empty
// string when no id is attached.
func TestRequestIDFromContext_Empty(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext(ctx-without-id): want empty, got %q", got)
	}
}

// TestNewRequestID_Format verifies generated ids match the canonical
// uuid hex+dash format and are unique across calls.
func TestNewRequestID_Format(t *testing.T) {
	a := NewRequestID()
	b := NewRequestID()
	if a == b {
		t.Errorf("NewRequestID returned identical ids: %s", a)
	}
	if len(a) != 36 {
		t.Errorf("NewRequestID: want length 36, got %d (%q)", len(a), a)
	}
	if strings.Count(a, "-") != 4 {
		t.Errorf("NewRequestID: want 4 dashes, got %d (%q)", strings.Count(a, "-"), a)
	}
}

// TestInstallDefault_LegacyLogBridge verifies that after InstallDefault,
// stdlib log.Printf calls flow through the slog handler — important
// during the gradual migration window where ~108 log.Printf sites
// haven't been moved to slog yet.
func TestInstallDefault_LegacyLogBridge(t *testing.T) {
	t.Cleanup(restoreDefaults())

	var buf bytes.Buffer
	logger := NewLogger(LoggerConfig{Format: "json"}, &buf)
	InstallDefault(logger)

	// Note: we intentionally use the stdlib log package here to verify
	// the bridge — do NOT replace this with slog.Info in this test.
	stdlibLogPrintf("legacy line %d", 42)

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("json unmarshal: %v; raw=%q", err, buf.String())
	}
	if got["msg"] != "legacy line 42" {
		t.Errorf("legacy log bridge: want msg='legacy line 42', got %v", got["msg"])
	}
}

// restoreDefaults snapshots and restores both the slog default logger
// AND the stdlib log package's output writer + flags. InstallDefault
// mutates both (slog.SetDefault + log.SetOutput + log.SetFlags), so a
// test that calls InstallDefault must restore all three to avoid
// leaking the test's local *bytes.Buffer into subsequent tests in
// the same binary that exercise stdlib log.Printf. The returned
// closure should be passed to t.Cleanup.
func restoreDefaults() func() {
	prevSlog := slog.Default()
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	return func() {
		slog.SetDefault(prevSlog)
		if prevWriter != nil {
			log.SetOutput(prevWriter)
		} else {
			log.SetOutput(io.Discard)
		}
		log.SetFlags(prevFlags)
	}
}
