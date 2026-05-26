package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/manufacturing"
)

// TestWriteManufacturingErrorMapsInvalidInputTo422 pins the
// contract that every error wrapping manufacturing.ErrInvalidInput
// surfaces as HTTP 422 Unprocessable Entity, not 500. The store
// layer wraps its user-supplied validation failures (negative
// actual_qty, invalid bom status, missing item_id, etc.) with
// fmt.Errorf("%w: …", ErrInvalidInput); without an errors.Is arm
// in writeManufacturingError those would fall through to the
// default 500 and surface as a generic "Internal Server Error" to
// the API caller — an undeserved scary status for a routine data-
// entry typo. The test enumerates representative wrapped variants
// that are minted across the manufacturing/store + work_order
// packages so any future regression in the mapping arm is
// caught at build time.
//
// The integration tests exercise the call sites end-to-end against
// a real database; this unit test pins just the writeManufacturingError
// switch so a refactor of that function alone (e.g. someone deletes
// the ErrInvalidInput case while doing an unrelated cleanup) fails
// in CI without needing the postgres-backed integration suite.
func TestWriteManufacturingErrorMapsInvalidInputTo422(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "bare ErrInvalidInput",
			err:  manufacturing.ErrInvalidInput,
		},
		{
			name: "wrapped: invalid bom status",
			err:  fmt.Errorf("%w: invalid bom status %q", manufacturing.ErrInvalidInput, "foo"),
		},
		{
			name: "wrapped: negative actual_qty",
			err:  fmt.Errorf("%w: actual_qty must be >= 0", manufacturing.ErrInvalidInput),
		},
		{
			name: "wrapped: over-yield actual_qty",
			err:  fmt.Errorf("%w: actual_qty 100 exceeds 110%% of planned 50", manufacturing.ErrInvalidInput),
		},
		{
			name: "wrapped: missing item_id",
			err:  fmt.Errorf("%w: item_id required", manufacturing.ErrInvalidInput),
		},
		{
			name: "wrapped: zero planned_qty",
			err:  fmt.Errorf("%w: planned_qty must be > 0", manufacturing.ErrInvalidInput),
		},
		{
			// CreateBOM previously coerced a non-positive
			// output_qty to 1 silently. It now returns
			// ErrInvalidInput so the caller gets a 422 with
			// a self-describing body rather than a BOM whose
			// consumption math (planned_qty / output_qty) is
			// silently wrong. Pin the mapping arm here so
			// nobody re-introduces the silent-coerce branch.
			name: "wrapped: zero output_qty",
			err:  fmt.Errorf("%w: output_qty must be > 0", manufacturing.ErrInvalidInput),
		},
		{
			name: "double-wrapped through fmt.Errorf chain",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("%w: zero qty", manufacturing.ErrInvalidInput)),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !errors.Is(tc.err, manufacturing.ErrInvalidInput) {
				t.Fatalf("test setup: errors.Is must see ErrInvalidInput in chain, got %v", tc.err)
			}
			rr := httptest.NewRecorder()
			writeManufacturingError(rr, tc.err)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Errorf("status: want 422, got %d (body=%q)", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "manufacturing: invalid input") {
				t.Errorf("body: want to contain wrapped sentinel text, got %q", rr.Body.String())
			}
		})
	}
}

// TestWriteManufacturingErrorPreservesExistingMappings makes sure
// the new ErrInvalidInput arm did not accidentally swallow any of
// the previously-mapped sentinels. Each sentinel below was wired
// to its current status code before this refactor and must stay
// there.
func TestWriteManufacturingErrorPreservesExistingMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"ErrBOMNotFound", manufacturing.ErrBOMNotFound, http.StatusNotFound},
		{"ErrWorkOrderNotFound", manufacturing.ErrWorkOrderNotFound, http.StatusNotFound},
		{"ErrBOMNotActive", manufacturing.ErrBOMNotActive, http.StatusUnprocessableEntity},
		{"ErrBOMHasNoComponents", manufacturing.ErrBOMHasNoComponents, http.StatusUnprocessableEntity},
		{"ErrBOMSelfReference", manufacturing.ErrBOMSelfReference, http.StatusUnprocessableEntity},
		{"ErrBOMDuplicateComponent", manufacturing.ErrBOMDuplicateComponent, http.StatusUnprocessableEntity},
		{"ErrBOMInvalidTransition", manufacturing.ErrBOMInvalidTransition, http.StatusUnprocessableEntity},
		{"ErrWorkOrderInvalidTransition", manufacturing.ErrWorkOrderInvalidTransition, http.StatusUnprocessableEntity},
		{"ErrWorkOrderInsufficientStock", manufacturing.ErrWorkOrderInsufficientStock, http.StatusUnprocessableEntity},
		{"unrelated error", errors.New("manufacturing: database connection lost"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			writeManufacturingError(rr, tc.err)
			if rr.Code != tc.status {
				t.Errorf("status: want %d, got %d (body=%q)", tc.status, rr.Code, rr.Body.String())
			}
		})
	}
}
