package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
)

// Approvals sentinel errors surfaced through the API / KChat bridge.
var (
	ErrApprovalNotFound      = errors.New("approvals: not found")
	ErrApprovalInvalidChain  = errors.New("approvals: invalid chain")
	ErrApprovalFinalized     = errors.New("approvals: already finalized")
	ErrApprovalNotAuthorized = errors.New("approvals: actor is not an approver for current step")
	ErrApprovalInvalidAction = errors.New("approvals: decision must be approve or reject")
	ErrApprovalDuplicate     = errors.New("approvals: actor has already decided on this step")
)

// RequestApproval opens an approval on a record. The chain's CurrentStep
// is normalized to 0 regardless of caller input so external systems
// cannot skip rungs. Emits `approval.requested` on the outbox so the
// KChat bridge can render approval cards to the step-0 approvers.
//
// Approval is decoupled from the workflow engine on purpose: a record can
// have many approvals concurrently (e.g. finance + legal) and approvals
// can exist for records that have no workflow at all. The two systems
// meet at the event bus.
func (e *Engine) RequestApproval(
	ctx context.Context,
	tenantID uuid.UUID,
	recordKType string,
	recordID uuid.UUID,
	chain ApprovalChain,
	requestedBy uuid.UUID,
) (*Approval, error) {
	if tenantID == uuid.Nil || recordKType == "" || recordID == uuid.Nil {
		return nil, errors.New("approvals: tenant, ktype, record required")
	}
	if requestedBy == uuid.Nil {
		return nil, errors.New("approvals: requested_by required")
	}
	if len(chain.Steps) == 0 {
		return nil, fmt.Errorf("%w: at least one step required", ErrApprovalInvalidChain)
	}
	for i, step := range chain.Steps {
		if len(step.Approvers) == 0 {
			return nil, fmt.Errorf("%w: step %d has no approvers", ErrApprovalInvalidChain, i)
		}
		if step.RequiredCount < 0 || step.RequiredCount > len(step.Approvers) {
			return nil, fmt.Errorf("%w: step %d required_count out of range", ErrApprovalInvalidChain, i)
		}
	}
	chain.RequestedBy = requestedBy
	chain.CurrentStep = 0
	if chain.History == nil {
		chain.History = []ApprovalAction{}
	}

	approval := &Approval{
		ID:          uuid.New(),
		TenantID:    tenantID,
		RecordKType: recordKType,
		RecordID:    recordID,
		Chain:       chain,
		State:       ApprovalStatePending,
		CreatedAt:   e.now().UTC(),
	}

	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		chainJSON, err := json.Marshal(chain)
		if err != nil {
			return fmt.Errorf("approvals: marshal chain: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals
			     (id, tenant_id, record_ktype, record_id, chain, state, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			approval.ID, approval.TenantID, approval.RecordKType, approval.RecordID,
			chainJSON, approval.State, approval.CreatedAt,
		); err != nil {
			return fmt.Errorf("approvals: insert: %w", err)
		}

		if e.events != nil {
			payload, _ := json.Marshal(map[string]any{
				"approval_id":   approval.ID,
				"record_ktype":  approval.RecordKType,
				"record_id":     approval.RecordID,
				"requested_by":  requestedBy,
				"current_step":  chain.CurrentStep,
				"step_approvers": chain.Steps[chain.CurrentStep].Approvers,
			})
			if err := e.events.EmitTx(ctx, tx, events.Event{
				TenantID: tenantID, Type: "approval.requested", Payload: payload,
			}); err != nil {
				return err
			}
		}
		if e.auditor != nil {
			after, _ := json.Marshal(map[string]any{
				"state":        approval.State,
				"current_step": chain.CurrentStep,
				"steps":        len(chain.Steps),
			})
			if err := e.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:    tenantID,
				ActorID:     &requestedBy,
				ActorKind:   audit.ActorUser,
				Action:      "approval.requested",
				TargetKType: recordKType,
				TargetID:    &recordID,
				After:       after,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return approval, nil
}

// Decide records one approver's decision. A step advances once its
// quorum is met; a single reject finalizes the whole approval as
// rejected. Callers repeat until the approval's state leaves `pending`.
func (e *Engine) Decide(
	ctx context.Context,
	tenantID uuid.UUID,
	approvalID uuid.UUID,
	decision string,
	actorID uuid.UUID,
) (*Approval, error) {
	if tenantID == uuid.Nil || approvalID == uuid.Nil {
		return nil, errors.New("approvals: tenant and approval id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("approvals: actor id required")
	}
	if decision != DecisionApprove && decision != DecisionReject {
		return nil, ErrApprovalInvalidAction
	}

	var out *Approval
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		approval, err := loadApprovalTx(ctx, tx, tenantID, approvalID)
		if err != nil {
			return err
		}
		if approval.State != ApprovalStatePending {
			return ErrApprovalFinalized
		}
		if approval.Chain.CurrentStep >= len(approval.Chain.Steps) {
			// Should not happen — state would be `approved` — but guard defensively.
			return ErrApprovalFinalized
		}
		step := approval.Chain.Steps[approval.Chain.CurrentStep]
		if !containsUUID(step.Approvers, actorID) {
			return ErrApprovalNotAuthorized
		}
		for _, prior := range approval.Chain.History {
			if prior.StepIndex == approval.Chain.CurrentStep && prior.ActorID == actorID {
				return ErrApprovalDuplicate
			}
		}

		entry := ApprovalAction{
			StepIndex: approval.Chain.CurrentStep,
			ActorID:   actorID,
			Decision:  decision,
			Timestamp: e.now().UTC(),
		}
		approval.Chain.History = append(approval.Chain.History, entry)

		finalState := ""
		var nextEvent string
		switch decision {
		case DecisionReject:
			approval.State = ApprovalStateRejected
			finalState = ApprovalStateRejected
			nextEvent = "approval.rejected"
		case DecisionApprove:
			approved := countDecisionsForStep(approval.Chain.History, approval.Chain.CurrentStep, DecisionApprove)
			required := step.RequiredCount
			if required == 0 {
				required = len(step.Approvers)
			}
			if approved >= required {
				approval.Chain.CurrentStep++
				if approval.Chain.CurrentStep >= len(approval.Chain.Steps) {
					approval.State = ApprovalStateApproved
					finalState = ApprovalStateApproved
					nextEvent = "approval.granted"
				} else {
					nextEvent = "approval.step_advanced"
				}
			} else {
				nextEvent = "approval.decision_recorded"
			}
		}

		chainJSON, _ := json.Marshal(approval.Chain)
		if _, err := tx.Exec(ctx,
			`UPDATE approvals SET chain = $1, state = $2
			 WHERE tenant_id = $3 AND id = $4`,
			chainJSON, approval.State, tenantID, approval.ID,
		); err != nil {
			return fmt.Errorf("approvals: update: %w", err)
		}

		if e.events != nil && nextEvent != "" {
			payload, _ := json.Marshal(map[string]any{
				"approval_id":  approval.ID,
				"record_ktype": approval.RecordKType,
				"record_id":    approval.RecordID,
				"actor_id":     actorID,
				"decision":     decision,
				"state":        approval.State,
				"current_step": approval.Chain.CurrentStep,
				"final_state":  finalState,
			})
			if err := e.events.EmitTx(ctx, tx, events.Event{
				TenantID: tenantID, Type: nextEvent, Payload: payload,
			}); err != nil {
				return err
			}
		}
		if e.auditor != nil {
			after, _ := json.Marshal(map[string]any{
				"state":    approval.State,
				"decision": decision,
				"step":     entry.StepIndex,
			})
			if err := e.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:    tenantID,
				ActorID:     &actorID,
				ActorKind:   audit.ActorUser,
				Action:      "approval." + decision,
				TargetKType: approval.RecordKType,
				TargetID:    &approval.RecordID,
				After:       after,
			}); err != nil {
				return err
			}
		}
		out = approval
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetApproval returns an approval by id.
func (e *Engine) GetApproval(ctx context.Context, tenantID, approvalID uuid.UUID) (*Approval, error) {
	var out *Approval
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := loadApprovalTx(ctx, tx, tenantID, approvalID)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPendingApprovals returns approvals whose current step includes the
// given approver. Used by the Approvals page in the web UI and by
// /approve `list` in KChat.
func (e *Engine) ListPendingApprovals(
	ctx context.Context,
	tenantID, approverID uuid.UUID,
) ([]Approval, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("approvals: tenant id required")
	}
	if approverID == uuid.Nil {
		return nil, errors.New("approvals: approver id required")
	}
	var out []Approval
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Postgres JSONB path: chain->'steps'->(current_step)->'approvers' contains approverID.
		// We filter in Go rather than SQL because current_step is itself stored as JSON, so the
		// dynamic path would require a lateral expression. Lists are small (pending approvals
		// per tenant scale sub-linearly with active records) so this is fine.
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, record_ktype, record_id, chain, state, created_at
			 FROM approvals
			 WHERE tenant_id = $1 AND state = $2
			 ORDER BY created_at ASC`,
			tenantID, ApprovalStatePending,
		)
		if err != nil {
			return fmt.Errorf("approvals: list: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				a         Approval
				chainJSON []byte
			)
			if err := rows.Scan(
				&a.ID, &a.TenantID, &a.RecordKType, &a.RecordID,
				&chainJSON, &a.State, &a.CreatedAt,
			); err != nil {
				return fmt.Errorf("approvals: scan: %w", err)
			}
			if err := json.Unmarshal(chainJSON, &a.Chain); err != nil {
				return fmt.Errorf("approvals: decode chain: %w", err)
			}
			if a.Chain.CurrentStep >= len(a.Chain.Steps) {
				continue
			}
			if !containsUUID(a.Chain.Steps[a.Chain.CurrentStep].Approvers, approverID) {
				continue
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- helpers -------------------------------------------------------------

func loadApprovalTx(ctx context.Context, tx pgx.Tx, tenantID, approvalID uuid.UUID) (*Approval, error) {
	var (
		a         Approval
		chainJSON []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, record_ktype, record_id, chain, state, created_at
		 FROM approvals
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, approvalID,
	).Scan(
		&a.ID, &a.TenantID, &a.RecordKType, &a.RecordID,
		&chainJSON, &a.State, &a.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrApprovalNotFound
		}
		return nil, fmt.Errorf("approvals: load: %w", err)
	}
	if err := json.Unmarshal(chainJSON, &a.Chain); err != nil {
		return nil, fmt.Errorf("approvals: decode chain: %w", err)
	}
	return &a, nil
}

func containsUUID(list []uuid.UUID, target uuid.UUID) bool {
	for _, x := range list {
		if x == target {
			return true
		}
	}
	return false
}

func countDecisionsForStep(history []ApprovalAction, step int, decision string) int {
	n := 0
	for _, a := range history {
		if a.StepIndex == step && a.Decision == decision {
			n++
		}
	}
	return n
}
