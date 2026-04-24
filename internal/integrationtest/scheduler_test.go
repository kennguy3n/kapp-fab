//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/scheduler"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestSchedulerPollDueAndDispatch seeds two actions due now and one
// scheduled for the future, then runs a single poll tick and verifies
// the two due rows get dispatched to their handlers while the third
// is untouched.
func TestSchedulerPollDueAndDispatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("sched"), Name: "Scheduler Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	store := scheduler.NewStore(h.pool, h.adminPool)
	registry := scheduler.NewRegistry()

	past := time.Now().UTC().Add(-time.Minute)
	future := time.Now().UTC().Add(time.Hour)

	if _, err := store.Upsert(ctx, scheduler.ScheduledAction{
		TenantID: tn.ID, ActionType: "alpha",
		IntervalSeconds: 60,
		NextRunAt:       past,
		Enabled:         true,
		Payload:         json.RawMessage(`{"kind":"alpha"}`),
	}); err != nil {
		t.Fatalf("upsert alpha: %v", err)
	}
	if _, err := store.Upsert(ctx, scheduler.ScheduledAction{
		TenantID: tn.ID, ActionType: "beta",
		IntervalSeconds: 30,
		NextRunAt:       past,
		Enabled:         true,
		Payload:         json.RawMessage(`{"kind":"beta"}`),
	}); err != nil {
		t.Fatalf("upsert beta: %v", err)
	}
	if _, err := store.Upsert(ctx, scheduler.ScheduledAction{
		TenantID: tn.ID, ActionType: "gamma",
		IntervalSeconds: 30,
		NextRunAt:       future,
		Enabled:         true,
	}); err != nil {
		t.Fatalf("upsert gamma: %v", err)
	}

	var mu sync.Mutex
	calls := map[string]int{}
	registry.Register("alpha", scheduler.HandlerFunc(func(_ context.Context, tenantID uuid.UUID, a scheduler.ScheduledAction) error {
		mu.Lock()
		defer mu.Unlock()
		if tenantID != tn.ID {
			t.Errorf("alpha: got tenant %s want %s", tenantID, tn.ID)
		}
		calls[a.ActionType]++
		return nil
	}))
	registry.Register("beta", scheduler.HandlerFunc(func(_ context.Context, _ uuid.UUID, a scheduler.ScheduledAction) error {
		mu.Lock()
		defer mu.Unlock()
		calls[a.ActionType]++
		return nil
	}))

	// PollDue returns rows across every tenant (admin pool /
	// BYPASSRLS). Filter to the tenant we seeded so other tests'
	// leftover actions don't fail this assertion.
	allDue, err := store.PollDue(ctx, 200)
	if err != nil {
		t.Fatalf("poll due: %v", err)
	}
	var due []scheduler.ScheduledAction
	for _, a := range allDue {
		if a.TenantID == tn.ID {
			due = append(due, a)
		}
	}
	if len(due) != 2 {
		t.Fatalf("poll due: got %d rows for this tenant want 2 (total across tenants=%d)", len(due), len(allDue))
	}
	// Dispatch the claimed actions.
	for _, a := range due {
		handler, ok := registry.Lookup(a.ActionType)
		if !ok {
			t.Errorf("missing handler for %s", a.ActionType)
			continue
		}
		if err := handler.Handle(ctx, a.TenantID, a); err != nil {
			t.Errorf("handler %s: %v", a.ActionType, err)
		}
	}

	mu.Lock()
	got := map[string]int{"alpha": calls["alpha"], "beta": calls["beta"]}
	mu.Unlock()
	if got["alpha"] != 1 || got["beta"] != 1 {
		t.Fatalf("dispatch counts: %v want map[alpha:1 beta:1]", got)
	}

	// A second immediate poll must not re-claim this tenant's rows
	// because PollDue advanced next_run_at inside the transaction.
	allAgain, err := store.PollDue(ctx, 200)
	if err != nil {
		t.Fatalf("poll due 2: %v", err)
	}
	for _, a := range allAgain {
		if a.TenantID == tn.ID {
			t.Fatalf("poll due 2: tenant row %s re-claimed (tentative advance should have taken effect)", a.ID)
		}
	}
}

