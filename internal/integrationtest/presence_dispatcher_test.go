//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// testPresenceDispatcher is a minimal mirror of the production
// PresenceHandler.process() routine used by phase_g_presence_test.go.
// Reproducing the logic in a test helper avoids importing the
// services/kchat-bridge main package into the test binary; the
// handler is exercised end-to-end by the bridge's own unit tests
// and this helper covers the flag/idempotency invariants the rest
// of the test suite cares about.
type testPresenceDispatcher struct {
	users    *tenant.UserStore
	features *tenant.FeatureStore
	records  *record.PGStore
}

func newTestPresenceDispatcher(users *tenant.UserStore, features *tenant.FeatureStore, records *record.PGStore) *testPresenceDispatcher {
	return &testPresenceDispatcher{users: users, features: features, records: records}
}

// fire walks every tenant the supplied kchat user belongs to and
// upserts an attendance record where the feature flag is enabled.
// Returns nil even when no work happens — the caller asserts the
// resulting record count, not the dispatcher's verdict.
func (d *testPresenceDispatcher) fire(ctx context.Context, kchatUserID string) error {
	user, err := d.users.GetUserByKChatID(ctx, kchatUserID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("presence: lookup user: %w", err)
	}
	if user.Email == "" {
		return nil
	}
	memberships, err := d.users.GetUserTenants(ctx, user.ID)
	if err != nil {
		return err
	}
	when := time.Now().UTC()
	dateKey := when.Format("2006-01-02")
	for _, m := range memberships {
		if m.Status != "active" {
			continue
		}
		on, err := d.features.IsEnabled(ctx, m.TenantID, "attendance_kchat_sync")
		if err != nil || !on {
			continue
		}
		empID, err := d.findEmployee(ctx, m.TenantID, user.Email)
		if err != nil || empID == uuid.Nil {
			continue
		}
		if _, err := d.upsert(ctx, m.TenantID, empID, user.ID, when, dateKey); err != nil {
			return err
		}
	}
	return nil
}

func (d *testPresenceDispatcher) findEmployee(ctx context.Context, tenantID uuid.UUID, email string) (uuid.UUID, error) {
	rows, err := d.records.ListByField(ctx, tenantID, record.ListFilter{KType: hr.KTypeEmployee}, "email", email)
	if err != nil {
		return uuid.Nil, err
	}
	if len(rows) == 0 {
		return uuid.Nil, nil
	}
	return rows[0].ID, nil
}

func (d *testPresenceDispatcher) upsert(ctx context.Context, tenantID, employeeID, actorID uuid.UUID, when time.Time, dateKey string) (uuid.UUID, error) {
	existing, err := d.records.ListByField(ctx, tenantID, record.ListFilter{KType: hr.KTypeAttendance}, "employee_id", employeeID.String())
	if err != nil {
		return uuid.Nil, err
	}
	for _, e := range existing {
		var data map[string]any
		if e.Data != nil {
			_ = json.Unmarshal(e.Data, &data)
		}
		if d, _ := data["date"].(string); d == dateKey {
			return e.ID, nil
		}
	}
	body, _ := json.Marshal(map[string]any{
		"employee_id": employeeID.String(),
		"date":        dateKey,
		"status":      "present",
		"source":      "kchat",
		"check_in":    when.Format(time.RFC3339),
	})
	created, err := d.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     hr.KTypeAttendance,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return created.ID, nil
}
