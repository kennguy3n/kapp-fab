package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// TestOfferApprovalDispatcherGuards verifies the cheap guard clauses on
// the deliver-loop hot path: a nil dispatcher, a nil store, a
// non-approval event, an approval for a different record type, and a
// malformed record_id must all short-circuit WITHOUT touching the store
// (which would dereference a nil pool and panic). Only an
// approval.granted event for an hr.offer_letter with a valid UUID falls
// through to DispatchApprovedOffer.
func TestOfferApprovalDispatcherGuards(t *testing.T) {
	t.Parallel()

	mustPayload := func(m map[string]any) json.RawMessage {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return b
	}

	// A store whose pool is nil: constructing is safe, but any method
	// call would panic — so reaching it in a guard test is itself the
	// failure signal.
	storeWithNilPool := hr.NewRecruitmentStore(nil, nil, nil, nil, nil)

	cases := []struct {
		name string
		d    *offerApprovalDispatcher
		e    events.Event
	}{
		{
			name: "nil dispatcher",
			d:    nil,
			e:    events.Event{Type: "approval.granted"},
		},
		{
			name: "nil store",
			d:    &offerApprovalDispatcher{store: nil},
			e:    events.Event{Type: "approval.granted"},
		},
		{
			name: "non-approval event",
			d:    &offerApprovalDispatcher{store: storeWithNilPool},
			e:    events.Event{Type: "hr.offer_letter.sent", TenantID: uuid.New()},
		},
		{
			name: "approval for different record type",
			d:    &offerApprovalDispatcher{store: storeWithNilPool},
			e: events.Event{
				Type:     "approval.granted",
				TenantID: uuid.New(),
				Payload:  mustPayload(map[string]any{"record_ktype": "purchasing.purchase_order", "record_id": uuid.New().String()}),
			},
		},
		{
			name: "malformed record id",
			d:    &offerApprovalDispatcher{store: storeWithNilPool},
			e: events.Event{
				Type:     "approval.granted",
				TenantID: uuid.New(),
				Payload:  mustPayload(map[string]any{"record_ktype": hr.KTypeOfferLetter, "record_id": "not-a-uuid"}),
			},
		},
		{
			name: "empty payload",
			d:    &offerApprovalDispatcher{store: storeWithNilPool},
			e:    events.Event{Type: "approval.granted", TenantID: uuid.New()},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// Must not panic and must return promptly. If a guard were
			// missing, the nil-pool store would panic here.
			c.d.handle(context.Background(), c.e)
		})
	}
}

// TestResolveDispatchActor verifies the offer dispatch is attributed to
// the approver from the payload when present and parseable, and falls
// back to the system actor for absent or malformed actor ids.
func TestResolveDispatchActor(t *testing.T) {
	t.Parallel()

	approver := uuid.New()
	cases := []struct {
		name    string
		actorID string
		want    uuid.UUID
	}{
		{"valid approver", approver.String(), approver},
		{"absent actor_id", "", workerSystemActor},
		{"malformed actor_id", "not-a-uuid", workerSystemActor},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := resolveDispatchActor(approvalGrantedPayload{ActorID: c.actorID})
			if got != c.want {
				t.Errorf("resolveDispatchActor(%q) = %s, want %s", c.actorID, got, c.want)
			}
		})
	}
}