// TestSchedulerCronVsIntervalAdvance covers NextRun/AdvanceNextRun for
// both cadence styles without relying on a database.
func TestSchedulerCronVsIntervalAdvance(t *testing.T) {
	from := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	got, err := scheduler.NextRun(scheduler.ScheduledAction{IntervalSeconds: 300}, from)
	if err != nil {
		t.Fatalf("interval NextRun: %v", err)
	}
	if want := from.Add(5 * time.Minute); !got.Equal(want) {
		t.Fatalf("interval: got %s want %s", got, want)
	}

	// Every hour on the hour.
	got, err = scheduler.NextRun(scheduler.ScheduledAction{CronExpr: "0 * * * *"}, from)
	if err != nil {
		t.Fatalf("cron NextRun: %v", err)
	}
	if want := from.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("cron: got %s want %s", got, want)
	}

	// Neither cron nor interval → error.
	if _, err := scheduler.NextRun(scheduler.ScheduledAction{}, from); err == nil {
		t.Fatal("NextRun: expected error when no cadence set")
	}

	// Invalid cron → error.
	if _, err := scheduler.NextRun(scheduler.ScheduledAction{CronExpr: "not a cron"}, from); err == nil {
		t.Fatal("NextRun: expected error for invalid cron")
	}
}

// TestSchedulerRLSIsolation confirms the scheduled_actions table is
// tenant-isolated: tenant B never sees tenant A's rows and
// PollDue returns rows across both tenants with the admin pool.
func TestSchedulerRLSIsolation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tnA, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("schedA"), Name: "A", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("schedB"), Name: "B", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}

	store := scheduler.NewStore(h.pool, h.adminPool)
	if _, err := store.Upsert(ctx, scheduler.ScheduledAction{
		TenantID: tnA.ID, ActionType: "a_only",
		IntervalSeconds: 60,
		NextRunAt:       time.Now().UTC().Add(-time.Minute),
		Enabled:         true,
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	// Under tenant B's GUC, the row must be invisible.
	var bCount int
	if err := withTenantCount(ctx, h, tnB.ID,
		`SELECT count(*) FROM scheduled_actions`, &bCount); err != nil {
		t.Fatalf("count under B: %v", err)
	}
	if bCount != 0 {
		t.Fatalf("RLS leak: tenant B sees %d scheduled_actions rows (want 0)", bCount)
	}

	// Under tenant A's GUC, the row is visible.
	var aCount int
	if err := withTenantCount(ctx, h, tnA.ID,
		`SELECT count(*) FROM scheduled_actions`, &aCount); err != nil {
		t.Fatalf("count under A: %v", err)
	}
	if aCount != 1 {
		t.Fatalf("tenant A sees %d rows (want 1)", aCount)
	}

	// Admin-pool poll sees the row regardless of tenant.
	due, err := store.PollDue(ctx, 10)
	if err != nil {
		t.Fatalf("poll due: %v", err)
	}
	found := false
	for _, a := range due {
		if a.TenantID == tnA.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("admin PollDue did not return tenant A's row")
	}
}

// TestSchedulerUpsertValidation drives the guard rails that reject
// misconfigured scheduled actions at the Go layer — schema checks sit
// behind DB CHECK constraints, but returning a clear error from
// Upsert spares callers the round-trip.
func TestSchedulerUpsertValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("schedV"), Name: "V", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	store := scheduler.NewStore(h.pool, h.adminPool)

	cases := []struct {
		name string
		a    scheduler.ScheduledAction
	}{
		{"missing tenant", scheduler.ScheduledAction{ActionType: "x", IntervalSeconds: 60}},
		{"missing action_type", scheduler.ScheduledAction{TenantID: tn.ID, IntervalSeconds: 60}},
		{"missing cadence", scheduler.ScheduledAction{TenantID: tn.ID, ActionType: "x"}},
		{"bad cron", scheduler.ScheduledAction{TenantID: tn.ID, ActionType: "x", CronExpr: "not a cron"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.Upsert(ctx, tc.a); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// withTenantCount runs `SELECT count(*) ...` under the supplied
// tenant's GUC so RLS is enforced. Helper keeps the isolation test
// readable.
func withTenantCount(ctx context.Context, h *harness, tenantID uuid.UUID, query string, out *int) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, query).Scan(out); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// interface check — enforces that a compile error trips if someone
// removes the handler contract from the public surface.
var _ scheduler.ActionHandler = scheduler.HandlerFunc(func(context.Context, uuid.UUID, scheduler.ScheduledAction) error {
	return errors.New("x")
})
