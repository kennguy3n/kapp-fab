//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// TestComputePaymentScheduleSplits50_50 covers the simplest path: a
// two-installment template with no discounts. Verifies the schedule
// has one row per installment, due_date offsets resolve from
// issue_date, percentage→amount math is correct, and amounts sum to
// the invoice total exactly (no rounding leak).
func TestComputePaymentScheduleSplits50_50(t *testing.T) {
	issue := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	terms := []finance.PaymentTermsInstallment{
		{DueDays: 0, Percentage: decimal.NewFromInt(50), Label: "deposit"},
		{DueDays: 30, Percentage: decimal.NewFromInt(50), Label: "balance"},
	}
	out, err := finance.ComputePaymentSchedule(issue, decimal.NewFromInt(1000), terms)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 entries, got %d", len(out))
	}
	if out[0].DueDate != "2026-05-01" || out[0].Amount.String() != "500" {
		t.Errorf("entry 0: %+v", out[0])
	}
	if out[1].DueDate != "2026-05-31" || out[1].Amount.String() != "500" {
		t.Errorf("entry 1: %+v", out[1])
	}
	if out[0].Label != "deposit" || out[1].Label != "balance" {
		t.Errorf("labels lost: %+v", out)
	}
}

// TestComputePaymentScheduleHandlesRoundingRemainder picks
// percentages whose product against the invoice total yields a
// fractional amount (1000 / 3 = 333.33…). The first two
// installments should round down, the last installment absorbs the
// remainder so the schedule's amounts still sum to 1000 exactly.
func TestComputePaymentScheduleHandlesRoundingRemainder(t *testing.T) {
	issue := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	terms := []finance.PaymentTermsInstallment{
		{DueDays: 0, Percentage: decimal.NewFromFloat(33.33)},
		{DueDays: 30, Percentage: decimal.NewFromFloat(33.33)},
		{DueDays: 60, Percentage: decimal.NewFromFloat(33.34)},
	}
	out, err := finance.ComputePaymentSchedule(issue, decimal.NewFromInt(1000), terms)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	sum := decimal.Zero
	for _, e := range out {
		sum = sum.Add(e.Amount)
	}
	if !sum.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("schedule sum %s != 1000", sum)
	}
}

// TestComputePaymentScheduleEmitsDiscount verifies that an
// installment with a positive discount_days+discount_percent emits
// discount_amount + discount_until on the rendered entry.
func TestComputePaymentScheduleEmitsDiscount(t *testing.T) {
	issue := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	terms := []finance.PaymentTermsInstallment{
		{
			DueDays: 30, Percentage: decimal.NewFromInt(100),
			DiscountDays: 10, DiscountPercent: decimal.NewFromInt(2),
		},
	}
	out, err := finance.ComputePaymentSchedule(issue, decimal.NewFromInt(1000), terms)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 entry, got %d", len(out))
	}
	if out[0].DiscountAmount.String() != "20" {
		t.Errorf("discount amount: got %s want 20", out[0].DiscountAmount)
	}
	if out[0].DiscountUntil != "2026-05-11" {
		t.Errorf("discount_until: got %s want 2026-05-11", out[0].DiscountUntil)
	}
}

// TestComputePaymentScheduleRejectsBadPercentages verifies that
// installment percentages summing wildly off 100 are refused. A
// silent acceptance here would silently over- or under-allocate
// revenue against AR.
func TestComputePaymentScheduleRejectsBadPercentages(t *testing.T) {
	issue := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	terms := []finance.PaymentTermsInstallment{
		{DueDays: 0, Percentage: decimal.NewFromInt(40)},
		{DueDays: 30, Percentage: decimal.NewFromInt(40)},
	}
	_, err := finance.ComputePaymentSchedule(issue, decimal.NewFromInt(1000), terms)
	if err == nil {
		t.Fatalf("expected error for percentages summing to 80")
	}
}

