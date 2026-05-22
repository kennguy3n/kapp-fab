package platform

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestIDMiddleware_GeneratesFreshID verifies that when no
// X-Request-ID header is present on the inbound request, the
// middleware mints a fresh UUID, stores it on context, and echoes it
// back in the response header.
func TestRequestIDMiddleware_GeneratesFreshID(t *testing.T) {
	var captured string
	mw := RequestIDMiddleware(slog.Default())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured == "" {
		t.Fatal("expected ctx-scoped request_id, got empty")
	}
	if got := rec.Header().Get(RequestIDHeader); got != captured {
		t.Errorf("response %s: want %q, got %q", RequestIDHeader, captured, got)
	}
	if len(captured) != 36 {
		t.Errorf("generated id should be UUID-formatted (36 chars), got %d: %q", len(captured), captured)
	}
}

// TestRequestIDMiddleware_HonoursIncomingID verifies a well-formed
// X-Request-ID header survives the middleware unchanged. End-to-end
// correlation across services depends on this contract.
func TestRequestIDMiddleware_HonoursIncomingID(t *testing.T) {
	const incoming = "my-corr-id-12345"
	var captured string
	mw := RequestIDMiddleware(slog.Default())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, incoming)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured != incoming {
		t.Errorf("want incoming id %q honoured, got %q", incoming, captured)
	}
	if got := rec.Header().Get(RequestIDHeader); got != incoming {
		t.Errorf("response echo: want %q, got %q", incoming, got)
	}
}

// TestRequestIDMiddleware_RejectsTooLongIncoming verifies that an
// over-long incoming header is replaced with a fresh id (log-poisoning
// defense).
func TestRequestIDMiddleware_RejectsTooLongIncoming(t *testing.T) {
	toolong := strings.Repeat("a", MaxIncomingRequestIDLen+1)
	var captured string
	mw := RequestIDMiddleware(slog.Default())
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, toolong)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured == toolong {
		t.Errorf("too-long incoming id should have been replaced, got original through")
	}
	if captured == "" {
		t.Errorf("expected freshly minted id when incoming rejected, got empty")
	}
}

// TestRequestIDMiddleware_RejectsNonPrintableIncoming verifies the
// sanitizer drops non-printable / non-ASCII characters (log-injection
// defense).
func TestRequestIDMiddleware_RejectsNonPrintableIncoming(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		wantNew bool
	}{
		{"control_char", "abc\x07def", true},
		{"newline_injection", "abc\ndef", true},
		{"tab", "abc\tdef", true},
		{"unicode", "abc\u00ff", true},
		{"space_only", "   ", true},
		{"empty", "", true},
		{"plain_ascii", "abc-123_def", false},
		{"chi_format", "host/1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			mw := RequestIDMiddleware(slog.Default())
			handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = RequestIDFromContext(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set(RequestIDHeader, tc.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if tc.wantNew && captured == tc.header {
				t.Errorf("expected sanitizer to reject %q, but it was honoured", tc.header)
			}
			if !tc.wantNew && captured != strings.TrimSpace(tc.header) {
				t.Errorf("expected %q honoured, got %q", tc.header, captured)
			}
		})
	}
}

// TestRequestIDMiddleware_LoggerCarriesRequestID verifies the
// ctx-scoped logger pre-tags the request_id attribute so every log
// line from the handler chain carries the correlation id without
// every caller having to remember to thread it.
func TestRequestIDMiddleware_LoggerCarriesRequestID(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(LoggerConfig{Format: "json"}, &buf)

	mw := RequestIDMiddleware(base)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LoggerFromContext(r.Context()).Info("from-handler")
	}))

	const incoming = "id-abc-123"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, incoming)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("json unmarshal: %v; raw=%q", err, buf.String())
	}
	if got["request_id"] != incoming {
		t.Errorf("logger should have pre-tagged request_id=%q; got %v", incoming, got["request_id"])
	}
	if got["msg"] != "from-handler" {
		t.Errorf("msg: want from-handler, got %v", got["msg"])
	}
}

// TestRequestIDMiddleware_NilBaseLoggerSafe verifies the middleware
// constructor does not panic when passed nil; it should fall back to
// slog.Default(). Defensive against accidental call-order issues at
// service boot.
func TestRequestIDMiddleware_NilBaseLoggerSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RequestIDMiddleware(nil) panicked: %v", r)
		}
	}()
	mw := RequestIDMiddleware(nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = LoggerFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Error("expected response request-id header even with nil base logger")
	}
}
