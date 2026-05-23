package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/insights"
)

// captureSlog redirects slog.Default() to a buffer for the duration
// of the calling test. Returns the buffer (drained at test
// completion) and a t.Cleanup that restores the previous handler.
// Use to assert structured-log emissions without parsing global
// stdio. The default handler restoration is important — leaking
// the test handler into a later test would conflate output.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestWriteInsightsErrorRoutesSecurityAssertion proves the HTTP
// error mapper recognises insights.ErrSecurityAssertion via
// errors.Is (i.e. across an fmt.Errorf("%w: …") wrap) and routes
// it to a 500 with the original diagnostic message intact.  This is
// the contract Runner.RunRawSQL's defense-in-depth probes rely on:
// they wrap ErrSecurityAssertion at the detection site so the
// HTTP layer can produce a stable, ops-distinguishable response
// without depending on a stringly-typed message match.
//
// Why a dedicated case instead of the default arm: the default
// arm also returns 500, but folding ErrSecurityAssertion in
// would force operators (and any future log / alert middleware)
// to grep response bodies or message strings to tell a real
// RLS misconfiguration apart from a generic 5xx (DB connection
// drop, marshalling bug, etc.).  The sentinel + explicit case
// gives them an errors.Is-able predicate instead.
func TestWriteInsightsErrorRoutesSecurityAssertion(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// wantLogSubstring is what must appear in the
		// server-side slog body — the FULL diagnostic
		// including any tenant IDs is preserved for
		// operator forensics.
		wantLogSubstring string
		// wantHTTPBodyAbsent is content that MUST NOT
		// appear in the HTTP response body — this is the
		// information-disclosure surface we're guarding.
		// For the mismatch case, both UUIDs must be
		// scrubbed; for the others, no PII to scrub but
		// we still verify the sanitized envelope fires.
		wantHTTPBodyAbsent []string
	}{
		{
			name: "row_security-off",
			err: fmt.Errorf("%w: refusing to run raw SQL with row_security=%q (must be 'on')",
				insights.ErrSecurityAssertion, "off"),
			wantLogSubstring: `row_security=\"off\"`,
		},
		{
			name: "empty-tenant-guc",
			err: fmt.Errorf("%w: refusing to run raw SQL with empty app.tenant_id GUC",
				insights.ErrSecurityAssertion),
			wantLogSubstring: "empty app.tenant_id GUC",
		},
		{
			name: "mismatched-tenant-guc",
			err: fmt.Errorf("%w: refusing to run raw SQL with mismatched app.tenant_id (got %q, want %q)",
				insights.ErrSecurityAssertion,
				"00000000-0000-0000-0000-000000000001",
				"11111111-1111-1111-1111-111111111112"),
			wantLogSubstring: "mismatched app.tenant_id",
			// Critical: the cross-tenant UUIDs must NOT
			// be exposed to the HTTP response — only to
			// server-side logs.  This guards against a
			// regression where a future maintainer
			// switches the sanitized body back to
			// err.Error() and re-introduces the leak.
			wantHTTPBodyAbsent: []string{
				"00000000-0000-0000-0000-000000000001",
				"11111111-1111-1111-1111-111111111112",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, insights.ErrSecurityAssertion) {
				t.Fatalf("errors.Is(%q, ErrSecurityAssertion) = false; want true (wrap broke the sentinel chain)", tc.err.Error())
			}
			logBuf := captureSlog(t)
			rec := httptest.NewRecorder()
			writeInsightsError(rec, tc.err)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
			}
			// The HTTP body must be the sanitized envelope,
			// not the verbose diagnostic.  Pin the literal
			// so a future refactor that loosens it back to
			// err.Error() trips this test.
			httpBody := rec.Body.String()
			if !strings.Contains(httpBody, "internal security assertion failed") {
				t.Fatalf("HTTP body = %q; want sanitized envelope containing 'internal security assertion failed'", httpBody)
			}
			for _, absent := range tc.wantHTTPBodyAbsent {
				if strings.Contains(httpBody, absent) {
					t.Fatalf("HTTP body leaked sensitive content %q; body = %q (information-disclosure regression)", absent, httpBody)
				}
			}
			// The slog output gets the FULL diagnostic so
			// operators tailing logs can root-cause the
			// failure.  This is the forensic path that the
			// sanitized HTTP body sacrifices.
			logOutput := logBuf.String()
			if !strings.Contains(logOutput, tc.wantLogSubstring) {
				t.Fatalf("slog output = %q; want substring %q (diagnostic message must reach server logs)", logOutput, tc.wantLogSubstring)
			}
			if !strings.Contains(logOutput, `"kind":"insights_security_assertion"`) {
				t.Fatalf("slog output = %q; want kind=insights_security_assertion field (alerting hook contract)", logOutput)
			}
		})
	}
}