// TestPostSalesInvoiceMaterialisesPaymentSchedule wires the full
// end-to-end path: seed a finance.payment_terms KRecord, attach its
// id to a draft AR invoice, post the invoice, and assert the
// posted record's data carries a payment_schedule that sums to the
// invoice total.
func TestPostSalesInvoiceMaterialisesPaymentSchedule(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn, _, poster := newTenantForFinance(t, h)
	actor := uuid.New()

	termsID := createPaymentTerms(t, h, tn.ID, actor, "Net30 50/50", []map[string]any{
		{"due_days": 0, "percentage": 50, "label": "deposit"},
		{"due_days": 30, "percentage": 50, "label": "balance"},
	})

	invoiceID := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-PT-001", uuid.NewString(),
		decimal.NewFromInt(2000), decimal.Zero, "")

	// Patch the draft to reference the payment terms.
	patch, _ := json.Marshal(map[string]any{
		"payment_terms_id": termsID.String(),
	})
	rec, err := h.records.Get(ctx, tn.ID, invoiceID)
	if err != nil {
		t.Fatalf("reload invoice draft: %v", err)
	}
	if _, err := h.records.Update(ctx, record.KRecord{
		ID:        invoiceID,
		TenantID:  tn.ID,
		Version:   rec.Version,
		Data:      patch,
		UpdatedBy: &actor,
	}); err != nil {
		t.Fatalf("attach payment_terms: %v", err)
	}

	if _, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor); err != nil {
		t.Fatalf("post invoice: %v", err)
	}

	posted, err := h.records.Get(ctx, tn.ID, invoiceID)
	if err != nil {
		t.Fatalf("reload posted invoice: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(posted.Data, &data); err != nil {
		t.Fatalf("decode posted: %v", err)
	}
	schedule, ok := data["payment_schedule"].([]any)
	if !ok {
		t.Fatalf("payment_schedule missing or wrong type: %T", data["payment_schedule"])
	}
	if len(schedule) != 2 {
		t.Fatalf("want 2 schedule rows, got %d", len(schedule))
	}
	sum := decimal.Zero
	for _, raw := range schedule {
		row, _ := raw.(map[string]any)
		amt := decimal.RequireFromString(asString(t, row["amount"]))
		sum = sum.Add(amt)
	}
	if !sum.Equal(decimal.NewFromInt(2000)) {
		t.Fatalf("schedule sum %s != 2000 (invoice total)", sum)
	}
}

// TestPostSalesInvoiceWithoutPaymentTermsLeavesScheduleEmpty makes
// sure the new path is opt-in: an invoice without
// payment_terms_id has no payment_schedule on the posted record.
func TestPostSalesInvoiceWithoutPaymentTermsLeavesScheduleEmpty(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn, _, poster := newTenantForFinance(t, h)
	actor := uuid.New()
	invoiceID := createARInvoiceRecord(t, h, tn.ID, actor,
		"INV-NOTERMS", uuid.NewString(),
		decimal.NewFromInt(500), decimal.Zero, "")

	if _, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor); err != nil {
		t.Fatalf("post invoice: %v", err)
	}
	posted, _ := h.records.Get(ctx, tn.ID, invoiceID)
	var data map[string]any
	_ = json.Unmarshal(posted.Data, &data)
	if _, ok := data["payment_schedule"]; ok {
		t.Fatalf("payment_schedule should be absent when no payment_terms_id; got %v", data["payment_schedule"])
	}
}

// TestPaymentTermsTableEnforcesRLS verifies a second tenant cannot
// see the first tenant's payment_terms records.
func TestPaymentTermsTableEnforcesRLS(t *testing.T) {
	h := newHarness(t)

	ctx := context.Background()
	tn1, _, _ := newTenantForFinance(t, h)
	tn2, _, _ := newTenantForFinance(t, h)
	actor := uuid.New()

	createPaymentTerms(t, h, tn1.ID, actor, "Tenant1 Net30", []map[string]any{
		{"due_days": 30, "percentage": 100},
	})

	rows, err := h.records.List(ctx, tn2.ID, record.ListFilter{
		KType: finance.KTypePaymentTerms,
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("tenant2 list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("RLS leak: tenant2 saw %d payment_terms rows from tenant1", len(rows))
	}
}

// createPaymentTerms inserts a finance.payment_terms record with
// the supplied installment plan. Installments are passed as raw
// maps so each test stays readable about exactly what shape went
// into the registry.
func createPaymentTerms(t *testing.T, h *harness, tenantID, actorID uuid.UUID, name string, installments []map[string]any) uuid.UUID {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name":         name,
		"installments": installments,
		"active":       true,
	})
	if err != nil {
		t.Fatalf("marshal terms: %v", err)
	}
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypePaymentTerms,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create payment_terms: %v", err)
	}
	return rec.ID
}

// asString coerces an arbitrary JSON-decoded number/string into its
// canonical string form. Used by the schedule sum assertion since
// json.Unmarshal lands the decimal payload as either string or
// float64 depending on Go's default decoder.
func asString(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return decimal.NewFromFloat(x).String()
	default:
		t.Fatalf("unexpected amount type %T", v)
		return ""
	}
}
