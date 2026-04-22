package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// ApprovalCardRenderer produces KChat cards for pending approvals. The
// worker service drains `approval.requested` events from the outbox and
// uses this renderer to produce per-approver DM payloads. Each card
// includes a summary of the target record (from its KType card template)
// and Approve/Reject action buttons whose callback URLs point back at
// the kchat-bridge /kchat/commands endpoint.
//
// The renderer is deliberately dumb: it takes the pre-hydrated approval
// + record and produces a card. All DB work (looking up records,
// approvals, approvers) happens in the worker that wraps this call, so
// the renderer can be reused for composer previews and tests without
// pulling in a pool.
type ApprovalCardRenderer struct {
	registry *ktype.PGRegistry
	cards    *CardRenderer
	// actionsBase is the base URL KChat should POST decisions back to. In
	// dev this is typically http://localhost:8082; in production it is
	// the load-balancer fronting kchat-bridge. Trailing slash optional.
	actionsBase string
}

// NewApprovalCardRenderer wires the renderer with its dependencies. A
// nil registry or cards renderer yields a renderer that produces a
// minimal card (title + action buttons) instead of panicking, which is
// useful for early smoke tests.
func NewApprovalCardRenderer(registry *ktype.PGRegistry, cards *CardRenderer, actionsBase string) *ApprovalCardRenderer {
	return &ApprovalCardRenderer{registry: registry, cards: cards, actionsBase: actionsBase}
}

// RenderApprovalCard returns the card payload sent to each approver on
// the current approval step. The caller is expected to loop over
// approval.Chain.Steps[current_step].Approvers and post the same card
// to each approver's DM (or a relevant channel). A rejected approval
// still renders a card for the audit trail, but without action buttons.
func (r *ApprovalCardRenderer) RenderApprovalCard(
	ctx context.Context,
	approval *workflow.Approval,
	rec *record.KRecord,
) (Card, error) {
	if approval == nil {
		return Card{}, fmt.Errorf("approvals: approval required")
	}
	title := fmt.Sprintf("Approval requested: %s", approval.RecordKType)
	subtitle := fmt.Sprintf("State: %s · Step %d of %d",
		approval.State, approval.Chain.CurrentStep+1, len(approval.Chain.Steps))

	card := Card{Title: title, Subtitle: subtitle}

	// Best-effort record summary via the KType's card template.
	if r.cards != nil && rec != nil {
		var data map[string]any
		if err := json.Unmarshal(rec.Data, &data); err == nil {
			sub, err := r.cards.RenderCard(ctx, rec.KType, data)
			if err == nil {
				card.Body = sub.Title
				if sub.Subtitle != "" {
					card.Body += " — " + sub.Subtitle
				}
				card.Fields = append(card.Fields, sub.Fields...)
			}
		}
	}

	card.Fields = append(card.Fields,
		CardKV{Label: "Requested by", Value: approval.Chain.RequestedBy.String()},
		CardKV{Label: "Approval ID", Value: approval.ID.String()},
	)

	// Only attach decision buttons if the approval is still pending.
	// Finalized approvals render read-only cards for audit trails.
	if approval.State == workflow.ApprovalStatePending {
		base := strings.TrimRight(r.actionsBase, "/")
		if base == "" {
			base = "/kchat/commands"
		} else {
			base += "/kchat/commands"
		}
		card.Actions = []CardLink{
			{
				Label: "Approve",
				URL: fmt.Sprintf("%s?command=approve&args=%s&args=%s",
					base, approval.ID, workflow.DecisionApprove),
			},
			{
				Label: "Reject",
				URL: fmt.Sprintf("%s?command=approve&args=%s&args=%s",
					base, approval.ID, workflow.DecisionReject),
			},
		}
	}

	return card, nil
}

// RenderForApprovers fans the card out to every approver in the current
// step. It returns one Card per approver so the caller can wrap each in
// a DM envelope. This is a convenience helper — callers that need
// per-approver customization (e.g. locale) can call RenderApprovalCard
// directly.
func (r *ApprovalCardRenderer) RenderForApprovers(
	ctx context.Context,
	approval *workflow.Approval,
	rec *record.KRecord,
) (map[uuid.UUID]Card, error) {
	if approval == nil {
		return nil, fmt.Errorf("approvals: approval required")
	}
	out := make(map[uuid.UUID]Card)
	if approval.Chain.CurrentStep >= len(approval.Chain.Steps) {
		return out, nil
	}
	base, err := r.RenderApprovalCard(ctx, approval, rec)
	if err != nil {
		return nil, err
	}
	for _, approver := range approval.Chain.Steps[approval.Chain.CurrentStep].Approvers {
		out[approver] = base
	}
	return out, nil
}
