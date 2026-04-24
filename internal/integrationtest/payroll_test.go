//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// TestPayrollEngineGeneratesSlipsFromStructure end-to-end: seed an
// employee + a salary_structure with fixed and percentage
// components, run GeneratePayslips, assert the resulting slip's
// earnings/deductions/gross/net match the expected roll.
func TestPayrollEngineGeneratesSlipsFromStructure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	empID := createEmployeeRecord(t, h, tn.ID, actor, "Engineer")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(5000), []map[string]any{
		{"code": "BONUS", "name": "Monthly Bonus", "type": "earning", "amount_type": "fixed", "amount": 500},
		{"code": "TAX", "name": "Income Tax", "type": "deduction", "amount_type": "percentage", "amount": 20},
	})
	runID := createPayRun(t, h, tn.ID, actor, "Jun 2026", "2026-06-01", "2026-06-30", "")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.CreatedCount != 1 {
		t.Fatalf("want 1 slip created, got %d (skipped_existing=%d skipped_no_struct=%d)",
			res.CreatedCount, res.SkippedExisting, res.SkippedNoStruct)
	}

	slip := getRecord(t, h, tn.ID, res.PayslipIDs[0])
	var sd map[string]any
	if err := json.Unmarshal(slip.Data, &sd); err != nil {
		t.Fatalf("decode slip: %v", err)
	}
	// Base 5000 + bonus 500 = gross 5500; tax 20% of 5000 = 1000
	// deduction; net 4500.
	gross := decimal.RequireFromString(asStrNum(t, sd["gross_pay"]))
	deduct := decimal.RequireFromString(asStrNum(t, sd["total_deductions"]))
	net := decimal.RequireFromString(asStrNum(t, sd["net_pay"]))
	if !gross.Equal(decimal.NewFromInt(5500)) {
		t.Errorf("gross: got %s want 5500", gross)
	}
	if !deduct.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("deductions: got %s want 1000", deduct)
	}
	if !net.Equal(decimal.NewFromInt(4500)) {
		t.Errorf("net: got %s want 4500", net)
	}
}

// TestPayrollEngineIsIdempotentOnReGenerate: a second call with the
// same pay_run_id does NOT duplicate slips; skipped_existing count
// increments instead; and the pay_run's total_gross / total_net
// are preserved rather than overwritten to zero.
func TestPayrollEngineIsIdempotentOnReGenerate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	empID := createEmployeeRecord(t, h, tn.ID, actor, "")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(4000), nil)
	runID := createPayRun(t, h, tn.ID, actor, "Jul 2026", "2026-07-01", "2026-07-31", "")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	if _, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor); err != nil {
		t.Fatalf("first generate: %v", err)
	}
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if res.CreatedCount != 0 {
		t.Fatalf("second call created %d (want 0)", res.CreatedCount)
	}
	if res.SkippedExisting != 1 {
		t.Fatalf("second call skipped_existing=%d (want 1)", res.SkippedExisting)
	}

	// Regression: idempotent re-run must preserve pay_run
	// total_gross / total_net, not zero them.
	runAfter := getRecord(t, h, tn.ID, runID)
	var runData map[string]any
	if err := json.Unmarshal(runAfter.Data, &runData); err != nil {
		t.Fatalf("decode pay_run: %v", err)
	}
	gross := decimal.RequireFromString(asStrNum(t, runData["total_gross"]))
	net := decimal.RequireFromString(asStrNum(t, runData["total_net"]))
	if !gross.Equal(decimal.NewFromInt(4000)) {
		t.Errorf("total_gross after re-run: got %s want 4000", gross)
	}
	if !net.Equal(decimal.NewFromInt(4000)) {
		t.Errorf("total_net after re-run: got %s want 4000", net)
	}
	if count, _ := runData["payslip_count"].(float64); count != 1 {
		t.Errorf("payslip_count after re-run: got %v want 1", runData["payslip_count"])
	}
}

// TestPayrollEngineHandlesMoreThan50Employees is a regression test
// for the silent-clamp bug on record.PGStore.List (capped at 500,
// defaults to 50). The engine now uses ListAll; seed 51 employees
// each with an active salary_structure and assert GeneratePayslips
// writes 51 slips (not 50).
func TestPayrollEngineHandlesMoreThan50Employees(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	const n = 51
	for i := 0; i < n; i++ {
		empID := createEmployeeRecord(t, h, tn.ID, actor, "Engineer")
		createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(1000), nil)
	}
	runID := createPayRun(t, h, tn.ID, actor, "Jun 2026", "2026-06-01", "2026-06-30", "")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.CreatedCount != n {
		t.Fatalf("created %d want %d (skipped_existing=%d skipped_no_struct=%d)",
			res.CreatedCount, n, res.SkippedExisting, res.SkippedNoStruct)
	}

	// Re-run to confirm idempotency still covers every employee,
	// not just the first 50. The second call must skip all 51.
	res2, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate (second call): %v", err)
	}
	if res2.CreatedCount != 0 || res2.SkippedExisting != n {
		t.Fatalf("idempotent re-run: created=%d skipped_existing=%d want 0/%d",
			res2.CreatedCount, res2.SkippedExisting, n)
	}
}

