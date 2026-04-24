//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestSLABreachHandlerWarningAndBreach seeds a fresh tenant with one
// open helpdesk.ticket whose response_by is 60 minutes after the
// ticket's created_at, then drives the handler at three simulated
// clocks: 30m (nothing should fire), 50m (response_warning fires at
// the 48m threshold), 70m (response_breach fires once response_by
// slips into the past). The SLA log is checked after each pass so a
// regression in the idempotency guard (duplicate rows per sweep)
// trips this test immediately.
func TestSLABreachHandlerWarningAndBreach(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("sla"), Name: "SLA Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := helpdesk.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register helpdesk ktypes: %v", err)
	}

	// The ticket carries both sla_response_by (60m out) and
	// sla_resolution_by (240m out). Only response fires across the
	// timeline tested here; resolution stays quiet until well past
	// 240m so the count of log rows stays predictable.
	createdAt := time.Now().UTC().Truncate(time.Second)
	respBy := createdAt.Add(60 * time.Minute)
	resolveBy := createdAt.Add(240 * time.Minute)
	ticketData, _ := json.Marshal(map[string]any{
		"subject":           "Test ticket",
		"status":            "open",
		"priority":          "high",
		"channel":           "chat",
		"sla_policy_id":     uuid.New().String(),
		"sla_response_by":   respBy.Format(time.RFC3339Nano),
		"sla_resolution_by": resolveBy.Format(time.RFC3339Nano),
	})
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:     tn.ID,
		KType:        helpdesk.KTypeTicket,
		KTypeVersion: 1,
		Data:         ticketData,
		CreatedBy:    uuid.New(),
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	ticketID := rec.ID

	hdStore := helpdesk.NewStore(h.pool)
	clock := createdAt
	handler := helpdesk.NewSLABreachHandler(h.pool, hdStore, h.publisher, dbutil.SetTenantContext).
		WithClock(func() time.Time { return clock })

	// Pass 1 — 30 minutes in. Below the 48-minute warning threshold,
	// nothing should be logged yet.
	clock = createdAt.Add(30 * time.Minute)
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{TenantID: tn.ID}); err != nil {
		t.Fatalf("pass1 handle: %v", err)
	}
	logs, err := hdStore.ListTicketLog(ctx, tn.ID, ticketID)
	if err != nil {
		t.Fatalf("list log after pass1: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("pass1: expected 0 logs, got %d (%+v)", len(logs), logs)
	}

	// Pass 2 — 50 minutes in. Past the 48m warning threshold but
	// before the 60m breach, so exactly one response_warning row
	// should land.
	clock = createdAt.Add(50 * time.Minute)
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{TenantID: tn.ID}); err != nil {
		t.Fatalf("pass2 handle: %v", err)
	}
	// Re-run the same tick to prove idempotency — a re-dispatch must
	// not append a second warning row.
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{TenantID: tn.ID}); err != nil {
		t.Fatalf("pass2 handle (idempotent): %v", err)
	}
	logs, err = hdStore.ListTicketLog(ctx, tn.ID, ticketID)
	if err != nil {
		t.Fatalf("list log after pass2: %v", err)
	}
	if len(logs) != 1 || logs[0].EventKind != helpdesk.EventResponseWarning {
		t.Fatalf("pass2: want 1 response_warning, got %+v", logs)
	}

	// Pass 3 — 70 minutes in. response_by is now in the past, so the
	// breach event fires. The existing warning stays in place, for a
	// total of two log rows.
	clock = createdAt.Add(70 * time.Minute)
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{TenantID: tn.ID}); err != nil {
		t.Fatalf("pass3 handle: %v", err)
	}
	logs, err = hdStore.ListTicketLog(ctx, tn.ID, ticketID)
	if err != nil {
		t.Fatalf("list log after pass3: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("pass3: want 2 logs, got %d (%+v)", len(logs), logs)
	}
	// ListTicketLog orders newest first, so the breach is [0].
	if logs[0].EventKind != helpdesk.EventResponseBreach {
		t.Fatalf("pass3: want newest=response_breach, got %s", logs[0].EventKind)
	}
	if logs[1].EventKind != helpdesk.EventResponseWarning {
		t.Fatalf("pass3: want oldest=response_warning, got %s", logs[1].EventKind)
	}
}

