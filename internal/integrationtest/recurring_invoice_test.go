//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// TestRecurringInvoiceEngineGeneratesAndAdvances exercises the
// recurring AR invoice generator end-to-end:
//
//  1. Seed a posted-or-not template invoice.
//  2. Author a finance.recurring_invoice row whose
//     next_generation_date is "today" (driven by the engine clock).
//  3. Drive the engine once — verify an ar_invoice draft was
//     created, the recurring row's cursor advanced one frequency
//     step, and last_generated_invoice_id points at the new draft.
//  4. Drive the engine again at the same fake time — no new invoices
//     fire (the cursor moved past today).
//  5. Advance the clock past the new cursor — a second invoice
//     materialises.
//
// Auto-post is left false so the test stays focused on the cloning
// + cursor logic; auto-post is exercised by a separate test below.
func TestRecurringInvoiceEngineGeneratesAndAdvances(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn, _, _ := newTenantForFinance(t, h)
	actor := uuid.New()

	templateID := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-TEMPLATE", uuid.NewString(),
		decimal.NewFromInt(1000), decimal.NewFromInt(80), "2200",
	)

	day := func(s string) time.Time {
		t.Helper()
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return d.UTC()
	}
	clock := day("2026-03-01")

	recurringID := createRecurringInvoiceRecord(t, h, tn.ID, actor, templateRow{
		Name:               "Monthly retainer",
		TemplateID:         templateID,
		Frequency:          finance.FrequencyMonthly,
		StartDate:          "2026-03-01",
		NextGenerationDate: "2026-03-01",
		AutoPost:           false,
	})

	engine := finance.NewRecurringEngine(h.records, nil).
		WithClock(func() time.Time { return clock }).
		WithSystemActor(actor)

	// First sweep at clock=2026-03-01: should fire.
	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: finance.ActionTypeRecurringInvoice,
	}); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	invoices := listInvoiceDrafts(t, h, tn.ID)
	if len(invoices) != 2 { // 1 template + 1 generated
		t.Fatalf("after first sweep: want 2 invoice records, got %d", len(invoices))
	}
	rec := getRecurring(t, h, tn.ID, recurringID)
	if got := readString(t, rec.Data, "next_generation_date"); got != "2026-04-01" {
		t.Fatalf("next_generation_date after first sweep: got %s want 2026-04-01", got)
	}
	if readString(t, rec.Data, "last_generated_invoice_id") == "" {
		t.Fatalf("last_generated_invoice_id not set after first sweep")
	}

	// Second sweep at the same clock: no new draft should appear
	// because the cursor is now in the future.
	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: finance.ActionTypeRecurringInvoice,
	}); err != nil {
		t.Fatalf("handle 2: %v", err)
	}
	if got := len(listInvoiceDrafts(t, h, tn.ID)); got != 2 {
		t.Fatalf("idempotent sweep: want 2 invoice records, got %d", got)
	}

	// Advance clock to 2026-04-01 — second invoice should fire.
	clock = day("2026-04-01")
	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: finance.ActionTypeRecurringInvoice,
	}); err != nil {
		t.Fatalf("handle 3: %v", err)
	}
	if got := len(listInvoiceDrafts(t, h, tn.ID)); got != 3 {
		t.Fatalf("after advance: want 3 invoice records, got %d", got)
	}
	rec = getRecurring(t, h, tn.ID, recurringID)
	if got := readString(t, rec.Data, "next_generation_date"); got != "2026-05-01" {
		t.Fatalf("next_generation_date after second fire: got %s want 2026-05-01", got)
	}
}

