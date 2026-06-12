package manufacturing

import (
	"testing"

	"github.com/google/uuid"
)

// TestSubcontractCanTransitionTo pins the subcontract order state
// machine: the legal forward edges, idempotent self-edges, and the
// deliberately-rejected issued -> cancelled edge (stock has moved).
func TestSubcontractCanTransitionTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from string
		to   string
		want bool
	}{
		{SubcontractStatusDraft, SubcontractStatusIssued, true},
		{SubcontractStatusDraft, SubcontractStatusCancelled, true},
		{SubcontractStatusDraft, SubcontractStatusReceived, false},
		{SubcontractStatusIssued, SubcontractStatusReceived, true},
		{SubcontractStatusIssued, SubcontractStatusCancelled, false},
		{SubcontractStatusReceived, SubcontractStatusClosed, true},
		{SubcontractStatusReceived, SubcontractStatusIssued, false},
		{SubcontractStatusClosed, SubcontractStatusReceived, false},
		{SubcontractStatusCancelled, SubcontractStatusDraft, false},
		// idempotent self-edges always allowed
		{SubcontractStatusIssued, SubcontractStatusIssued, true},
		{SubcontractStatusClosed, SubcontractStatusClosed, true},
	}
	for _, tc := range cases {
		o := SubcontractOrder{Status: tc.from}
		if got := o.CanTransitionTo(tc.to); got != tc.want {
			t.Errorf("CanTransitionTo(%s -> %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestIssueSubcontractInputComponentPayload verifies the per-component
// lot/serial lookup tolerates nil maps and the zero batch UUID.
func TestIssueSubcontractInputComponentPayload(t *testing.T) {
	t.Parallel()
	item := uuid.New()
	batch := uuid.New()

	var empty IssueSubcontractInput
	if b, s := empty.componentPayload(item); b != nil || s != nil {
		t.Errorf("empty payload = (%v, %v), want (nil, nil)", b, s)
	}

	in := IssueSubcontractInput{
		ComponentBatches: map[uuid.UUID]uuid.UUID{item: batch, uuid.New(): uuid.Nil},
		ComponentSerials: map[uuid.UUID][]string{item: {"SN-1", "SN-2"}},
	}
	b, s := in.componentPayload(item)
	if b == nil || *b != batch {
		t.Errorf("batch = %v, want %v", b, batch)
	}
	if len(s) != 2 {
		t.Errorf("serials = %v, want 2 entries", s)
	}

	// A zero batch UUID must be treated as absent.
	other := uuid.New()
	in.ComponentBatches[other] = uuid.Nil
	if b, _ := in.componentPayload(other); b != nil {
		t.Errorf("zero batch should be nil, got %v", b)
	}
}