// TestSLABreachHandlerSkipsClosedTicket ensures a resolved ticket
// does NOT fire breach events even when its sla_resolution_by is in
// the past. The status filter in loadOpenTickets is the primary
// guard; this test would catch a regression that reverses the IN
// clause or drops the status predicate entirely.
func TestSLABreachHandlerSkipsClosedTicket(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("sla-closed"), Name: "SLA Closed Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := helpdesk.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register helpdesk ktypes: %v", err)
	}

	past := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	ticketData, _ := json.Marshal(map[string]any{
		"subject":           "Already resolved",
		"status":            "resolved",
		"priority":          "medium",
		"channel":           "chat",
		"sla_response_by":   past,
		"sla_resolution_by": past,
		"resolved_at":       past,
	})
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:     tn.ID,
		KType:        helpdesk.KTypeTicket,
		KTypeVersion: 1,
		Data:         ticketData,
		CreatedBy:    uuid.New(),
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	hdStore := helpdesk.NewStore(h.pool)
	handler := helpdesk.NewSLABreachHandler(h.pool, hdStore, h.publisher, dbutil.SetTenantContext)
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{TenantID: tn.ID}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	logs, err := hdStore.ListTicketLog(ctx, tn.ID, rec.ID)
	if err != nil {
		t.Fatalf("list log: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("resolved ticket should not log breaches, got %+v", logs)
	}
}

// TestSetupWizardSeedsSLABreachAction verifies the tenant wizard
// drops a `sla_breach_check` row into scheduled_actions with the
// 5-minute cadence the task contract requires. It also doubles as
// the drift check between the duplicated literal in
// internal/tenant/wizard.go and helpdesk.ActionTypeSLABreach.
func TestSetupWizardSeedsSLABreachAction(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("wiz"), Name: "Wizard Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	wizard := tenant.NewWizard(h.pool)
	if _, err := wizard.RunSetupWizard(ctx, tn.ID, tenant.SetupWizardConfig{
		CompanyName: "Wizard Co",
	}); err != nil {
		t.Fatalf("wizard: %v", err)
	}

	// Read back inside a tenant-scoped tx so the assertion exercises
	// the production RLS path. The scheduled_actions table is RLS-
	// protected; a bare pool query under kapp_app returns zero rows
	// without the GUC set.
	var (
		foundInterval int
		foundEnabled  bool
	)
	queryErr := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(interval_seconds,0), enabled
			   FROM scheduled_actions
			  WHERE tenant_id = $1 AND action_type = $2`,
			tn.ID, helpdesk.ActionTypeSLABreach,
		).Scan(&foundInterval, &foundEnabled)
	})
	if queryErr != nil {
		t.Fatalf("read back seeded action: %v", queryErr)
	}
	if foundInterval != helpdesk.DefaultSLABreachIntervalSeconds {
		t.Fatalf("seeded interval: got %d want %d",
			foundInterval, helpdesk.DefaultSLABreachIntervalSeconds)
	}
	if !foundEnabled {
		t.Fatalf("seeded row should be enabled")
	}

	// Re-running the wizard must be idempotent — no extra row.
	if _, err := wizard.RunSetupWizard(ctx, tn.ID, tenant.SetupWizardConfig{
		CompanyName: "Wizard Co Again",
	}); err != nil {
		t.Fatalf("wizard rerun: %v", err)
	}
	var n int
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM scheduled_actions
			  WHERE tenant_id = $1 AND action_type = $2`,
			tn.ID, helpdesk.ActionTypeSLABreach,
		).Scan(&n)
	}); err != nil {
		t.Fatalf("count after rerun: %v", err)
	}
	if n != 1 {
		t.Fatalf("wizard reseeded %q (got %d rows)", helpdesk.ActionTypeSLABreach, n)
	}
}