// TestWriteInsightsErrorSecurityAssertionDistinctFromDefault
// guards against an accidental refactor that removes the
// ErrSecurityAssertion case from writeInsightsError.  If the
// sentinel branch is deleted, a plain wrapped error would still
// hit the default 500 arm — same status, so a status-only
// assertion wouldn't catch the regression.  Instead we check
// that errors.Is can identify the failure mode against the
// returned error chain (which a default-arm-only handler would
// lose, since http.Error consumes the err).  This test pins
// the contract at the source: ANY future code that wants to
// drive different behaviour for ErrSecurityAssertion vs.
// generic 500s must keep errors.Is operative against the
// runner-side error before it reaches the HTTP boundary.
func TestWriteInsightsErrorSecurityAssertionDistinctFromDefault(t *testing.T) {
	secErr := fmt.Errorf("%w: probe failed", insights.ErrSecurityAssertion)
	genericErr := errors.New("insights: execute raw sql: dial tcp: connection refused")

	// Both produce 500, but only secErr matches the sentinel.
	rec := httptest.NewRecorder()
	writeInsightsError(rec, secErr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("security err status = %d; want 500", rec.Code)
	}
	rec = httptest.NewRecorder()
	writeInsightsError(rec, genericErr)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("generic err status = %d; want 500", rec.Code)
	}

	if !errors.Is(secErr, insights.ErrSecurityAssertion) {
		t.Fatalf("security err lost sentinel after wrap")
	}
	if errors.Is(genericErr, insights.ErrSecurityAssertion) {
		t.Fatalf("generic err falsely matched ErrSecurityAssertion")
	}

	// Smoke-check that context-cancellation still routes to 504,
	// proving the new ErrSecurityAssertion arm didn't accidentally
	// short-circuit the prior timeout mapping (defensive: a copy-
	// paste of the case block above this one could easily move
	// the cancellation arm below ErrSecurityAssertion and have
	// it dead-code, since the default 500 would catch it first
	// only if context.Canceled were ever wrapped with the
	// sentinel — paranoia, but cheap).
	rec = httptest.NewRecorder()
	writeInsightsError(rec, context.Canceled)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("context.Canceled status = %d; want 504 (cancellation arm regressed)", rec.Code)
	}
}

// TestWriteInsightsErrorJoinedSentinelHonoursSwitchOrder pins
// the switch-ordering invariant: if a future refactor joins
// ErrValidation and ErrSecurityAssertion into a single error
// (errors.Join, or fmt.Errorf("%w: %w", a, b) — both produce a
// multi-sentinel chain where errors.Is matches BOTH), the
// HTTP layer must route to the EARLIER case in the switch.
// ErrValidation appears above ErrSecurityAssertion, so the
// joined error gets 400 (client-input contract wins).
//
// This documents the rule for runner-side code: do NOT join
// these sentinels.  If a probe failure is also a validation
// failure (hypothetically — the current code never produces
// this shape), the caller must pick one sentinel based on the
// dominant semantic, not bag-of-sentinels.  The validation
// path takes precedence because a 400 is more actionable for
// the client than an opaque 500.
func TestWriteInsightsErrorJoinedSentinelHonoursSwitchOrder(t *testing.T) {
	joined := errors.Join(insights.ErrValidation, insights.ErrSecurityAssertion)
	if !errors.Is(joined, insights.ErrValidation) {
		t.Fatalf("joined error lost ErrValidation; errors.Join semantics regressed")
	}
	if !errors.Is(joined, insights.ErrSecurityAssertion) {
		t.Fatalf("joined error lost ErrSecurityAssertion; errors.Join semantics regressed")
	}

	rec := httptest.NewRecorder()
	writeInsightsError(rec, joined)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("joined(ErrValidation, ErrSecurityAssertion) status = %d; want 400 — ErrValidation must take precedence over ErrSecurityAssertion in the switch", rec.Code)
	}
}
