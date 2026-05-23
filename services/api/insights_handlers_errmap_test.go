package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/insights"
)

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
		name     string
		err      error
		wantBody string
	}{
		{
			name: "row_security-off",
			err: fmt.Errorf("%w: refusing to run raw SQL with row_security=%q (must be 'on')",
				insights.ErrSecurityAssertion, "off"),
			wantBody: `row_security="off"`,
		},
		{
			name: "empty-tenant-guc",
			err: fmt.Errorf("%w: refusing to run raw SQL with empty app.tenant_id GUC",
				insights.ErrSecurityAssertion),
			wantBody: "empty app.tenant_id GUC",
		},
		{
			name: "mismatched-tenant-guc",
			err: fmt.Errorf("%w: refusing to run raw SQL with mismatched app.tenant_id (got %q, want %q)",
				insights.ErrSecurityAssertion, "00000000-0000-0000-0000-000000000000", "11111111-1111-1111-1111-111111111111"),
			wantBody: "mismatched app.tenant_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, insights.ErrSecurityAssertion) {
				t.Fatalf("errors.Is(%q, ErrSecurityAssertion) = false; want true (wrap broke the sentinel chain)", tc.err.Error())
			}
			rec := httptest.NewRecorder()
			writeInsightsError(rec, tc.err)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
			}
			if got := rec.Body.String(); !strings.Contains(got, tc.wantBody) {
				t.Fatalf("body = %q; want substring %q (diagnostic message must survive the wrap)", got, tc.wantBody)
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
