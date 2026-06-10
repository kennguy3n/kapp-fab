package main

import (
	"context"
	"encoding/json"
	"log"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// offerApprovalDispatcher reacts to `approval.granted` outbox events for
// hr.offer_letter records by dispatching the now-approved offer
// (draft→sent + applicant email). It closes the loop opened by
// RecruitmentStore.SendOfferLetter, which parks an offer in 'draft'
// behind a hiring-manager approval rather than sending it immediately.
//
// Delivery is best-effort and idempotent: DispatchApprovedOffer is a
// no-op when the offer is already 'sent', so duplicate event delivery
// (at-least-once on the outbox) cannot double-send. A nil store disables
// the side-effect so worker builds without recruitment wired still run.
type offerApprovalDispatcher struct {
	store *hr.RecruitmentStore
}

// approvalGrantedPayload is the subset of the approval.granted event
// payload (internal/workflow/approvals.go Decide) this dispatcher needs.
// ActorID is the approver who granted the final step; forwarding it
// attributes the offer dispatch to the hiring manager rather than the
// system actor, giving the audit trail precise attribution.
type approvalGrantedPayload struct {
	RecordKType string `json:"record_ktype"`
	RecordID    string `json:"record_id"`
	ActorID     string `json:"actor_id"`
}

// handle dispatches the approved offer when e is an approval.granted
// event targeting an hr.offer_letter. All other events are ignored.
// Failures are logged, never propagated — the offer stays in 'draft' and
// can be re-dispatched by re-granting or via the HTTP send endpoint.
func (d *offerApprovalDispatcher) handle(ctx context.Context, e events.Event) {
	if d == nil || d.store == nil {
		return
	}
	if e.Type != "approval.granted" {
		return
	}
	var p approvalGrantedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return
	}
	if p.RecordKType != hr.KTypeOfferLetter {
		return
	}
	offerID, err := uuid.Parse(p.RecordID)
	if err != nil {
		return
	}
	if _, err := d.store.DispatchApprovedOffer(ctx, e.TenantID, resolveDispatchActor(p), offerID); err != nil {
		log.Printf("worker: dispatch approved offer tenant=%s offer=%s: %v", e.TenantID, offerID, err)
	}
}

// resolveDispatchActor attributes the offer dispatch to the approver who
// granted the offer when the payload carries a parseable actor_id, and
// falls back to the system actor so a malformed/absent id never blocks
// delivery.
func resolveDispatchActor(p approvalGrantedPayload) uuid.UUID {
	if p.ActorID != "" {
		if approver, err := uuid.Parse(p.ActorID); err == nil {
			return approver
		}
	}
	return workerSystemActor
}
