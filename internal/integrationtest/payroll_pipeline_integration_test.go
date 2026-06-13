//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// createEmployeeWithJoin seeds an active employee with an explicit
// date_of_joining so the pipeline's proration path is exercised.
func createEmployeeWithJoin(t *testing.T, h *harness, tenantID, actorID uuid.UUID, joinDate string) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":            "Joiner " + uuid.NewString()[:8],
		"email":           uuid.NewString() + "@example.com",
		"status":          "active",
		"date_of_joining": joinDate,
	})
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     hr.KTypeEmployee,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create employee: %v", err)
	}
	return rec.ID
}

// TestPayrollPipeline_EndToEnd is the P1 acceptance test. A monthly run
// with a mid-month joiner (prorated) + an LOP day + a bonus + a
// salary-sacrifice pre-tax item produces a payslip whose gross/taxable/net
// tie out, whose lines are persisted in pipeline order, and whose
// payroll_ytd advances correctly across two consecutive months. It also
// asserts re-running a draft is idempotent (YTD not double-counted) and
// that PostPayRun emits a balanced journal entry from the persisted lines.
func TestPayrollPipeline_EndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	if _, err := ledgerStore.CreateAccount(ctx, ledger.Account{
		TenantID: tn.ID, Code: "5100", Name: "Salary Expense", Type: ledger.AccountTypeExpense, Active: true,
	}); err != nil {
		t.Fatalf("seed salary expense: %v", err)
	}
	if _, err := ledgerStore.CreateAccount(ctx, ledger.Account{
		TenantID: tn.ID, Code: "2300", Name: "Salary Payable", Type: ledger.AccountTypeLiability, Active: true,
	}); err != nil {
		t.Fatalf("seed salary payable: %v", err)
	}

	actor := uuid.New()
	empID := createEmployeeWithJoin(t, h, tn.ID, actor, "2026-03-17")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(3100), []map[string]any{
		{"code": "PENSION", "name": "Pension Sacrifice", "type": "deduction",
			"amount_type": "fixed", "amount": 300, "pre_tax": true},
	})

	store := hr.NewPayrollStore(h.pool)
	resolver := func(ctx context.Context, tenantID uuid.UUID) (string, error) { return "US", nil }
	engine := hr.NewPayrollEngine(h.records, ledgerStore).WithPool(h.pool).WithCountryResolver(resolver)

	// --- Month 1: March, joiner on the 17th, 1 LOP day + 500 bonus. -----
	run1 := createPayRunWithAccounts(t, h, tn.ID, actor, "Mar 2026", "2026-03-01", "2026-03-31", "", "5100", "2300")
	// Seed the typed run header so pay inputs (which FK to payroll_runs)
	// can be entered before generation, mirroring the run-management flow
	// where the typed run exists from creation.
	seedTypedRun(t, store, tn.ID, run1, actor, "Mar 2026", "2026-03-01", "2026-03-31")
	if _, err := store.AddPayInput(ctx, tn.ID, hr.PayInput{
		RunID: run1, EmployeeID: empID, Type: hr.PayInputLOPDays, Qty: decimal.NewFromInt(1),
	}, actor); err != nil {
		t.Fatalf("add LOP input: %v", err)
	}
	if _, err := store.AddPayInput(ctx, tn.ID, hr.PayInput{
		RunID: run1, EmployeeID: empID, Type: hr.PayInputBonus, Amount: decimal.NewFromInt(500), Taxable: true,
	}, actor); err != nil {
		t.Fatalf("add bonus input: %v", err)
	}

	res1, err := engine.GeneratePayslips(ctx, tn.ID, run1, actor)
	if err != nil {
		t.Fatalf("generate month1: %v", err)
	}
	if res1.CreatedCount != 1 {
		t.Fatalf("month1 created %d slips, want 1", res1.CreatedCount)
	}
	slip1ID := res1.PayslipIDs[0]

	// gross = prorated base (3100*15/31 = 1500) - LOP (100) + bonus (500) = 1900.
	gross1, taxable1, tax1, totalEE1, net1 := readTypedSlip(t, h, tn.ID, run1, empID)
	if !gross1.Equal(decimal.NewFromInt(1900)) {
		t.Fatalf("month1 gross = %s, want 1900", gross1)
	}
	// taxable = 1900 taxable earnings - 300 pre-tax pension = 1600.
	if !taxable1.Equal(decimal.NewFromInt(1600)) {
		t.Fatalf("month1 taxable = %s, want 1600", taxable1)
	}
	if !tax1.IsPositive() {
		t.Fatalf("month1 expected non-zero statutory tax, got %s", tax1)
	}
	// net = gross - total_ee_deductions; total_ee = pension(300) + tax + posttax(0).
	if !net1.Equal(gross1.Sub(totalEE1)) {
		t.Fatalf("month1 net %s != gross %s - totalEE %s", net1, gross1, totalEE1)
	}
	if !totalEE1.Equal(decimal.NewFromInt(300).Add(tax1)) {
		t.Fatalf("month1 totalEE %s != pension 300 + tax %s", totalEE1, tax1)
	}

	assertLinesInPipelineOrder(t, store, tn.ID, slip1ID)

	// YTD after month1: cumulative taxable gross == taxable1.
	ytd1, err := store.LoadYTD(ctx, tn.ID, empID, 2026)
	if err != nil {
		t.Fatalf("load ytd month1: %v", err)
	}
	if !ytd1.Exists || !ytd1.CumulativeGross.Equal(taxable1) {
		t.Fatalf("month1 ytd cumulative_gross = %s (exists=%v), want %s", ytd1.CumulativeGross, ytd1.Exists, taxable1)
	}
	if !ytd1.CumulativeTax.Equal(tax1) {
		t.Fatalf("month1 ytd cumulative_tax = %s, want %s", ytd1.CumulativeTax, tax1)
	}

	// --- Idempotency: re-running the draft run must not double-count. ---
	resDup, err := engine.GeneratePayslips(ctx, tn.ID, run1, actor)
	if err != nil {
		t.Fatalf("re-generate month1: %v", err)
	}
	if resDup.CreatedCount != 0 || resDup.SkippedExisting != 1 {
		t.Fatalf("re-generate should skip the existing slip: created=%d skipped=%d", resDup.CreatedCount, resDup.SkippedExisting)
	}
	ytdDup, err := store.LoadYTD(ctx, tn.ID, empID, 2026)
	if err != nil {
		t.Fatalf("load ytd after re-run: %v", err)
	}
	if !ytdDup.CumulativeGross.Equal(ytd1.CumulativeGross) {
		t.Fatalf("re-run double-counted ytd: got %s want %s", ytdDup.CumulativeGross, ytd1.CumulativeGross)
	}

	// --- Month 2: April, full period (no proration / inputs). -----------
	run2 := createPayRunWithAccounts(t, h, tn.ID, actor, "Apr 2026", "2026-04-01", "2026-04-30", "", "5100", "2300")
	res2, err := engine.GeneratePayslips(ctx, tn.ID, run2, actor)
	if err != nil {
		t.Fatalf("generate month2: %v", err)
	}
	if res2.CreatedCount != 1 {
		t.Fatalf("month2 created %d slips, want 1", res2.CreatedCount)
	}
	gross2, taxable2, _, _, _ := readTypedSlip(t, h, tn.ID, run2, empID)
	if !gross2.Equal(decimal.NewFromInt(3100)) {
		t.Fatalf("month2 gross = %s, want 3100", gross2)
	}
	if !taxable2.Equal(decimal.NewFromInt(2800)) {
		t.Fatalf("month2 taxable = %s, want 2800", taxable2)
	}

	// YTD now spans both runs: cumulative taxable = taxable1 + taxable2.
	ytd2, err := store.LoadYTD(ctx, tn.ID, empID, 2026)
	if err != nil {
		t.Fatalf("load ytd month2: %v", err)
	}
	wantCum := taxable1.Add(taxable2)
	if !ytd2.CumulativeGross.Equal(wantCum) {
		t.Fatalf("month2 ytd cumulative_gross = %s, want %s", ytd2.CumulativeGross, wantCum)
	}

	// --- GL tie-out: approve month1's slip and post the run. ------------
	slipRec := getRecord(t, h, tn.ID, slip1ID)
	approvePayslip(t, h, tn.ID, actor, slipRec)
	entry, err := engine.PostPayRun(ctx, tn.ID, run1, actor)
	if err != nil {
		t.Fatalf("post pay run: %v", err)
	}
	var dr, cr decimal.Decimal
	for _, line := range entry.Lines {
		dr = dr.Add(line.Debit)
		cr = cr.Add(line.Credit)
	}
	if !dr.Equal(cr) {
		t.Fatalf("JE not balanced: debits %s credits %s", dr, cr)
	}
	// Expense debit equals the slip gross sourced from the typed lines.
	if !dr.Equal(gross1) {
		t.Fatalf("JE total %s != month1 gross %s", dr, gross1)
	}

	// The typed run + slip should now read paid.
	runRec := getRecord(t, h, tn.ID, run1)
	var runData map[string]any
	_ = json.Unmarshal(runRec.Data, &runData)
	if runData["status"] != "paid" {
		t.Errorf("run status: got %v want paid", runData["status"])
	}
}

