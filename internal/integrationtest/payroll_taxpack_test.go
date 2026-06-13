//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr"
)

// TestPayrollEngineAppliesUSTaxPack is the Phase M Task 2 acceptance
// test for "tenants.country drives a per-country statutory pack
// during slip generation". It seeds an employee + salary_structure
// for a tenant whose CountryResolver returns "US", runs
// GeneratePayslips, and asserts the resulting slip carries
// FED_TAX + FICA_OASDI + FICA_MEDICARE deduction lines on top of
// the structure-driven components, with `total_deductions` and
// `net_pay` reflecting the new lines.
func TestPayrollEngineAppliesUSTaxPack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	empID := createEmployeeRecord(t, h, tn.ID, actor, "Engineer")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(5000), nil)
	runID := createPayRun(t, h, tn.ID, actor, "Mar 2026", "2026-03-01", "2026-03-31", "")

	resolver := func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		return "US", nil
	}
	engine := hr.NewPayrollEngine(h.records, ledgerStore).WithPool(h.pool).WithCountryResolver(resolver)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.CreatedCount != 1 {
		t.Fatalf("created %d slips, want 1", res.CreatedCount)
	}

	slip := getRecord(t, h, tn.ID, res.PayslipIDs[0])
	var sd map[string]any
	if err := json.Unmarshal(slip.Data, &sd); err != nil {
		t.Fatalf("decode slip: %v", err)
	}

	deductions, _ := sd["deductions"].([]any)
	codes := map[string]bool{}
	for _, d := range deductions {
		row, _ := d.(map[string]any)
		code, _ := row["code"].(string)
		codes[code] = true
	}
	for _, want := range []string{"FED_TAX", "FICA_OASDI", "FICA_MEDICARE"} {
		if !codes[want] {
			t.Errorf("missing %s deduction; got codes=%v", want, codes)
		}
	}

	gross := decimal.RequireFromString(asStrNum(t, sd["gross_pay"]))
	deduct := decimal.RequireFromString(asStrNum(t, sd["total_deductions"]))
	net := decimal.RequireFromString(asStrNum(t, sd["net_pay"]))
	if !gross.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("gross = %s, want 5000", gross)
	}
	if !deduct.IsPositive() {
		t.Errorf("expected non-zero statutory deductions, got %s", deduct)
	}
	if !net.Equal(gross.Sub(deduct)) {
		t.Errorf("net %s != gross %s - deductions %s", net, gross, deduct)
	}
}

// TestPayrollEngineAppliesAUTaxPack mirrors the US test but targets
// the AU resident schedule. The minimum-viable assertion is that a
// single PAYG_WITHHOLDING line lands on the slip and the gross/net
// rollup respects it.
func TestPayrollEngineAppliesAUTaxPack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	empID := createEmployeeRecord(t, h, tn.ID, actor, "")
	createSalaryStructure(t, h, tn.ID, actor, empID, "AUD", decimal.NewFromInt(6000), nil)
	runID := createPayRun(t, h, tn.ID, actor, "Mar 2026", "2026-03-01", "2026-03-31", "")

	resolver := func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		return "AU", nil
	}
	engine := hr.NewPayrollEngine(h.records, ledgerStore).WithPool(h.pool).WithCountryResolver(resolver)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.CreatedCount != 1 {
		t.Fatalf("created %d slips, want 1", res.CreatedCount)
	}

	slip := getRecord(t, h, tn.ID, res.PayslipIDs[0])
	var sd map[string]any
	if err := json.Unmarshal(slip.Data, &sd); err != nil {
		t.Fatalf("decode slip: %v", err)
	}

	deductions, _ := sd["deductions"].([]any)
	var foundPAYG bool
	for _, d := range deductions {
		row, _ := d.(map[string]any)
		code, _ := row["code"].(string)
		if strings.HasPrefix(code, "PAYG") {
			foundPAYG = true
		}
	}
	if !foundPAYG {
		t.Fatalf("expected a PAYG_* deduction on AU slip, got %v", deductions)
	}
}

