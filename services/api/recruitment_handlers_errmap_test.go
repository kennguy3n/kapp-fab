package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// TestWriteRecruitmentErrorMappings pins the sentinel→HTTP-status
// contract for the recruitment surface so a refactor of
// writeRecruitmentError alone fails without the postgres-backed
// integration suite. The store wraps user-facing validation and
// state-machine failures with fmt.Errorf("%w: …", sentinel); the
// switch must keep mapping each wrapped chain to the right status.
func TestWriteRecruitmentErrorMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{
			name:   "not found → 404",
			err:    hr.ErrRecruitNotFound,
			status: http.StatusNotFound,
			body:   "recruitment: not found",
		},
		{
			name:   "wrapped not found → 404",
			err:    fmt.Errorf("get offer: %w", hr.ErrRecruitNotFound),
			status: http.StatusNotFound,
		},
		{
			name:   "invalid transition → 409",
			err:    hr.ErrInvalidTransition,
			status: http.StatusConflict,
		},
		{
			name:   "wrapped invalid transition → 409",
			err:    fmt.Errorf("%w: cannot send offer in status accepted", hr.ErrInvalidTransition),
			status: http.StatusConflict,
		},
		{
			name:   "opening not publishable → 409",
			err:    hr.ErrOpeningNotPublishable,
			status: http.StatusConflict,
		},
		{
			name:   "opening full → 409",
			err:    hr.ErrOpeningFull,
			status: http.StatusConflict,
		},
		{
			name:   "offer not approved → 409",
			err:    hr.ErrOfferNotApproved,
			status: http.StatusConflict,
		},
		{
			name:   "interview not scheduled → 409",
			err:    hr.ErrInterviewNotScheduled,
			status: http.StatusConflict,
		},
		{
			name:   "invalid input → 422",
			err:    hr.ErrRecruitInvalidInput,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "wrapped invalid input → 422",
			err:    fmt.Errorf("%w: rating must be 1-5", hr.ErrRecruitInvalidInput),
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "invalid status → 422",
			err:    hr.ErrInvalidStatus,
			status: http.StatusUnprocessableEntity,
		},
		{
			name:   "unrelated error → 500",
			err:    errors.New("recruitment: database connection lost"),
			status: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			writeRecruitmentError(rr, tc.err)
			if rr.Code != tc.status {
				t.Fatalf("status: want %d, got %d (body=%q)", tc.status, rr.Code, rr.Body.String())
			}
			if tc.body != "" && !strings.Contains(rr.Body.String(), tc.body) {
				t.Fatalf("body: want to contain %q, got %q", tc.body, rr.Body.String())
			}
		})
	}
}
