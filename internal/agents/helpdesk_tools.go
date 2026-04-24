package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// RegisterHelpdeskTools attaches every Phase I helpdesk agent tool to
// an executor. Shape matches RegisterCRMTools / RegisterFinanceTools so
// services/api/main.go can line them up the same way.
func RegisterHelpdeskTools(x *Executor, store *helpdesk.Store) {
	x.Register(&createTicketTool{executor: x, helpdesk: store})
	x.Register(&assignTicketTool{executor: x})
	x.Register(&resolveTicketTool{executor: x})
}

// ----- helpdesk.create_ticket -----

type createTicketInput struct {
	Subject     string    `json:"subject"`
	Description string    `json:"description,omitempty"`
	Priority    string    `json:"priority,omitempty"`
	Channel     string    `json:"channel,omitempty"`
	CustomerID  uuid.UUID `json:"customer_id,omitempty"`
	AssignedTo  uuid.UUID `json:"assigned_to,omitempty"`
	ThreadID    string    `json:"thread_id,omitempty"`
}

type createTicketTool struct {
	executor *Executor
	helpdesk *helpdesk.Store
}

func (t *createTicketTool) Name() string               { return "helpdesk.create_ticket" }
func (t *createTicketTool) RequiresConfirmation() bool { return false }
func (t *createTicketTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in createTicketInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.Subject == "" {
		return nil, errors.New("helpdesk.create_ticket: subject required")
	}
	if in.Priority == "" {
		in.Priority = helpdesk.PriorityMedium
	}
	if in.Channel == "" {
		in.Channel = "chat"
	}

	data := map[string]any{
		"subject":  in.Subject,
		"status":   "open",
		"priority": in.Priority,
		"channel":  in.Channel,
	}
	if in.Description != "" {
		data["description"] = in.Description
	}
	if in.CustomerID != uuid.Nil {
		data["customer_id"] = in.CustomerID.String()
	}
	if in.AssignedTo != uuid.Nil {
		data["assigned_to"] = in.AssignedTo.String()
	}
	if in.ThreadID != "" {
		data["thread_id"] = in.ThreadID
	}
	// Apply the SLA policy for this priority when one exists so the
	// ticket carries response_by / resolution_by from birth.
	if t.helpdesk != nil {
		if policy, err := t.helpdesk.ResolvePolicy(ctx, inv.TenantID, in.Priority); err == nil {
			respBy, resBy := helpdesk.ComputeDueTimes(*policy, time.Now().UTC())
			data["sla_policy_id"] = policy.ID.String()
			data["sla_response_by"] = respBy.Format(time.RFC3339)
			data["sla_resolution_by"] = resBy.Format(time.RFC3339)
		}
	}

	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would create ticket %q (%s)", in.Subject, in.Priority),
			Preview: preview,
		}, nil
	}
	dataJSON, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		KType:     helpdesk.KTypeTicket,
		Data:      dataJSON,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	// Start the lifecycle workflow at `open` so subsequent
	// /helpdesk.assign_ticket + /helpdesk.resolve_ticket calls have
	// a run to transition. Best-effort; a missing workflow registration
	// is tolerated for tenants that opt out of the workflow engine.
	run, _ := t.executor.workflow.StartRun(
		ctx, inv.TenantID, helpdesk.WorkflowTicketLifecycle,
		rec.ID, "open", inv.ActorID,
	)
	return &Result{
		Summary: fmt.Sprintf("Created ticket %s (%s)", rec.ID, in.Priority),
		Record:  rec,
		Run:     run,
	}, nil
}

// ----- helpdesk.assign_ticket -----

type assignTicketInput struct {
	RecordID   uuid.UUID `json:"record_id"`
	AssignedTo uuid.UUID `json:"assigned_to"`
}

type assignTicketTool struct{ executor *Executor }

