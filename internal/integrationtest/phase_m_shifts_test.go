//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// TestPhaseMShiftKTypesRegister asserts the Phase M shift KTypes
// (hr.shift_type, hr.shift_assignment) round-trip through the
// registry — schemas valid, names unique, opt-in via ShiftKTypes()
// rather than baked into RegisterKTypes so existing deployments
// stay green if they don't enable shift scheduling.
func TestPhaseMShiftKTypesRegister(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, kt := range hr.ShiftKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register %s: %v", kt.Name, err)
		}
	}
	got, err := h.ktypes.Get(ctx, hr.KTypeShiftType, 1)
	if err != nil || got == nil {
		t.Fatalf("get shift_type: %v rec=%v", err, got)
	}
	got, err = h.ktypes.Get(ctx, hr.KTypeShiftAssignment, 1)
	if err != nil || got == nil {
		t.Fatalf("get shift_assignment: %v rec=%v", err, got)
	}
}

// TestPhaseMAssignShiftAgentTool exercises the Phase M
// `hr.assign_shift` agent tool against the live record store and
// verifies the persisted KRecord matches the schema (employee_id,
// shift_type_id, shift_date, status='scheduled') so the calendar
// UI's ListByField queries pick it up.
func TestPhaseMAssignShiftAgentTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, hrStore := newTenantForHR(t, h)
	for _, kt := range hr.ShiftKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register %s: %v", kt.Name, err)
		}
	}

	actor := uuid.New()

	// hr.employee + hr.shift_type seeded as KRecords.
	emp, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     hr.KTypeEmployee,
		Data:      json.RawMessage(`{"name":"Grace Hopper","email":"grace@example.com","status":"active"}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}
	st, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     hr.KTypeShiftType,
		Data:      json.RawMessage(`{"name":"Morning","start_time":"06:00","end_time":"14:00","active":true}`),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create shift type: %v", err)
	}

	engine := workflow.NewEngine(h.pool, h.publisher, h.auditor)
	executor := agents.NewExecutor(h.records, engine, h.auditor)
	agents.RegisterHRTools(executor, hrStore)

	inputs, _ := json.Marshal(map[string]any{
		"employee_id":   emp.ID.String(),
		"shift_type_id": st.ID.String(),
		"shift_date":    "2026-04-01",
		"notes":         "covering for K. Johnson",
	})
	result, err := executor.Invoke(ctx, agents.Invocation{
		TenantID:  tn.ID,
		ActorID:   actor,
		ToolName:  "hr.assign_shift",
		Inputs:    inputs,
		Mode:      agents.ModeCommit,
		Confirmed: true,
	})
	if err != nil {
		t.Fatalf("invoke assign_shift: %v", err)
	}
	if result == nil || result.Record == nil {
		t.Fatalf("assign_shift returned no record: %+v", result)
	}
	if result.Record.KType != hr.KTypeShiftAssignment {
		t.Fatalf("ktype = %q; want %q", result.Record.KType, hr.KTypeShiftAssignment)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Record.Data, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body["status"] != "scheduled" {
		t.Fatalf("status = %v; want scheduled", body["status"])
	}
	if body["shift_date"] != "2026-04-01" {
		t.Fatalf("shift_date = %v; want 2026-04-01", body["shift_date"])
	}
	if body["employee_id"] != emp.ID.String() {
		t.Fatalf("employee_id = %v; want %s", body["employee_id"], emp.ID.String())
	}

	// Dry-run mode previews without writing — the second invoke
	// must not produce a duplicate row in the store.
	preReq, _ := json.Marshal(map[string]any{
		"employee_id":   emp.ID.String(),
		"shift_type_id": st.ID.String(),
		"shift_date":    "2026-04-02",
	})
	preview, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID,
		ActorID:  actor,
		ToolName: "hr.assign_shift",
		Inputs:   preReq,
		Mode:     agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("dry-run invoke: %v", err)
	}
	if preview == nil || preview.Record != nil {
		t.Fatalf("dry-run produced a real record: %+v", preview)
	}
	if preview.Preview == nil {
		t.Fatalf("dry-run produced no preview: %+v", preview)
	}
}
