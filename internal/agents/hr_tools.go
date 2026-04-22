package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// RegisterHRTools attaches the Phase E HR tools to an executor. A nil
// `store` is tolerated so kernel / Phase A-D integration tests that
// never apply the HR migration still pass — commit-mode calls then
// return a clear error rather than panicking.
func RegisterHRTools(x *Executor, store *hr.Store) {
	x.Register(&requestLeaveTool{executor: x})
	x.Register(&approveLeaveTool{executor: x, store: store})
}

// ----- hr.request_leave -----

type requestLeaveInput struct {
	EmployeeID uuid.UUID       `json:"employee_id"`
	LeaveType  string          `json:"leave_type"`
	FromDate   string          `json:"from_date"`
	ToDate     string          `json:"to_date"`
	Days       decimal.Decimal `json:"days,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

type requestLeaveTool struct{ executor *Executor }

func (t *requestLeaveTool) Name() string               { return "hr.request_leave" }
func (t *requestLeaveTool) RequiresConfirmation() bool { return true }
func (t *requestLeaveTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in requestLeaveInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.EmployeeID == uuid.Nil || in.LeaveType == "" || in.FromDate == "" || in.ToDate == "" {
		return nil, errors.New("hr.request_leave: employee_id, leave_type, from_date, to_date required")
	}
	data := map[string]any{
		"employee_id": in.EmployeeID,
		"leave_type":  in.LeaveType,
		"from_date":   in.FromDate,
		"to_date":     in.ToDate,
		"days":        in.Days,
		"reason":      in.Reason,
		"status":      "pending_approval",
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(data)
		return &Result{
			Summary: fmt.Sprintf("Would request %s leave %s → %s for %s", in.LeaveType, in.FromDate, in.ToDate, in.EmployeeID),
			Preview: preview,
		}, nil
	}
	if t.executor.records == nil {
		return nil, errors.New("hr.request_leave: record store not configured")
	}
	body, _ := json.Marshal(data)
	rec, err := t.executor.records.Create(ctx, record.KRecord{
		ID:        uuid.New(),
		TenantID:  inv.TenantID,
		KType:     hr.KTypeLeaveRequest,
		Data:      body,
		CreatedBy: inv.ActorID,
	})
	if err != nil {
		return nil, err
	}
	return &Result{
		Summary: fmt.Sprintf("Leave request %s submitted", rec.ID),
		Record:  rec,
	}, nil
}

// ----- hr.approve_leave -----

type approveLeaveInput struct {
	LeaveRequestID uuid.UUID       `json:"leave_request_id"`
	ApproverID     uuid.UUID       `json:"approver_id,omitempty"`
	Days           decimal.Decimal `json:"days,omitempty"`
}

type approveLeaveTool struct {
	executor *Executor
	store    *hr.Store
}

func (t *approveLeaveTool) Name() string               { return "hr.approve_leave" }
func (t *approveLeaveTool) RequiresConfirmation() bool { return true }
func (t *approveLeaveTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	var in approveLeaveInput
	if err := decodeInputs(inv, &in); err != nil {
		return nil, err
	}
	if in.LeaveRequestID == uuid.Nil {
		return nil, errors.New("hr.approve_leave: leave_request_id required")
	}
	if inv.Mode == ModeDryRun {
		preview, _ := json.Marshal(in)
		return &Result{
			Summary: fmt.Sprintf("Would approve leave request %s", in.LeaveRequestID),
			Preview: preview,
		}, nil
	}
	if t.executor.records == nil {
		return nil, errors.New("hr.approve_leave: record store not configured")
	}
	existing, err := t.executor.records.Get(ctx, inv.TenantID, in.LeaveRequestID)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(existing.Data, &body); err != nil {
		return nil, fmt.Errorf("hr.approve_leave: decode record: %w", err)
	}
	body["status"] = "approved"
	if in.ApproverID != uuid.Nil {
		body["approver_id"] = in.ApproverID
	}
	patch, _ := json.Marshal(body)
	existing.Data = patch
	existing.UpdatedBy = &inv.ActorID
	rec, err := t.executor.records.Update(ctx, *existing)
	if err != nil {
		return nil, err
	}
	var ledgerID int64
	if t.store != nil {
		days := in.Days
		if days.IsZero() {
			if d, ok := body["days"]; ok {
				if dd, err := decimal.NewFromString(fmt.Sprint(d)); err == nil {
					days = dd
				}
			}
		}
		if !days.IsZero() {
			employeeID, _ := uuid.Parse(fmt.Sprint(body["employee_id"]))
			leaveType, _ := body["leave_type"].(string)
			src := rec.ID
			entry, err := t.store.AppendLeaveLedger(ctx, hr.LeaveLedgerEntry{
				TenantID:    inv.TenantID,
				EmployeeID:  employeeID,
				LeaveType:   leaveType,
				DeltaDays:   days.Neg(),
				EffectiveOn: time.Now().UTC(),
				SourceKType: hr.KTypeLeaveRequest,
				SourceID:    &src,
				CreatedBy:   inv.ActorID,
			})
			if err != nil && !errors.Is(err, hr.ErrDuplicateLeaveSource) {
				return nil, err
			}
			if entry != nil {
				ledgerID = entry.ID
			}
		}
	}
	return &Result{
		Summary: fmt.Sprintf("Leave request %s approved", rec.ID),
		Record:  rec,
		Extra:   map[string]any{"leave_ledger_id": ledgerID},
	}, nil
}