func (t *assignTicketTool) Name() string               { return "helpdesk.assign_ticket" }
func (t *assignTicketTool) RequiresConfirmation() bool { return false }
func (t *assignTicketTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in assignTicketInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RecordID == uuid.Nil || in.AssignedTo == uuid.Nil {
		return nil, errors.New("helpdesk.assign_ticket: record_id and assigned_to required")
	}

	patch := map[string]any{
		"assigned_to": in.AssignedTo.String(),
	}
	patchJSON, _ := json.Marshal(patch)

	if inv.Mode == ModeDryRun {
		return &Result{
			Summary: fmt.Sprintf("Would assign ticket %s to %s", in.RecordID, in.AssignedTo),
			Preview: patchJSON,
		}, nil
	}
	actor := inv.ActorID
	rec, err := t.executor.records.Update(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		ID:        in.RecordID,
		KType:     helpdesk.KTypeTicket,
		Data:      patchJSON,
		UpdatedBy: &actor,
	})
	if err != nil {
		return nil, err
	}
	// Advance the workflow to in_progress when it is currently at open.
	// Mirror resolveTicketTool: only overwrite `run` on success so a
	// lost-race ErrRunNotFound doesn't blank out the run we already
	// fetched for the response.
	run, _ := t.executor.workflow.GetRunByRecord(ctx, inv.TenantID, in.RecordID)
	if run != nil && run.State == "open" {
		if nextRun, terr := t.executor.workflow.Transition(ctx, inv.TenantID, run.ID, "start", inv.ActorID); terr == nil {
			run = nextRun
		} else if !errors.Is(terr, workflow.ErrRunNotFound) {
			return nil, terr
		}
	}
	return &Result{
		Summary: fmt.Sprintf("Assigned ticket %s to %s", in.RecordID, in.AssignedTo),
		Record:  rec,
		Run:     run,
	}, nil
}

// ----- helpdesk.resolve_ticket -----

type resolveTicketInput struct {
	RecordID   uuid.UUID `json:"record_id"`
	Resolution string    `json:"resolution,omitempty"`
}

type resolveTicketTool struct{ executor *Executor }

func (t *resolveTicketTool) Name() string               { return "helpdesk.resolve_ticket" }
func (t *resolveTicketTool) RequiresConfirmation() bool { return false }
func (t *resolveTicketTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in resolveTicketInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.RecordID == uuid.Nil {
		return nil, errors.New("helpdesk.resolve_ticket: record_id required")
	}
	patch := map[string]any{
		"status":      "resolved",
		"resolved_at": time.Now().UTC().Format(time.RFC3339),
	}
	if in.Resolution != "" {
		patch["resolution_notes"] = in.Resolution
	}
	patchJSON, _ := json.Marshal(patch)

	if inv.Mode == ModeDryRun {
		return &Result{
			Summary: fmt.Sprintf("Would resolve ticket %s", in.RecordID),
			Preview: patchJSON,
		}, nil
	}
	actor := inv.ActorID
	rec, err := t.executor.records.Update(ctx, record.KRecord{
		TenantID:  inv.TenantID,
		ID:        in.RecordID,
		KType:     helpdesk.KTypeTicket,
		Data:      patchJSON,
		UpdatedBy: &actor,
	})
	if err != nil {
		return nil, err
	}
	// Advance the lifecycle workflow if one exists. Swallow only the
	// benign "run vanished" / "wrong source state" cases so real DB
	// errors don't leave the ticket's `status: resolved` KRecord out
	// of sync with the workflow engine without surfacing a failure.
	run, _ := t.executor.workflow.GetRunByRecord(ctx, inv.TenantID, in.RecordID)
	if run != nil {
		if nextRun, terr := t.executor.workflow.Transition(ctx, inv.TenantID, run.ID, "resolve", inv.ActorID); terr == nil {
			run = nextRun
		} else if !errors.Is(terr, workflow.ErrRunNotFound) && !errors.Is(terr, workflow.ErrTransitionFromWrong) {
			return nil, terr
		}
	}
	return &Result{
		Summary: fmt.Sprintf("Resolved ticket %s", in.RecordID),
		Record:  rec,
		Run:     run,
	}, nil
}