// TestRecurringInvoiceEngineAnchorsCadenceAndBackfills is the
// regression guard for the "AdvanceDate(today, ...) drifts the
// cadence and collapses missed periods" bug. Two behaviours are
// asserted in one run:
//
//  1. Cadence drift: when the sweeper fires one day late, the
//     advanced cursor must stay anchored to the original day-of-
//     month (2026-04-01), not slide to 2026-04-02.
//  2. Multi-period catch-up: when the sweeper fires three months
//     late with no intervening runs, the engine must emit one
//     invoice per missed period (Jan/Feb/Mar/Apr = 4 invoices), not
//     a single one, and leave the cursor at 2026-05-01.
func TestRecurringInvoiceEngineAnchorsCadenceAndBackfills(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _ := newTenantForFinance(t, h)
	actor := uuid.New()

	day := func(s string) time.Time {
		t.Helper()
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return d.UTC()
	}

	// --- Case 1: late-by-one-day sweep must not drift the cadence.
	templateID := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-ANCHOR", uuid.NewString(),
		decimal.NewFromInt(1000), decimal.Zero, "",
	)
	clock := day("2026-03-02") // one day late
	recurringID := createRecurringInvoiceRecord(t, h, tn.ID, actor, templateRow{
		Name:               "Late anchor",
		TemplateID:         templateID,
		Frequency:          finance.FrequencyMonthly,
		StartDate:          "2026-03-01",
		NextGenerationDate: "2026-03-01",
		AutoPost:           false,
	})

	engine := finance.NewRecurringEngine(h.records, nil).
		WithClock(func() time.Time { return clock })
	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: finance.ActionTypeRecurringInvoice,
	}); err != nil {
		t.Fatalf("case1 handle: %v", err)
	}
	rec := getRecurring(t, h, tn.ID, recurringID)
	if got := readString(t, rec.Data, "next_generation_date"); got != "2026-04-01" {
		t.Fatalf("case1: cadence drifted to %s, want anchored 2026-04-01", got)
	}
	// Pause the case1 recurring so it does not re-fire during
	// case2 and inflate the back-fill count.
	pauseRecurring(t, h, tn.ID, recurringID)

	// --- Case 2: three-month gap must back-fill, not squash.
	templateID2 := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-CATCHUP", uuid.NewString(),
		decimal.NewFromInt(1000), decimal.Zero, "",
	)
	recurring2ID := createRecurringInvoiceRecord(t, h, tn.ID, actor, templateRow{
		Name:               "Back-fill retainer",
		TemplateID:         templateID2,
		Frequency:          finance.FrequencyMonthly,
		StartDate:          "2026-01-01",
		NextGenerationDate: "2026-01-01",
		AutoPost:           false,
	})
	before := len(listInvoiceDrafts(t, h, tn.ID))
	clock = day("2026-04-15") // 3 full missed periods + current
	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{
		ActionType: finance.ActionTypeRecurringInvoice,
	}); err != nil {
		t.Fatalf("case2 handle: %v", err)
	}
	after := len(listInvoiceDrafts(t, h, tn.ID))
	// Jan/Feb/Mar/Apr → 4 new invoice drafts from recurring2.
	if added := after - before; added != 4 {
		t.Fatalf("case2: back-fill want +4 invoices, got +%d", added)
	}
	rec = getRecurring(t, h, tn.ID, recurring2ID)
	if got := readString(t, rec.Data, "next_generation_date"); got != "2026-05-01" {
		t.Fatalf("case2: next_generation_date after back-fill: got %s want 2026-05-01", got)
	}
}

// TestRecurringInvoiceEngineCompletesPastEndDate verifies a row
// whose end_date has elapsed flips to status="completed" rather than
// continuing to fire forever.
func TestRecurringInvoiceEngineCompletesPastEndDate(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn, _, _ := newTenantForFinance(t, h)
	actor := uuid.New()
	templateID := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-TEMPLATE-END", uuid.NewString(),
		decimal.NewFromInt(500), decimal.Zero, "",
	)

	clock := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	recurringID := createRecurringInvoiceRecord(t, h, tn.ID, actor, templateRow{
		Name:               "Past-end retainer",
		TemplateID:         templateID,
		Frequency:          finance.FrequencyMonthly,
		StartDate:          "2026-01-01",
		NextGenerationDate: "2026-05-01",
		EndDate:            "2026-04-30",
		AutoPost:           false,
	})

	engine := finance.NewRecurringEngine(h.records, nil).
		WithClock(func() time.Time { return clock })
	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	rec := getRecurring(t, h, tn.ID, recurringID)
	if got := readString(t, rec.Data, "status"); got != finance.RecurringStatusCompleted {
		t.Fatalf("status: got %s want completed", got)
	}
	if got := len(listInvoiceDrafts(t, h, tn.ID)); got != 1 { // template only
		t.Fatalf("post-end sweep should not generate; got %d invoice records", got)
	}
}

