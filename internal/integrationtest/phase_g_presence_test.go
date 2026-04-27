//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestAttendanceKChatPresenceFlag asserts the Phase G/L presence
// path:
//
//  1. The attendance_kchat_sync feature flag gates the auto-creation
//     side-effect — disabled tenants do not receive a record.
//  2. Enabling the flag and re-running the resolution path creates an
//     hr.attendance KRecord with status=present + source=kchat.
//
// The test exercises the resolution helpers directly rather than the
// HTTP handler so it doesn't have to spin up a chi router; the
// HandleHTTP wrapper is a thin JSON-decoding shim over the same
// process function.
func TestAttendanceKChatPresenceFlag(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Bring up a tenant + register the HR KTypes so attendance can
	// land. newTenantForInventory already wires what we need but
	// brings finance/inventory schemas too — fine for this test.
	tn, _, _, _, _, _ := newTenantForInventory(t, h)
	if err := hr.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register hr ktypes: %v", err)
	}

	// Seed a kapp user + tenant membership backed by a unique
	// kchat_user_id. Email matches the hr.employee record we'll
	// create so the presence handler resolves the user → employee.
	kchatID := "kchat-" + uuid.NewString()
	user, err := h.users.CreateUser(ctx, tenant.User{
		KChatUserID: kchatID,
		Email:       fmt.Sprintf("%s@example.com", kchatID),
		DisplayName: "Presence Test User",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := h.users.AddUserToTenant(ctx, user.ID, tn.ID, "tenant.admin"); err != nil {
		t.Fatalf("add user to tenant: %v", err)
	}

	// Create the matching hr.employee KRecord (email is the join
	// column the presence handler uses).
	empBody, _ := json.Marshal(map[string]any{
		"name":  "Presence Employee",
		"email": user.Email,
	})
	emp, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     hr.KTypeEmployee,
		Data:      empBody,
		CreatedBy: user.ID,
	})
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}

	features := tenant.NewFeatureStore(h.pool)
	users := tenant.NewUserStore(h.pool).WithAdminPool(h.adminPool)

	// Stand up a presence handler bound to the same stores the
	// production webhook uses. We deliberately stop short of
	// importing services/kchat-bridge (that's a main package, no
	// re-export) and instead inline the same algorithm: lookup
	// user → memberships → employee → upsert attendance.
	dispatcher := newTestPresenceDispatcher(users, features, h.records)

	// Flag off → no record created. Feature flags default to "on"
	// in this codebase (see tenant.FeatureStore.IsEnabled), so we
	// have to write an explicit enabled=false row to exercise the
	// gate.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{"attendance_kchat_sync": false}); err != nil {
		t.Fatalf("disable flag: %v", err)
	}
	if err := dispatcher.fire(ctx, kchatID); err != nil {
		t.Fatalf("fire flag-off: %v", err)
	}
	rows, err := h.records.ListByField(ctx, tn.ID, record.ListFilter{KType: hr.KTypeAttendance}, "employee_id", emp.ID.String())
	if err != nil {
		t.Fatalf("list attendance flag-off: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("flag-off: expected 0 attendance records, got %d", len(rows))
	}

	// Flag on → record gets created.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{"attendance_kchat_sync": true}); err != nil {
		t.Fatalf("enable flag: %v", err)
	}
	if err := dispatcher.fire(ctx, kchatID); err != nil {
		t.Fatalf("fire flag-on: %v", err)
	}
	rows, err = h.records.ListByField(ctx, tn.ID, record.ListFilter{KType: hr.KTypeAttendance}, "employee_id", emp.ID.String())
	if err != nil {
		t.Fatalf("list attendance flag-on: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("flag-on: expected 1 attendance record, got %d", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode attendance data: %v", err)
	}
	if data["source"] != "kchat" || data["status"] != "present" {
		t.Fatalf("unexpected attendance data: %v", data)
	}

	// Idempotency: a second fire must not duplicate the record.
	if err := dispatcher.fire(ctx, kchatID); err != nil {
		t.Fatalf("fire idempotent: %v", err)
	}
	rows, _ = h.records.ListByField(ctx, tn.ID, record.ListFilter{KType: hr.KTypeAttendance}, "employee_id", emp.ID.String())
	if len(rows) != 1 {
		t.Fatalf("idempotency: expected 1, got %d", len(rows))
	}
}