// seedTypedRun upserts the typed payroll_runs header so pay inputs can be
// attached before slip generation.
func seedTypedRun(t *testing.T, store *hr.PayrollStore, tenantID, runID, actorID uuid.UUID, name, start, end string) {
	t.Helper()
	ps, err := time.Parse("2006-01-02", start)
	if err != nil {
		t.Fatalf("parse start: %v", err)
	}
	pe, err := time.Parse("2006-01-02", end)
	if err != nil {
		t.Fatalf("parse end: %v", err)
	}
	if err := store.UpsertRun(context.Background(), tenantID, hr.RunHeader{
		ID:                       runID,
		Name:                     name,
		PeriodStart:              ps,
		PeriodEnd:                pe,
		Currency:                 "USD",
		Status:                   "draft",
		SalaryExpenseAccountCode: "5100",
		SalaryPayableAccountCode: "2300",
		CreatedBy:                actorID,
	}); err != nil {
		t.Fatalf("seed typed run: %v", err)
	}
}

// readTypedSlip returns the persisted typed payslip rollup for a
// (run, employee), read under the tenant's RLS context.
func readTypedSlip(t *testing.T, h *harness, tenantID, runID, employeeID uuid.UUID) (gross, taxable, tax, totalEE, net decimal.Decimal) {
	t.Helper()
	const q = `SELECT gross, taxable_gross, tax_total, total_ee_deductions, net
	             FROM payroll_payslips WHERE tenant_id = $1 AND run_id = $2 AND employee_id = $3`
	if err := dbutil.WithTenantTx(context.Background(), h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, tenantID, runID, employeeID).
			Scan(&gross, &taxable, &tax, &totalEE, &net)
	}); err != nil {
		t.Fatalf("read typed slip: %v", err)
	}
	return gross, taxable, tax, totalEE, net
}

