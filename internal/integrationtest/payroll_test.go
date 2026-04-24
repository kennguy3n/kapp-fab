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
// increments instead.
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