// TestPayrollEngineSkipsStatutoryWhenCountryUnset confirms a tenant
// without a country code falls through to the legacy "no statutory
// pack" code path. The slip carries only the structure-driven
// components and matches the gross.
func TestPayrollEngineSkipsStatutoryWhenCountryUnset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	empID := createEmployeeRecord(t, h, tn.ID, actor, "Engineer")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(5000), nil)
	runID := createPayRun(t, h, tn.ID, actor, "Mar 2026", "2026-03-01", "2026-03-31", "")

	resolver := func(ctx context.Context, tenantID uuid.UUID) (string, error) {
		return "", nil // no country → no pack
	}
	engine := hr.NewPayrollEngine(h.records, ledgerStore).WithPool(h.pool).WithCountryResolver(resolver)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	slip := getRecord(t, h, tn.ID, res.PayslipIDs[0])
	var sd map[string]any
	if err := json.Unmarshal(slip.Data, &sd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	deduct := decimal.RequireFromString(asStrNum(t, sd["total_deductions"]))
	if !deduct.IsZero() {
		t.Fatalf("expected 0 deductions when no country pack resolved, got %s", deduct)
	}
}

// TestPayrollEngine401kReducesIncomeTaxNotFICA is the end-to-end two-base
// acceptance test. A US employee with a $1,000 pre-tax 401(k) deferral
// (default flag: reduces income tax only) on a $10,000 base must withhold
// FICA on the FULL $10,000 (OASDI $620 + Medicare $145) while federal
// income tax is computed on the reduced $9,000 income-tax base. It then
// verifies payroll_ytd advances cumulative_contribution_gross by the full
// gross and cumulative_taxable by the reduced base.
func TestPayrollEngine401kReducesIncomeTaxNotFICA(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn, ledgerStore, _ := newTenantForFinance(t, h)
	registerPayrollKTypes(t, h)
	actor := uuid.New()

	empID := createEmployeeRecord(t, h, tn.ID, actor, "Engineer")
	createSalaryStructure(t, h, tn.ID, actor, empID, "USD", decimal.NewFromInt(10000), []map[string]any{
		{"code": "401K", "name": "401(k) deferral", "type": "deduction",
			"amount_type": "fixed", "amount": 1000, "pre_tax": true},
	})
	runID := createPayRun(t, h, tn.ID, actor, "Mar 2026", "2026-03-01", "2026-03-31", "")

	resolver := func(ctx context.Context, tenantID uuid.UUID) (string, error) { return "US", nil }
	store := hr.NewPayrollStore(h.pool)
	engine := hr.NewPayrollEngine(h.records, ledgerStore).WithPool(h.pool).WithCountryResolver(resolver)
	res, err := engine.GeneratePayslips(ctx, tn.ID, runID, actor)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.CreatedCount != 1 {
		t.Fatalf("created %d slips, want 1", res.CreatedCount)
	}

	gross, taxable, _, _, _ := readTypedSlip(t, h, tn.ID, runID, empID)
	if !gross.Equal(decimal.NewFromInt(10000)) {
		t.Fatalf("gross = %s, want 10000", gross)
	}
	if !taxable.Equal(decimal.NewFromInt(9000)) {
		t.Fatalf("taxable = %s, want 9000 (10000 - 1000 pre-tax 401k)", taxable)
	}

	slip := getRecord(t, h, tn.ID, res.PayslipIDs[0])
	var sd map[string]any
	if err := json.Unmarshal(slip.Data, &sd); err != nil {
		t.Fatalf("decode slip: %v", err)
	}
	amounts := map[string]decimal.Decimal{}
	deductions, _ := sd["deductions"].([]any)
	for _, d := range deductions {
		row, _ := d.(map[string]any)
		code, _ := row["code"].(string)
		amounts[code] = decimal.RequireFromString(asStrNum(t, row["amount"]))
	}
	// FICA runs on the FULL $10,000 contribution base, unaffected by 401k.
	if amounts["FICA_OASDI"].Cmp(decimal.NewFromInt(620)) != 0 {
		t.Fatalf("FICA_OASDI = %s, want 620.00 (6.2%% of full gross)", amounts["FICA_OASDI"])
	}
	if amounts["FICA_MEDICARE"].Cmp(decimal.NewFromFloat(145)) != 0 {
		t.Fatalf("FICA_MEDICARE = %s, want 145.00 (1.45%% of full gross)", amounts["FICA_MEDICARE"])
	}

	// YTD: cumulative_taxable tracks the reduced income-tax base; the
	// contribution base tracks the full gross.
	ytd, err := store.LoadYTD(ctx, tn.ID, empID, 2026)
	if err != nil {
		t.Fatalf("load ytd: %v", err)
	}
	if !ytd.CumulativeTaxable.Equal(decimal.NewFromInt(9000)) {
		t.Fatalf("ytd cumulative_taxable = %s, want 9000", ytd.CumulativeTaxable)
	}
	if !ytd.CumulativeContributionGross.Equal(decimal.NewFromInt(10000)) {
		t.Fatalf("ytd cumulative_contribution_gross = %s, want 10000", ytd.CumulativeContributionGross)
	}
}
