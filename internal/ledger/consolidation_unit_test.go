package ledger

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestConsolidationStoreCreateGroupValidatesInput exercises the
// pure-Go validation guards on CreateGroup so they fail fast even
// without a database. Mirrors the pattern in
// runner_unit_test.go::TestRunRawSQLValidation.
func TestConsolidationStoreCreateGroupValidatesInput(t *testing.T) {
	// nil adminPool is the same shape the real CreateGroup will
	// see when the operator hasn't configured one, so the early
	// "admin pool required" guard fires before any DB call. Using
	// a zero-value store keeps this a pure-Go test.
	s := &ConsolidationStore{}

	cases := []struct {
		name   string
		group  ConsolidationGroup
		errSub string
	}{
		{
			name:   "missing admin pool",
			group:  ConsolidationGroup{Name: "x", PresentationCurrency: "USD", MemberTenantIDs: []uuid.UUID{uuid.New()}},
			errSub: "admin pool required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateGroup(context.Background(), tc.group)
			if err == nil || !contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v; want to contain %q", err, tc.errSub)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