// assertLinesInPipelineOrder verifies the persisted lines are ordered
// earning… → pretax_deduction… → tax… → posttax_deduction… with a
// strictly increasing seq.
func assertLinesInPipelineOrder(t *testing.T, store *hr.PayrollStore, tenantID, payslipID uuid.UUID) {
	t.Helper()
	lines, err := store.GetPayslipLines(context.Background(), tenantID, payslipID)
	if err != nil {
		t.Fatalf("get payslip lines: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("expected persisted lines")
	}
	rank := map[string]int{
		"earning":           0,
		"pretax_deduction":  1,
		"tax":               2,
		"posttax_deduction": 3,
		"er_contribution":   4,
	}
	prevSeq := -1
	prevRank := -1
	for _, l := range lines {
		if l.Seq <= prevSeq {
			t.Fatalf("seq not strictly increasing: %d after %d", l.Seq, prevSeq)
		}
		r, ok := rank[l.Kind]
		if !ok {
			t.Fatalf("unexpected line kind %q", l.Kind)
		}
		if r < prevRank {
			t.Fatalf("line kind %q (rank %d) out of pipeline order after rank %d", l.Kind, r, prevRank)
		}
		prevSeq = l.Seq
		prevRank = r
	}
}

// TestPayrollPipeline_ConcurrentYTD asserts FinalizePayslip serializes
// two concurrent finalizers for the SAME (employee, tax_year) when no
// payroll_ytd row exists yet — the first-of-year race. Both calls must
// succeed (no PK-violation rollback) and the accumulator must reflect
// each contribution exactly once (no double-count, no lost update).
func TestPayrollPipeline_ConcurrentYTD(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, _, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)

	actor := uuid.New()
	empID := createEmployeeWithJoin(t, h, tn.ID, actor, "2026-01-01")

	store := hr.NewPayrollStore(h.pool)

	// Two distinct runs in the same tax year so each finalizer writes a
	// separate slip but contends on the single (employee, 2026) YTD row.
	runA := uuid.New()
	runB := uuid.New()
	seedTypedRun(t, store, tn.ID, runA, actor, "Jan 2026", "2026-01-01", "2026-01-31")
	seedTypedRun(t, store, tn.ID, runB, actor, "Feb 2026", "2026-02-01", "2026-02-28")

	mkInput := func(runID uuid.UUID, start, end string, taxable int64) hr.FinalizeInput {
		ps, _ := time.Parse("2006-01-02", start)
		pe, _ := time.Parse("2006-01-02", end)
		amt := decimal.NewFromInt(taxable)
		return hr.FinalizeInput{
			TenantID:     tn.ID,
			RunID:        runID,
			PayslipID:    uuid.New(),
			EmployeeID:   empID,
			Currency:     "USD",
			PeriodStart:  ps,
			PeriodEnd:    pe,
			TaxYear:      2026,
			Gross:        amt,
			TaxableGross: amt,
			Earnings: []hr.PayslipLine{
				{Code: "BASE", Label: "Base Salary", Amount: amt, Taxable: true},
			},
		}
	}
	inA := mkInput(runA, "2026-01-01", "2026-01-31", 1000)
	inB := mkInput(runB, "2026-02-01", "2026-02-28", 2000)

	// Release both goroutines from a common barrier to maximize the
	// chance they reach the seed/lock step simultaneously.
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i, in := range []hr.FinalizeInput{inA, inB} {
		wg.Add(1)
		go func(idx int, fi hr.FinalizeInput) {
			defer wg.Done()
			<-start
			_, errs[idx] = store.FinalizePayslip(ctx, fi)
		}(i, in)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent FinalizePayslip[%d] failed: %v", i, err)
		}
	}

	// Both contributions land exactly once: 1000 + 2000 = 3000.
	ytd, err := store.LoadYTD(ctx, tn.ID, empID, 2026)
	if err != nil {
		t.Fatalf("load ytd: %v", err)
	}
	if !ytd.Exists {
		t.Fatalf("expected a YTD row after concurrent finalizers")
	}
	if want := decimal.NewFromInt(3000); !ytd.CumulativeGross.Equal(want) {
		t.Fatalf("ytd cumulative_gross = %s, want %s (double-count or lost update)", ytd.CumulativeGross, want)
	}
}
