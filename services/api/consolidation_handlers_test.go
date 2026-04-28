package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseAsOfDecode covers the consolidation run-handler body
// parsing path. Pinned in response to two Devin Review findings:
//
//  1. The original handler used `_ = json.NewDecoder(...)` and
//     silently ran as-of-now when the body was malformed
//     (e.g. {"as_of":"not-a-date"}).
//  2. The original handler used `r.ContentLength > 0`, which is
//     false for chunked transfer-encoded clients (ContentLength
//     is -1 in that case) and silently skipped the body.
//
// parseAsOf now: returns an error on malformed JSON, tolerates an
// empty body (io.EOF), and accepts chunked / unknown-length bodies.
func TestParseAsOfDecode(t *testing.T) {
	t.Parallel()
	// reference time round-trips through JSON without timezone drift.
	ref := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		body      string
		chunked   bool
		wantErr   bool
		wantZero  bool
		wantValue *time.Time
	}{
		{name: "empty_body", body: "", wantZero: true},
		{name: "empty_object", body: "{}", wantZero: true},
		{name: "missing_field", body: `{"other":"x"}`, wantZero: true},
		{name: "valid_as_of", body: `{"as_of":"2026-04-01T12:00:00Z"}`, wantValue: &ref},
		{
			name:    "malformed_as_of_date",
			body:    `{"as_of":"not-a-date"}`,
			wantErr: true,
		},
		{
			name:    "malformed_json",
			body:    `{`,
			wantErr: true,
		},
		{
			name:      "chunked_valid_body",
			body:      `{"as_of":"2026-04-01T12:00:00Z"}`,
			chunked:   true,
			wantValue: &ref,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(tc.body))
			if tc.chunked {
				// Force a chunked stream by wrapping the body with a
				// reader that does not advertise Content-Length and
				// setting ContentLength = -1, the same shape Go's
				// net/http client uses for unknown-length writes.
				r.Body = io.NopCloser(bytes.NewReader([]byte(tc.body)))
				r.ContentLength = -1
			}
			got, err := parseAsOf(r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (got=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantZero {
				if !got.IsZero() {
					t.Fatalf("want zero time, got %v", got)
				}
				return
			}
			if tc.wantValue != nil && !got.Equal(*tc.wantValue) {
				t.Fatalf("got %v, want %v", got, *tc.wantValue)
			}
		})
	}
}