// TestPayrollEngineListPayslipsForRunReturnsAllSlips is the
// regression guard for the "PayslipsForRun UI silently truncates at
// 50 rows" bug. The engine-level listing walks every row via
// ListAll, so even with >50 total payslips across all runs in the
// tenant, calling ListPayslipsForRun for a specific run must return
// all matching slips.
func TestPayrollEngineListPayslipsForRunReturnsAllSlips(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	// Seed 60 employees split across two runs: 55 into run A, 5
	// into run B. Post-generation there are 60 payslips total, so
	// the generic 50-row list cap would only surface a subset. The
	// engine's dedicated path must return exactly 55 for run A and
	// exactly 5 for run B.
	var aEmps, bEmps []uuid.UUID
	for i := 0; i < 55; i++ {
		empID := createEmployeeRecord(t, h, tn.ID, actor, "Engineering")
		createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(1000), nil)
		aEmps = append(aEmps, empID)
	}
	for i := 0; i < 5; i++ {
		empID := createEmployeeRecord(t, h, tn.ID, actor, "Sales")
		createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(1000), nil)
		bEmps = append(bEmps, empID)
	}
	runA := createPayRun(t, h, tn.ID, actor, "A", "2026-09-01", "2026-09-30", "Engineering")
	runB := createPayRun(t, h, tn.ID, actor, "B", "2026-09-01", "2026-09-30", "Sales")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	if _, err := engine.GeneratePayslips(ctx, tn.ID, runA, actor); err != nil {
		t.Fatalf("generate A: %v", err)
	}
	if _, err := engine.GeneratePayslips(ctx, tn.ID, runB, actor); err != nil {
		t.Fatalf("generate B: %v", err)
	}

	aSlips, err := engine.ListPayslipsForRun(ctx, tn.ID, runA)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(aSlips) != len(aEmps) {
		t.Fatalf("run A: got %d slips, want %d", len(aSlips), len(aEmps))
	}
	bSlips, err := engine.ListPayslipsForRun(ctx, tn.ID, runB)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bSlips) != len(bEmps) {
		t.Fatalf("run B: got %d slips, want %d", len(bSlips), len(bEmps))
	}
	// Sanity: every returned slip for run A must carry pay_run_id=runA.
	for _, s := range aSlips {
		var sd map[string]any
		if err := json.Unmarshal(s.Data, &sd); err != nil {
			t.Fatalf("decode slip: %v", err)
		}
		if got, _ := sd["pay_run_id"].(string); got != runA.String() {
			t.Fatalf("cross-run leak: slip %s has pay_run_id=%s want %s", s.ID, got, runA)
		}
	}
}

// TestPayrollEngineFiltersByDepartment verifies the optional
// department filter on pay_run trims out-of-scope employees.
func TestPayrollEngineFiltersByDepartment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	engID := createEmployeeRecord(t, h, tn.ID, actor, "Engineering")
	salesID := createEmployeeRecord(t, h, tn.ID, actor, "Sales")
	createSalaryStructure(t, h, tn.ID, actor, engID, "USD", decimal.NewFromInt(4000), nil)
	createSalaryStructure(t, h, tn.ID, actor, salesID, "USD", decimal.NewFromInt(4000), nil)

	runID := createPayRun(t, h, tn.ID, actor, "Aug 2026", "2026-08-01", "2026-08-31", "Engineering")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.CreatedCount != 1 {
		t.Fatalf("want 1 slip (Engineering only), got %d", res.CreatedCount)
	}
}

// TestPayrollEnginePostsJournalEntry seeds a run + approved slips,
// calls PostPayRun, asserts the resulting journal entry balances
// (Dr expense = Cr payable) and that pay_run.status flips to paid.
func TestPayrollEnginePostsJournalEntry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	// Seed payroll accounts on top of the finance chart.
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
	empID := createEmployeeRecord(t, h, tn.ID, actor, "")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(6000), []map[string]any{
		{"code": "TAX", "name": "Tax", "type": "deduction", "amount_type": "percentage", "amount": 10},
	})

	runID := createPayRunWithAccounts(t, h, tn.ID, actor,
		"Sep 2026", "2026-09-01", "2026-09-30", "", "5100", "2300")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Flip the lone slip to approved.
	slipRec := getRecord(t, h, tn.ID, res.PayslipIDs[0])
	approvePayslip(t, h, tn.ID, actor, slipRec)

	entry, err := engine.PostPayRun(ctx, tn.ID, runID, actor)
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

	// pay_run patched → paid
	runAfter := getRecord(t, h, tn.ID, runID)
	var runData map[string]any
	_ = json.Unmarshal(runAfter.Data, &runData)
	if runData["status"] != "paid" {
		t.Errorf("run status: got %v want paid", runData["status"])
	}
}

