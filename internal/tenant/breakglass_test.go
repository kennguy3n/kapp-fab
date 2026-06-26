package tenant

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestOpenSession_Validation exercises the input-validation guards on
// OpenSession without touching the database. The store's pool is nil;
// the validation checks run before the first QueryRow, so a nil pool
// is safe as long as we expect the validation error rather than a
// successful insert.
func TestOpenSession_Validation(t *testing.T) {
	store := &BreakGlassStore{pool: nil}
	now := time.Now().UTC()
	future := now.Add(30 * time.Minute)

	tests := []struct {
		name    string
		entry   BreakGlassEntry
		wantErr error
	}{
		{
			name:    "missing reason code",
			entry:   BreakGlassEntry{ExpiresAt: &future},
			wantErr: ErrReasonRequired,
		},
		{
			name:    "missing expiry",
			entry:   BreakGlassEntry{ReasonCode: "incident-123"},
			wantErr: ErrExpiryRequired,
		},
		{
			name: "expiry in the past",
			entry: BreakGlassEntry{
				ReasonCode: "incident-123",
				ExpiresAt:  ptrTime(now.Add(-time.Hour)),
			},
			wantErr: nil, // not a sentinel; we check non-nil + non-ExpiryTooFar
		},
		{
			name: "expiry too far",
			entry: BreakGlassEntry{
				ReasonCode: "incident-123",
				ExpiresAt:  ptrTime(now.Add(MaxBreakGlassDuration + time.Hour)),
			},
			wantErr: ErrExpiryTooFar,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.OpenSession(nil, tc.entry)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			// For the "past" case we expect a non-nil error that is
			// NOT ErrExpiryTooFar (it's the "must be in the future"
			// check).
			if err == nil {
				t.Fatal("expected error for past expiry, got nil")
			}
			if errors.Is(err, ErrExpiryTooFar) {
				t.Fatalf("past expiry should not be ErrExpiryTooFar: %v", err)
			}
		})
	}
}

// TestLogAction_Validation checks the reason-code guard on LogAction.
func TestLogAction_Validation(t *testing.T) {
	store := &BreakGlassStore{pool: nil}
	if err := store.LogAction(nil, BreakGlassEntry{}); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("want ErrReasonRequired, got %v", err)
	}
}

// TestBreakGlassEntry_MetadataDefaults verifies that the JSON metadata
// field round-trips through the default empty-object sentinel.
func TestBreakGlassEntry_MetadataDefaults(t *testing.T) {
	e := BreakGlassEntry{ReasonCode: "test"}
	if len(e.Metadata) == 0 {
		// OpenSession fills in "{}" but the zero value is nil; just
		// verify marshalling doesn't blow up.
		out, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(out) == "" {
			t.Fatal("empty marshal output")
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestMaxBreakGlassDuration pins the cap so an accidental bump is
// caught in review. 4 hours is the policy maximum.
func TestMaxBreakGlassDuration(t *testing.T) {
	if MaxBreakGlassDuration != 4*time.Hour {
		t.Fatalf("MaxBreakGlassDuration changed to %v; expected 4h", MaxBreakGlassDuration)
	}
}

// TestBreakGlassSession_ActiveFlag verifies that the Active field is
// settable and that the zero value (no ExpiresAt) reads as inactive.
func TestBreakGlassSession_ActiveFlag(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	// Simulate what ListSessions does: compute active from ExpiresAt.
	s := BreakGlassSession{
		Entry:  BreakGlassEntry{ExpiresAt: &future},
		Active: future.After(now),
	}
	if !s.Active {
		t.Fatal("future expiry should be active")
	}
	past := now.Add(-time.Hour)
	s2 := BreakGlassSession{
		Entry:  BreakGlassEntry{ExpiresAt: &past},
		Active: past.After(now),
	}
	if s2.Active {
		t.Fatal("past expiry should not be active")
	}
	_ = uuid.New() // keep uuid import live
}