// TestRecurringInvoiceEngineAutoPosts wires a stub poster and
// verifies it fires for auto_post=true. The poster is a stub so
// the test does not depend on the full ledger postability of the
// template — that path is already covered by phase_c_test.go.
func TestRecurringInvoiceEngineAutoPosts(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn, _, _ := newTenantForFinance(t, h)
	actor := uuid.New()
	templateID := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-TEMPLATE-AUTO", uuid.NewString(),
		decimal.NewFromInt(750), decimal.Zero, "",
	)

	clock := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	createRecurringInvoiceRecord(t, h, tn.ID, actor, templateRow{
		Name:               "Auto-post weekly",
		TemplateID:         templateID,
		Frequency:          finance.FrequencyWeekly,
		StartDate:          "2026-07-15",
		NextGenerationDate: "2026-07-15",
		AutoPost:           true,
	})

	var posted []uuid.UUID
	stubPoster := func(_ context.Context, tenantID, invoiceID, _ uuid.UUID) error {
		if tenantID != tn.ID {
			t.Errorf("poster: tenant mismatch %s != %s", tenantID, tn.ID)
		}
		posted = append(posted, invoiceID)
		return nil
	}

	engine := finance.NewRecurringEngine(h.records, stubPoster).
		WithClock(func() time.Time { return clock })

	if err := engine.Handle(ctx, tn.ID, scheduler.ScheduledAction{}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(posted) != 1 {
		t.Fatalf("expected 1 auto-post; got %d", len(posted))
	}
	// Posted ID should match the freshly-cloned draft.
	drafts := listInvoiceDrafts(t, h, tn.ID)
	if len(drafts) != 2 {
		t.Fatalf("expected template + 1 generated; got %d", len(drafts))
	}
	found := false
	for _, d := range drafts {
		if d.ID == posted[0] {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto-post called for unknown invoice id %s", posted[0])
	}
}

// templateRow is the small struct createRecurringInvoiceRecord
// accepts. Defined locally so the tests stay readable.
type templateRow struct {
	Name               string
	TemplateID         uuid.UUID
	Frequency          string
	StartDate          string
	EndDate            string
	NextGenerationDate string
	AutoPost           bool
}

func createRecurringInvoiceRecord(t *testing.T, h *harness, tenantID, actorID uuid.UUID, in templateRow) uuid.UUID {
	t.Helper()
	data := map[string]any{
		"name":                 in.Name,
		"template_invoice_id":  in.TemplateID.String(),
		"frequency":            in.Frequency,
		"start_date":           in.StartDate,
		"next_generation_date": in.NextGenerationDate,
		"auto_post":            in.AutoPost,
		"status":               finance.RecurringStatusActive,
	}
	if in.EndDate != "" {
		data["end_date"] = in.EndDate
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal recurring: %v", err)
	}
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypeRecurringInvoice,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create recurring: %v", err)
	}
	return rec.ID
}

// listInvoiceDrafts pulls every finance.ar_invoice for the tenant via
// the record store. Used to count materialised invoices without
// caring about status.
func listInvoiceDrafts(t *testing.T, h *harness, tenantID uuid.UUID) []record.KRecord {
	t.Helper()
	rows, err := h.records.List(context.Background(), tenantID, record.ListFilter{
		KType: finance.KTypeARInvoice,
		Limit: 500,
	})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	return rows
}

func getRecurring(t *testing.T, h *harness, tenantID, recurringID uuid.UUID) *record.KRecord {
	t.Helper()
	r, err := h.records.Get(context.Background(), tenantID, recurringID)
	if err != nil {
		t.Fatalf("get recurring: %v", err)
	}
	return r
}

// pauseRecurring flips a recurring_invoice row to status=paused so
// subsequent sweeps skip it. Used by tests that stack multiple
// recurring rows in the same tenant and want to isolate assertions
// to the most recently-seeded row.
func pauseRecurring(t *testing.T, h *harness, tenantID, recurringID uuid.UUID) {
	t.Helper()
	r, err := h.records.Get(context.Background(), tenantID, recurringID)
	if err != nil {
		t.Fatalf("get recurring for pause: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("decode recurring: %v", err)
	}
	data["status"] = finance.RecurringStatusPaused
	patch, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal recurring: %v", err)
	}
	if _, err := h.records.Update(context.Background(), record.KRecord{
		TenantID:  tenantID,
		ID:        recurringID,
		Version:   r.Version,
		Data:      patch,
		UpdatedBy: r.UpdatedBy,
	}); err != nil {
		t.Fatalf("pause recurring: %v", err)
	}
}

func readString(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// TestRecurringInvoiceTableEnforcesRLS verifies a second tenant
// cannot see the first tenant's recurring_invoice rows. The records
// store routes every read through dbutil.WithTenantTx, so a missing
// or wrong RLS policy on krecords would surface here as a leak.
func TestRecurringInvoiceTableEnforcesRLS(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn1, _, _ := newTenantForFinance(t, h)
	tn2, _, _ := newTenantForFinance(t, h)

	actor := uuid.New()
	tmpl := createARInvoiceRecord(t, h, tn1.ID, actor,
		"INV-RLS", uuid.NewString(),
		decimal.NewFromInt(100), decimal.Zero, "")
	createRecurringInvoiceRecord(t, h, tn1.ID, actor, templateRow{
		Name:               "Tenant1-only",
		TemplateID:         tmpl,
		Frequency:          finance.FrequencyMonthly,
		StartDate:          "2026-01-01",
		NextGenerationDate: "2026-01-01",
	})

	rows, err := h.records.List(ctx, tn2.ID, record.ListFilter{
		KType: finance.KTypeRecurringInvoice,
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("tenant2 list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("RLS leak: tenant2 saw %d recurring_invoice rows from tenant1", len(rows))
	}
}

// Compile-only guard: make sure ActionTypeRecurringInvoice and the
// default cadence constant stay exported. If either name drifts the
// test fails to compile, surfacing the regression at CI time.
var (
	_ = finance.ActionTypeRecurringInvoice
	_ = finance.DefaultRecurringInvoiceIntervalSeconds
	_ = fmt.Sprintf
)