// TestPayrollEngineRejectsRunWithoutAccounts: PostPayRun with no
// salary accounts configured surfaces ErrMissingAccounts.
func TestPayrollEngineRejectsRunWithoutAccounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()
	runID := createPayRun(t, h, tn.ID, actor, "Oct 2026", "2026-10-01", "2026-10-31", "")

	engine := hr.NewPayrollEngine(h.records, ledgerStore)
	if _, err := engine.PostPayRun(ctx, tn.ID, runID, actor); err == nil {
		t.Fatalf("expected ErrMissingAccounts, got nil")
	}
}

// TestPayRunTableEnforcesRLS — tenant2 cannot read tenant1's runs.
func TestPayRunTableEnforcesRLS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn1, _, _ := newTenantForFinance(t, h)
	tn2, _, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	createPayRun(t, h, tn1.ID, actor, "RLS Run", "2026-11-01", "2026-11-30", "")

	rows, err := h.records.List(ctx, tn2.ID, record.ListFilter{
		KType: hr.KTypePayRun,
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("tenant2 list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("RLS leak: tenant2 saw %d pay_run rows from tenant1", len(rows))
	}
}

// --- fixtures ---------------------------------------------------------

// registerPayrollKTypes registers the Phase E HR catalog + the
// Phase J payroll KTypes (component / structure / payslip /
// pay_run). Idempotent: the underlying PGRegistry upserts on
// conflict.
func registerPayrollKTypes(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	if err := hr.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register hr ktypes: %v", err)
	}
	for _, kt := range hr.PayrollKTypes() {
		if err := h.ktypes.Register(ctx, kt); err != nil {
			t.Fatalf("register payroll ktype %s: %v", kt.Name, err)
		}
	}
}

func createEmployeeRecord(t *testing.T, h *harness, tenantID, actorID uuid.UUID, department string) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":            "Test Employee " + uuid.NewString()[:8],
		"email":           uuid.NewString() + "@example.com",
		"department":      department,
		"status":          "active",
		"date_of_joining": "2024-01-01",
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

func createSalaryStructure(t *testing.T, h *harness, tenantID, actorID, employeeID uuid.UUID, currency string, base decimal.Decimal, components []map[string]any) uuid.UUID {
	t.Helper()
	// Marshal components → JSON numbers so the schema validator
	// accepts them; decimal.Decimal serialises as a string otherwise.
	normComponents := make([]map[string]any, 0, len(components))
	for _, c := range components {
		nc := map[string]any{}
		for k, v := range c {
			nc[k] = v
		}
		if a, ok := nc["amount"]; ok {
			switch x := a.(type) {
			case int:
				nc["amount"] = float64(x)
			case float64:
				nc["amount"] = x
			case decimal.Decimal:
				f, _ := x.Float64()
				nc["amount"] = f
			}
		}
		normComponents = append(normComponents, nc)
	}
	baseF, _ := base.Float64()
	body, _ := json.Marshal(map[string]any{
		"employee_id":       employeeID.String(),
		"effective_from":    "2024-01-01",
		"currency":          currency,
		"base_salary":       baseF,
		"payment_frequency": "monthly",
		"components":        normComponents,
		"status":            "active",
	})
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     hr.KTypeSalaryStructure,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create salary_structure: %v", err)
	}
	return rec.ID
}

func createPayRun(t *testing.T, h *harness, tenantID, actorID uuid.UUID, name, start, end, department string) uuid.UUID {
	return createPayRunWithAccounts(t, h, tenantID, actorID, name, start, end, department, "", "")
}

func createPayRunWithAccounts(t *testing.T, h *harness, tenantID, actorID uuid.UUID, name, start, end, department, expenseCode, payableCode string) uuid.UUID {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":                        name,
		"pay_period_start":            start,
		"pay_period_end":              end,
		"department":                  department,
		"currency":                    "USD",
		"salary_expense_account_code": expenseCode,
		"salary_payable_account_code": payableCode,
		"status":                      "draft",
	})
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     hr.KTypePayRun,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create pay_run: %v", err)
	}
	return rec.ID
}

func approvePayslip(t *testing.T, h *harness, tenantID, actorID uuid.UUID, slip *record.KRecord) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"status": "approved"})
	if _, err := h.records.Update(context.Background(), record.KRecord{
		ID:        slip.ID,
		TenantID:  tenantID,
		Version:   slip.Version,
		Data:      body,
		UpdatedBy: &actorID,
	}); err != nil {
		t.Fatalf("approve payslip: %v", err)
	}
}

func getRecord(t *testing.T, h *harness, tenantID, id uuid.UUID) *record.KRecord {
	t.Helper()
	rec, err := h.records.Get(context.Background(), tenantID, id)
	if err != nil {
		t.Fatalf("get record %s: %v", id, err)
	}
	return rec
}

func asStrNum(t *testing.T, v any) string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return decimal.NewFromFloat(x).String()
	default:
		t.Fatalf("unexpected numeric type %T", v)
		return ""
	}
}
