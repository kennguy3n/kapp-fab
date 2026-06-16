package hr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr/taxpacks"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// CountryResolver returns the ISO 3166-1 alpha-2 country code for a
// tenant. Implemented by *tenant.PGStore via a thin adapter so the
// hr package doesn't import tenant directly (and keeps the existing
// no-cycle invariant). An empty country (the tenant has none, or the
// row doesn't exist) is treated as "no statutory pack" — the slip
// simply skips statutory withholding. A non-nil error, by contrast,
// signals a genuine lookup failure (e.g. a transient database error)
// and aborts the run: the engine must not silently emit payslips
// with zero withholding because the jurisdiction couldn't be read.
type CountryResolver func(ctx context.Context, tenantID uuid.UUID) (string, error)

// Payroll engine — materialises payslips off salary structures for a
// pay_run, and posts the approved batch as a single journal entry
// (Dr salary expense / Cr salary payable).
//
// Draft payslips are idempotent per (pay_run_id, employee_id): a
// second call with the same pay_run_id skips employees whose slip
// already exists.

// Sentinels surfaced by the engine.
var (
	ErrPayRunNotFound    = errors.New("hr: pay_run not found")
	ErrPayRunWrongStatus = errors.New("hr: pay_run in wrong status for action")
	ErrNoActiveEmployees = errors.New("hr: no active employees in scope")
	ErrNoActiveStructure = errors.New("hr: employee has no active salary_structure for period")
	ErrNoApprovedSlips   = errors.New("hr: pay_run has no approved payslips to post")
	ErrMissingAccounts   = errors.New("hr: pay_run missing salary_expense/salary_payable account codes")
)

// PayrollEngine owns the generation + posting surface. The ledger
// store is optional — `GeneratePayslips` does not touch it. It's
// only required by `PostPayRun`.
type PayrollEngine struct {
	records  *record.PGStore
	ledger   *ledger.PGStore
	now      func() time.Time
	resolver CountryResolver
	// pool backs the typed payroll tables (runs / payslips / lines /
	// pay_inputs / ytd). GeneratePayslips requires it; PostPayRun uses it
	// to source the journal entry from the persisted typed lines and to
	// keep the typed run/slip status in sync. Wired via WithPool.
	pool *pgxpool.Pool
}

// NewPayrollEngine binds the engine to a record store. Pass the
// ledger store to enable PostPayRun.
func NewPayrollEngine(records *record.PGStore, ledgerStore *ledger.PGStore) *PayrollEngine {
	return &PayrollEngine{records: records, ledger: ledgerStore, now: time.Now}
}

// WithPool wires the pgx pool that backs the typed payroll tables. The
// typed model (payroll_runs / payroll_payslips / payroll_payslip_lines /
// payroll_pay_inputs / payroll_ytd) is the source of truth for the
// ordered calculation pipeline and persisted YTD; GeneratePayslips
// returns an error if no pool is configured.
func (e *PayrollEngine) WithPool(pool *pgxpool.Pool) *PayrollEngine {
	e.pool = pool
	return e
}

// payrollStore builds a typed-table store bound to the engine's pool and
// clock. Returns nil when no pool is configured.
func (e *PayrollEngine) payrollStore() *PayrollStore {
	if e.pool == nil {
		return nil
	}
	return NewPayrollStore(e.pool).WithClock(e.now)
}

// WithClock overrides the engine's now() source so tests can drive
// deterministic timestamps through the posting path.
func (e *PayrollEngine) WithClock(now func() time.Time) *PayrollEngine {
	if now != nil {
		e.now = now
	}
	return e
}

// WithCountryResolver wires a tenant→country lookup so the engine
// can resolve a per-country tax pack at slip generation time. A nil
// resolver disables statutory withholding entirely (matching the
// pre-Phase-M behaviour); a resolver that returns "" (or a country
// with no registered pack) falls back to the no-pack code path. A
// resolver that returns a non-nil error aborts GeneratePayslips so a
// transient lookup failure can't silently zero out withholding.
func (e *PayrollEngine) WithCountryResolver(r CountryResolver) *PayrollEngine {
	e.resolver = r
	return e
}

// resolveTaxPack looks up the statutory tax pack for a tenant via the
// configured resolver. A nil resolver, an empty country, or a country
// with no registered pack all yield the zero-value pack — i.e. the
// slip runs without statutory withholding. A resolver error is
// wrapped and returned so callers abort rather than silently treating
// a transient lookup failure as "no withholding".
func (e *PayrollEngine) resolveTaxPack(ctx context.Context, tenantID uuid.UUID) (taxpacks.TaxPack, error) {
	var pack taxpacks.TaxPack
	if e.resolver == nil {
		return pack, nil
	}
	country, err := e.resolver(ctx, tenantID)
	if err != nil {
		return pack, fmt.Errorf("hr: resolve tenant country: %w", err)
	}
	if country != "" {
		if p, err := taxpacks.Lookup(country); err == nil {
			pack = p
		}
	}
	return pack, nil
}

// GenerateResult describes what happened during GeneratePayslips. All
// fields are populated even if no slips were actually written (e.g.
// every in-scope employee already had a slip for the run).
type GenerateResult struct {
	PayslipIDs      []uuid.UUID `json:"payslip_ids"`
	CreatedCount    int         `json:"created_count"`
	SkippedExisting int         `json:"skipped_existing"`
	SkippedNoStruct int         `json:"skipped_no_structure"`
}

// GeneratePayslips walks active employees (optionally filtered by
// department on the pay_run), resolves each employee's salary
// structure, rolls the components into earnings/deductions, and
// writes a draft payslip KRecord. Idempotent per (pay_run_id,
// employee_id): existing slips are skipped, not replaced.
func (e *PayrollEngine) GeneratePayslips(
	ctx context.Context, tenantID, payRunID, actorID uuid.UUID,
) (*GenerateResult, error) {
	if e.records == nil {
		return nil, errors.New("hr: payroll engine records store nil")
	}
	if tenantID == uuid.Nil || payRunID == uuid.Nil || actorID == uuid.Nil {
		return nil, errors.New("hr: tenant_id, pay_run_id and actor_id required")
	}

	runRec, err := e.records.Get(ctx, tenantID, payRunID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPayRunNotFound, err)
	}
	if runRec.KType != KTypePayRun {
		return nil, fmt.Errorf("%w: %s is %s", ErrPayRunNotFound, payRunID, runRec.KType)
	}
	var run payRunData
	if err := json.Unmarshal(runRec.Data, &run); err != nil {
		return nil, fmt.Errorf("hr: decode pay_run: %w", err)
	}
	if run.Status != "" && run.Status != "draft" && run.Status != "processing" {
		return nil, fmt.Errorf("%w: %s", ErrPayRunWrongStatus, run.Status)
	}
	periodStart, err := parsePayrollDate(run.PayPeriodStart)
	if err != nil {
		return nil, fmt.Errorf("hr: pay_period_start: %w", err)
	}
	periodEnd, err := parsePayrollDate(run.PayPeriodEnd)
	if err != nil {
		return nil, fmt.Errorf("hr: pay_period_end: %w", err)
	}
	if !periodEnd.After(periodStart) && !periodEnd.Equal(periodStart) {
		return nil, errors.New("hr: pay_period_end must be >= pay_period_start")
	}
	runCurrency := strings.ToUpper(run.Currency)
	if runCurrency == "" {
		runCurrency = "USD"
	}

	// ListAll (not List) because HTTP-facing List silently clamps to
	// 500 rows; payroll has to walk every active employee + structure
	// + existing-payslip row for the tenant to stay correct on
	// re-runs and >50-employee tenants.
	employees, err := e.records.ListAll(ctx, tenantID, record.ListFilter{
		KType: KTypeEmployee,
	})
	if err != nil {
		return nil, fmt.Errorf("hr: list employees: %w", err)
	}
	structures, err := e.records.ListAll(ctx, tenantID, record.ListFilter{
		KType: KTypeSalaryStructure,
	})
	if err != nil {
		return nil, fmt.Errorf("hr: list structures: %w", err)
	}
	// Index active structures by employee_id. If an employee has
	// multiple active structures that cover the period we pick the
	// one with the latest effective_from.
	structByEmp := map[string]structureView{}
	for i := range structures {
		var sd structureData
		if err := json.Unmarshal(structures[i].Data, &sd); err != nil {
			continue
		}
		if sd.Status != "" && sd.Status != "active" {
			continue
		}
		effFrom, err := parsePayrollDate(sd.EffectiveFrom)
		if err != nil {
			continue
		}
		if effFrom.After(periodEnd) {
			continue
		}
		if sd.EffectiveUntil != "" {
			effUntil, err := parsePayrollDate(sd.EffectiveUntil)
			if err == nil && effUntil.Before(periodStart) {
				continue
			}
		}
		existing, ok := structByEmp[sd.EmployeeID]
		if ok && !effFrom.After(existing.EffectiveFrom) {
			continue
		}
		structByEmp[sd.EmployeeID] = structureView{
			ID:            structures[i].ID,
			EffectiveFrom: effFrom,
			Data:          sd,
		}
	}

	// Pre-load existing payslips for this run so re-generation is
	// idempotent. ListByField pushes the pay_run_id predicate into
	// SQL — without it we would scan every payslip the tenant has
	// ever produced just to find the small subset belonging to
	// this run.
	existingSlips, err := e.records.ListByField(ctx, tenantID, record.ListFilter{
		KType: KTypePayslip,
	}, "pay_run_id", payRunID.String())
	if err != nil {
		return nil, fmt.Errorf("hr: list payslips: %w", err)
	}
	// Accumulate existing-slip totals in the same pass that builds
	// the coverage set so re-running GeneratePayslips preserves the
	// pay_run's total_gross / total_net rather than zeroing them
	// when every employee is skipped as already-covered.
	coveredEmps := map[string]bool{}
	var existingCount int
	out := &GenerateResult{}
	var totalGross, totalDeductions, totalNet decimal.Decimal

	// Resolve the tenant's tax pack once — every slip in this run
	// shares the same jurisdiction. A resolver error aborts the run:
	// guessing "no withholding" on a transient lookup failure would
	// silently under-deduct statutory tax for every employee.
	pack, err := e.resolveTaxPack(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	period := taxpacks.PayPeriod{Start: periodStart, End: periodEnd}
	taxYear := periodEnd.Year()

	store := e.payrollStore()
	if store == nil {
		return nil, errors.New("hr: payroll engine requires a database pool (call WithPool)")
	}
	// Promote the run header into the typed payroll_runs table so the
	// slips/lines written below have a parent row to FK to. The upsert
	// keeps re-generation safe and refreshes the account codes the
	// posting path reads later.
	if err := store.UpsertRun(ctx, tenantID, RunHeader{
		ID:                       payRunID,
		Name:                     run.Name,
		PeriodStart:              periodStart,
		PeriodEnd:                periodEnd,
		Currency:                 runCurrency,
		Status:                   "processing",
		Department:               run.Department,
		SalaryExpenseAccountCode: run.SalaryExpenseAccountCode,
		SalaryPayableAccountCode: run.SalaryPayableAccountCode,
		DeductionAccountMap:      run.DeductionAccountMap,
		CreatedBy:                actorID,
	}); err != nil {
		return nil, err
	}

	// existingSlips is already narrowed to this pay_run via the
	// ListByField filter above, so we no longer need the in-memory
	// pay_run_id check that the old ListAll path required.
	for _, s := range existingSlips {
		var sd payslipData
		if err := json.Unmarshal(s.Data, &sd); err != nil {
			continue
		}
		coveredEmps[sd.EmployeeID] = true
		existingCount++
		totalGross = totalGross.Add(sd.GrossPay)
		totalDeductions = totalDeductions.Add(sd.TotalDeductions)
		totalNet = totalNet.Add(sd.NetPay)
	}

	for _, emp := range employees {
		var ed employeeData
		if err := json.Unmarshal(emp.Data, &ed); err != nil {
			continue
		}
		if ed.Status != "" && ed.Status != "active" {
			continue
		}
		if run.Department != "" && !strings.EqualFold(ed.Department, run.Department) {
			continue
		}
		empIDStr := emp.ID.String()
		if coveredEmps[empIDStr] {
			out.SkippedExisting++
			continue
		}
		sv, ok := structByEmp[empIDStr]
		if !ok {
			out.SkippedNoStruct++
			continue
		}
		slipCurrency := runCurrency
		if strings.ToUpper(sv.Data.Currency) != "" {
			slipCurrency = strings.ToUpper(sv.Data.Currency)
		}
		// D1 + D2: ordered pipeline over the structure + this run's
		// variable inputs, prorated for a mid-period join date. The
		// statutory tax step (D3) runs inside FinalizePayslip's
		// transaction so it reads the real persisted YTD.
		inputs, err := store.GatherPayInputs(ctx, tenantID, payRunID, emp.ID)
		if err != nil {
			return nil, err
		}
		joinDate, _ := parsePayrollDate(ed.DateOfJoining)
		pipe := buildPipeline(sv.Data, inputs, joinDate, period)

		// computeTax is evaluated by FinalizePayslip with the prior-period
		// cumulative bases. The pack receives the FULL period gross as its
		// `gross` argument (the statutory contribution base) plus the
		// post-pre-tax income-tax base (D2) in TaxableGross, so pre-tax
		// deductions reduce income tax but not social-security contributions.
		monthsYTD := monthsEmployedYTD(joinDate, periodEnd, taxYear)
		empData := ed
		empCurrency := slipCurrency
		empIDLocal := empIDStr
		periodGross := pipe.Gross
		taxableGross := pipe.TaxableGross
		contributionGross := pipe.ContributionGross
		computeTax := func(ytdTaxable, ytdContribution decimal.Decimal) ([]PayslipLine, decimal.Decimal, error) {
			if pack == nil {
				return nil, decimal.Zero, nil
			}
			// The engine always knows all three bases, so it passes them
			// explicitly as non-nil pointers — a zero base (e.g. pre-tax
			// deductions wiping out the income-tax base) is the real value,
			// distinct from "unset". Local copies so each slip's pointers
			// are independent across loop iterations.
			taxableBase := taxableGross
			contributionBase := contributionGross
			ytdTaxableBase := ytdTaxable
			ytdContributionBase := ytdContribution
			info := taxpacks.EmployeeInfo{
				ID:         empIDLocal,
				FilingType: empData.FilingType,
				Allowances: empData.Allowances,
				Resident:   empData.Resident == nil || *empData.Resident, // default resident=true
				HasTFN:     empData.HasTFN == nil || *empData.HasTFN,     // default has_tfn=true

				// Two-base withholding: income tax runs on the income-tax
				// base (post pre-tax), contributions on the contribution
				// base (full gross unless a component reduces it).
				TaxableGross:         &taxableBase,         // period income-tax base
				ContributionGross:    &contributionBase,    // period contribution base
				YTDGross:             ytdTaxableBase,       // cumulative income-tax base
				YTDContributionGross: &ytdContributionBase, // cumulative contribution base
				Currency:             empCurrency,

				// Phase-M2 fields. Each defaults to its zero
				// value; the packs apply their own "most
				// common" fallbacks so pre-Phase-M2
				// KRecords still produce correct slips.
				Canton:        empData.Canton,
				Nationality:   empData.Nationality,
				TaxRegime:     empData.TaxRegime,
				KiwiSaverRate: empData.KiwiSaverRate,
				NumDependents: empData.NumDependents,
				Age:           empData.Age,
				PermitType:    empData.PermitType,

				// Phase-M3 fields (CA + LATAM).
				Province:  empData.Province,
				CPPExempt: empData.CPPExempt,
				EIExempt:  empData.EIExempt,

				// D3: a real, persisted month index derived from the
				// join date + tax year rather than a static field, so
				// the CN cumulative pack and mid-year joiners are correct.
				MonthsEmployedYTD: monthsYTD,
			}
			extra, err := pack.ComputeWithholding(ctx, info, periodGross, period)
			if err != nil {
				return nil, decimal.Zero, fmt.Errorf("hr: tax pack %s: %w", pack.Country(), err)
			}
			lines := make([]PayslipLine, 0, len(extra))
			var total decimal.Decimal
			for _, d := range extra {
				lines = append(lines, PayslipLine{Code: d.Code, Label: d.Name, Amount: d.Amount})
				total = total.Add(d.Amount)
			}
			return lines, total, nil
		}

		// Persist the slip + ordered lines and advance YTD atomically.
		fin, err := store.FinalizePayslip(ctx, FinalizeInput{
			TenantID:          tenantID,
			RunID:             payRunID,
			PayslipID:         uuid.New(),
			EmployeeID:        emp.ID,
			StructureID:       sv.ID,
			Currency:          slipCurrency,
			PeriodStart:       periodStart,
			PeriodEnd:         periodEnd,
			TaxYear:           taxYear,
			Gross:             pipe.Gross,
			TaxableGross:      pipe.TaxableGross,
			ContributionGross: pipe.ContributionGross,
			Earnings:          pipe.Earnings,
			PretaxDeductions:  pipe.PretaxDeductions,
			PosttaxDeductions: pipe.PosttaxDeductions,
			ComputeTax:        computeTax,
		})
		if err != nil {
			return nil, fmt.Errorf("hr: finalize payslip for %s: %w", empIDStr, err)
		}

		// KType compatibility shim: mirror the typed slip into the
		// hr.payslip KRecord so existing readers (HTTP list/get, agent
		// tools, PostPayRun's approval gate) keep working. The KType id is
		// forced equal to the typed payslip id so the two stay linked and
		// PostPayRun can load the typed lines by the same id.
		slipData := map[string]any{
			"pay_run_id":       payRunID.String(),
			"employee_id":      empIDStr,
			"pay_period_start": run.PayPeriodStart,
			"pay_period_end":   run.PayPeriodEnd,
			"structure_id":     sv.ID.String(),
			"currency":         slipCurrency,
			"earnings":         linesToShimJSON(fin.Lines, LineKindEarning),
			"deductions":       linesToShimJSON(fin.Lines, LineKindPretaxDeduction, LineKindTax, LineKindPosttaxDeduction),
			"gross_pay":        decimalFloat(pipe.Gross),
			"taxable_gross":    decimalFloat(pipe.TaxableGross),
			"total_deductions": decimalFloat(fin.TotalEEDeductions),
			"net_pay":          decimalFloat(fin.Net),
			"status":           "draft",
		}
		body, err := json.Marshal(slipData)
		if err != nil {
			return nil, fmt.Errorf("hr: marshal payslip for %s: %w", empIDStr, err)
		}
		created, err := e.records.Create(ctx, record.KRecord{
			ID:        fin.PayslipID,
			TenantID:  tenantID,
			KType:     KTypePayslip,
			Data:      body,
			CreatedBy: actorID,
		})
		if err != nil {
			return nil, fmt.Errorf("hr: create payslip for %s: %w", empIDStr, err)
		}
		out.PayslipIDs = append(out.PayslipIDs, created.ID)
		out.CreatedCount++
		totalGross = totalGross.Add(pipe.Gross)
		totalDeductions = totalDeductions.Add(fin.TotalEEDeductions)
		totalNet = totalNet.Add(fin.Net)
	}

	// Refresh the typed run rollup from the persisted payslips (the source
	// of truth). total_taxable has no KType equivalent, so the typed sum
	// is the only correct source for it.
	tot, err := store.SumRunTotals(ctx, tenantID, payRunID)
	if err != nil {
		return out, err
	}
	runStatus := run.Status
	if runStatus == "" || runStatus == "draft" {
		runStatus = "processing"
	}
	if err := store.UpdateRunTotals(ctx, tenantID, payRunID, tot.Count, tot.Gross, tot.Taxable, tot.EEDeductions, tot.Net, runStatus); err != nil {
		return out, err
	}

	// Roll up totals onto the pay_run and flip status→processing so
	// the UI signals "draft slips are being produced". The existing
	// row version threads through as a compare-and-swap.
	patch := map[string]any{
		"payslip_count": out.CreatedCount + existingCount,
		"total_gross":   decimalFloat(totalGross),
		"total_net":     decimalFloat(totalNet),
	}
	if run.Status == "" || run.Status == "draft" {
		patch["status"] = "processing"
	}
	patchJSON, _ := json.Marshal(patch)
	if _, err := e.records.Update(ctx, record.KRecord{
		ID:        runRec.ID,
		TenantID:  tenantID,
		Version:   runRec.Version,
		Data:      patchJSON,
		UpdatedBy: &actorID,
	}); err != nil {
		return out, fmt.Errorf("hr: patch pay_run totals: %w", err)
	}

	if out.CreatedCount == 0 && out.SkippedExisting == 0 && out.SkippedNoStruct == 0 {
		return out, ErrNoActiveEmployees
	}
	return out, nil
}

// postPayRunMaxRetries bounds the compare-and-swap retry loop on
// the pay_run record patch. Three is enough to absorb a handful of
// concurrent writers while keeping the call bounded.
const postPayRunMaxRetries = 3

// PostPayRun turns every approved payslip for the run into a single
// journal entry: Dr salary expense (gross) + Cr salary payable
// (net) + Cr deduction liabilities (each deduction rolled into
// salary_payable). Sets pay_run.status=paid and patches the JE id
// back onto the pay_run record.
//
// The path is end-to-end idempotent so retries after a partial
// failure converge instead of leaving the run stuck:
//
//   - GetJournalEntryBySource is consulted up front; when a JE
//     already exists for the pay_run the engine reuses it and skips
//     PostJournalEntry entirely. Mirrors ledger/invoice.go's
//     duplicate-reload pattern.
//   - The payslip roll-up accepts both "approved" and "paid" rows
//     when a JE already exists (pure retry path), so totals recompute
//     from the full set of what was previously promoted. A fresh run
//     with zero approved slips still returns ErrNoApprovedSlips.
//   - Slips already at status=paid are skipped in the flip loop.
//   - The pay_run patch is retried on ErrVersionConflict up to
//     postPayRunMaxRetries times. The JE insert is already guarded by
//     the partial unique index on (tenant_id, source_ktype, source_id),
//     so the retry loop only races the record's optimistic version.
func (e *PayrollEngine) PostPayRun(
	ctx context.Context, tenantID, payRunID, actorID uuid.UUID,
) (*ledger.JournalEntry, error) {
	if e.ledger == nil {
		return nil, errors.New("hr: payroll engine ledger store nil")
	}

	runRec, err := e.records.Get(ctx, tenantID, payRunID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPayRunNotFound, err)
	}
	if runRec.KType != KTypePayRun {
		return nil, fmt.Errorf("%w: %s is %s", ErrPayRunNotFound, payRunID, runRec.KType)
	}
	var run payRunData
	if err := json.Unmarshal(runRec.Data, &run); err != nil {
		return nil, fmt.Errorf("hr: decode pay_run: %w", err)
	}
	if run.SalaryExpenseAccountCode == "" || run.SalaryPayableAccountCode == "" {
		return nil, ErrMissingAccounts
	}
	currency := strings.ToUpper(run.Currency)
	if currency == "" {
		currency = "USD"
	}
	// Typed-table store (may be nil for a pool-less engine). When present
	// the journal entry is sourced from the persisted typed lines and the
	// typed run/slip status is synced to paid after posting.
	store := e.payrollStore()

	// Fast-check: does a JE already exist for this pay_run? If
	// so, this call is a retry of a previous attempt that committed
	// the JE (and possibly flipped some slips) but failed before
	// the pay_run patch landed. Reuse the entry so the partial
	// state can converge rather than trip ErrNoApprovedSlips on
	// the retry.
	existingJE, err := e.ledger.GetJournalEntryBySource(ctx, tenantID, KTypePayRun, payRunID)
	if err != nil && !errors.Is(err, ledger.ErrEntryNotFound) {
		return nil, fmt.Errorf("hr: lookup pay_run je: %w", err)
	}
	if run.Status == "paid" && existingJE != nil {
		// Run already fully paid — return the JE as a no-op so
		// the HTTP caller gets an idempotent 200.
		return existingJE, nil
	}
	if run.Status == "paid" && existingJE == nil {
		// Legacy path: status=paid with no JE linked should not
		// happen, but keep the old error contract rather than
		// silently re-post.
		return nil, fmt.Errorf("%w: already paid", ErrPayRunWrongStatus)
	}

	// ListByField (not ListAll): push the pay_run_id filter down
	// into SQL so we only scan slips for THIS pay_run, not every
	// payslip the tenant has ever produced. On a fresh tenant the
	// difference is small; on a multi-year payroll history it
	// reduces the materialised set from O(all payslips) to
	// O(slips for this run), bounded by employees-per-run.
	// HTTP-facing List would silently cap at 500 rows; ListByField
	// has the same ListAllMaxRows safety cap as ListAll, which is
	// vastly larger than any realistic single pay_run population.
	slips, err := e.records.ListByField(ctx, tenantID, record.ListFilter{
		KType: KTypePayslip,
	}, "pay_run_id", payRunID.String())
	if err != nil {
		return nil, fmt.Errorf("hr: list payslips: %w", err)
	}
	// On a fresh run only "approved" slips are in scope. On the
	// retry path (JE already exists) previously-flipped "paid"
	// slips also roll up into the totals — otherwise a partial
	// success would under-report gross/net after retry. The
	// pay_run_id filter already happened in SQL above; here we
	// only narrow by status.
	var approved []record.KRecord
	var gross, deductions, net decimal.Decimal
	// perCodeDeductions sums each statutory deduction code
	// across every approved slip in the run. PostPayRun consults
	// DeductionAccountMap below to decide whether to credit the
	// per-code total to a dedicated liability account or fall
	// back to the catch-all salary_payable roll-up.
	//
	// Amounts are accumulated *with sign* so the invariant
	//   Σ perCodeDeductions[c]  ==  Σ sd.TotalDeductions
	// holds across every slip whose deduction lines all carry a
	// non-empty Code. Empty-coded deduction lines (rare; usually
	// ad-hoc adjustments) are not tracked per-code — they fall
	// through into the `unmapped` rollover below, which then
	// credits the catch-all salary_payable line for the
	// difference. Negative deduction amounts (legitimate when a
	// salary structure includes a credit-style component via
	// rollStructure) are kept in the aggregate so the per-code
	// total reflects the net effect; splitDeductionsByCode
	// preserves the sign and PostPayRun emits a Cr line for
	// positive aggregates and a Dr line for negative ones.
	perCodeDeductions := map[string]decimal.Decimal{}
	for _, s := range slips {
		var sd payslipData
		if err := json.Unmarshal(s.Data, &sd); err != nil {
			continue
		}
		if sd.Status != "approved" && (existingJE == nil || sd.Status != "paid") {
			continue
		}
		approved = append(approved, s)
		// Prefer the persisted typed lines as the posting source. The
		// slip's KType id equals the typed payslip id (forced at
		// generation), so we load by the same id. Legacy slips with no
		// typed lines fall back to the KType rollup fields.
		if store != nil {
			tl, lerr := store.GetPayslipLines(ctx, tenantID, s.ID)
			if lerr != nil {
				return nil, fmt.Errorf("hr: load typed lines for %s: %w", s.ID, lerr)
			}
			if len(tl) > 0 {
				g, d, n, pc := rollupTypedLines(tl)
				gross = gross.Add(g)
				deductions = deductions.Add(d)
				net = net.Add(n)
				for code, amt := range pc {
					perCodeDeductions[code] = perCodeDeductions[code].Add(amt)
				}
				continue
			}
		}
		gross = gross.Add(sd.GrossPay)
		deductions = deductions.Add(sd.TotalDeductions)
		net = net.Add(sd.NetPay)
		for _, d := range sd.Deductions {
			if d.Code == "" {
				continue
			}
			perCodeDeductions[d.Code] = perCodeDeductions[d.Code].Add(d.Amount)
		}
	}
	if len(approved) == 0 && existingJE == nil {
		return nil, ErrNoApprovedSlips
	}

	entry := existingJE
	if entry == nil {
		postedAt := e.now().UTC()
		lines := buildPayrollJournalLines(payrollJournalInput{
			Gross:                    gross,
			Net:                      net,
			Deductions:               deductions,
			PerCodeDeductions:        perCodeDeductions,
			DeductionAccountMap:      run.DeductionAccountMap,
			SalaryExpenseAccountCode: run.SalaryExpenseAccountCode,
			SalaryPayableAccountCode: run.SalaryPayableAccountCode,
			Currency:                 currency,
		})
		sourceID := payRunID
		posted, postErr := e.ledger.PostJournalEntry(ctx, ledger.JournalEntry{
			TenantID:    tenantID,
			PostedAt:    postedAt,
			Memo:        fmt.Sprintf("Payroll run %s", run.Name),
			SourceKType: KTypePayRun,
			SourceID:    &sourceID,
			CreatedBy:   actorID,
			Lines:       lines,
		})
		if postErr != nil {
			if errors.Is(postErr, ledger.ErrDuplicateSourceEntry) {
				// Lost the race with a concurrent poster; reload and proceed.
				reloaded, reloadErr := e.ledger.GetJournalEntryBySource(ctx, tenantID, KTypePayRun, payRunID)
				if reloadErr != nil {
					return nil, fmt.Errorf("hr: reload duplicate pay_run JE: %w", reloadErr)
				}
				posted = reloaded
			} else {
				return nil, fmt.Errorf("hr: post pay_run je: %w", postErr)
			}
		}
		entry = posted
	}

	// Flip each in-scope slip → paid and patch its JE id. Slips
	// already at status=paid are skipped so re-runs don't bump
	// their version needlessly.
	for _, s := range approved {
		var sd payslipData
		if err := json.Unmarshal(s.Data, &sd); err == nil && sd.Status == "paid" {
			continue
		}
		body, _ := json.Marshal(map[string]any{
			"status":           "paid",
			"journal_entry_id": entry.ID.String(),
		})
		if _, err := e.records.Update(ctx, record.KRecord{
			ID:        s.ID,
			TenantID:  tenantID,
			Version:   s.Version,
			Data:      body,
			UpdatedBy: &actorID,
		}); err != nil {
			return entry, fmt.Errorf("hr: mark payslip %s paid: %w", s.ID, err)
		}
	}

	// Sync the typed run + slips to paid and link the JE. Only the slips
	// that were actually posted (the approved set, whose ids match the
	// typed payslip ids) are flipped, so a partially-approved run leaves
	// its still-draft typed slips untouched. Idempotent (only flips rows
	// not already paid), so retries converge.
	if store != nil {
		paidIDs := make([]uuid.UUID, 0, len(approved))
		for i := range approved {
			paidIDs = append(paidIDs, approved[i].ID)
		}
		if err := store.MarkRunPosted(ctx, tenantID, payRunID, entry.ID, paidIDs); err != nil {
			return entry, fmt.Errorf("hr: sync typed payroll status: %w", err)
		}
	}

	// Flip the pay_run → paid with a CAS retry loop. The JE and
	// slip writes are committed by this point; the only remaining
	// failure mode is a concurrent patch to the pay_run record
	// bumping its version. Re-read up front (a concurrent
	// GeneratePayslips or other patch may have bumped the version
	// between our initial Get and now), then re-read + re-patch up
	// to postPayRunMaxRetries times before surfacing the conflict.
	runPatch, _ := json.Marshal(map[string]any{
		"status":           "paid",
		"journal_entry_id": entry.ID.String(),
		"payslip_count":    len(approved),
		"total_gross":      decimalFloat(gross),
		"total_net":        decimalFloat(net),
	})
	currentRun, err := e.records.Get(ctx, tenantID, payRunID)
	if err != nil {
		return entry, fmt.Errorf("hr: reload pay_run before patch: %w", err)
	}
	for attempt := 0; attempt < postPayRunMaxRetries; attempt++ {
		if _, err := e.records.Update(ctx, record.KRecord{
			ID:        currentRun.ID,
			TenantID:  tenantID,
			Version:   currentRun.Version,
			Data:      runPatch,
			UpdatedBy: &actorID,
		}); err != nil {
			if errors.Is(err, record.ErrVersionConflict) && attempt+1 < postPayRunMaxRetries {
				reloaded, reloadErr := e.records.Get(ctx, tenantID, payRunID)
				if reloadErr != nil {
					return entry, fmt.Errorf("hr: reload pay_run after conflict: %w", reloadErr)
				}
				currentRun = reloaded
				continue
			}
			return entry, fmt.Errorf("hr: patch pay_run paid: %w", err)
		}
		return entry, nil
	}
	return entry, fmt.Errorf("hr: patch pay_run paid: exceeded %d retries", postPayRunMaxRetries)
}

// ListPayslipsForRun returns every payslip KRecord whose data
// pay_run_id matches the given run. Unlike the generic records
// list route — which the HTTP layer caps at 500 rows and defaults
// to 50 — this pushes the pay_run_id filter into SQL via
// PGStore.ListByField, so the frontend's "View slips" panel never
// silently drops results on tenants with more than 50 payslips
// across all pay_runs.
//
// Returns slips in the same relative order as ListAll /
// ListByField (most recently updated first) so the UI gets a
// stable-enough ordering without the store having to sort by
// pay_period.
func (e *PayrollEngine) ListPayslipsForRun(
	ctx context.Context, tenantID, payRunID uuid.UUID,
) ([]record.KRecord, error) {
	if e.records == nil {
		return nil, errors.New("hr: payroll engine records store nil")
	}
	if tenantID == uuid.Nil || payRunID == uuid.Nil {
		return nil, errors.New("hr: tenant_id and pay_run_id required")
	}
	slips, err := e.records.ListByField(ctx, tenantID, record.ListFilter{
		KType: KTypePayslip,
	}, "pay_run_id", payRunID.String())
	if err != nil {
		return nil, fmt.Errorf("hr: list payslips: %w", err)
	}
	return slips, nil
}

// deductionSplit is one (code, account, amount) row emitted by
// splitDeductionsByCode. Carried as a slice (not a map) so the
// resulting journal lines have a deterministic order — codes are
// sorted ASC by code before splitting so the same pay_run posted
// twice produces the same line ordering regardless of map
// iteration randomness, which keeps audit diffs stable.
type deductionSplit struct {
	code    string
	account string
	amount  decimal.Decimal
}

// payrollJournalInput bundles the inputs buildPayrollJournalLines
// needs to assemble a balanced payroll journal entry. Keeping the
// helper signature as a struct (rather than a long parameter list)
// makes it easy to extend later — e.g. adding per-employee
// employer-contribution lines for KiwiSaver or EPF without
// disturbing the call site.
type payrollJournalInput struct {
	Gross                    decimal.Decimal
	Net                      decimal.Decimal
	Deductions               decimal.Decimal
	PerCodeDeductions        map[string]decimal.Decimal
	DeductionAccountMap      map[string]string
	SalaryExpenseAccountCode string
	SalaryPayableAccountCode string
	Currency                 string
}

// buildPayrollJournalLines assembles a balanced double-entry
// journal for a payroll run. The base shape is:
//
//	Dr SalaryExpense    gross
//	  Cr SalaryPayable    net
//	  Cr <per-code liability accounts>  (mapped codes, positive)
//	  Cr SalaryPayable    unmapped remainder (if > 0)
//	  Dr <per-code liability accounts>  (mapped codes, negative)
//	  Dr SalaryPayable    |unmapped|    (if < 0)
//
// Balance invariant — proved by TestBuildPayrollJournalLines_*:
//
//	Σ Debit  ==  Σ Credit  ==  gross
//
// regardless of which codes are mapped, whether any aggregate is
// negative, or how many empty-coded ad-hoc lines were present
// (those don't appear in PerCodeDeductions but are absorbed into
// the salary_payable catch-all via the signed `unmapped`).
func buildPayrollJournalLines(in payrollJournalInput) []ledger.JournalLine {
	lines := []ledger.JournalLine{
		{AccountCode: in.SalaryExpenseAccountCode, Debit: in.Gross, Credit: decimal.Zero, Currency: in.Currency, Memo: "Payroll expense"},
		{AccountCode: in.SalaryPayableAccountCode, Debit: decimal.Zero, Credit: in.Net, Currency: in.Currency, Memo: "Net payable"},
	}
	if in.Deductions.IsZero() {
		return lines
	}
	mappedSplits := splitDeductionsByCode(in.PerCodeDeductions, in.DeductionAccountMap)
	unmapped := in.Deductions
	for _, ms := range mappedSplits {
		unmapped = unmapped.Sub(ms.amount)
		line := ledger.JournalLine{
			AccountCode: ms.account,
			Currency:    in.Currency,
			Memo:        fmt.Sprintf("Deductions payable: %s", ms.code),
		}
		if ms.amount.IsNegative() {
			line.Debit = ms.amount.Neg()
			line.Credit = decimal.Zero
		} else {
			line.Debit = decimal.Zero
			line.Credit = ms.amount
		}
		lines = append(lines, line)
	}
	if unmapped.IsPositive() {
		lines = append(lines, ledger.JournalLine{
			AccountCode: in.SalaryPayableAccountCode, Debit: decimal.Zero, Credit: unmapped, Currency: in.Currency, Memo: "Deductions payable",
		})
	} else if unmapped.IsNegative() {
		lines = append(lines, ledger.JournalLine{
			AccountCode: in.SalaryPayableAccountCode, Debit: unmapped.Neg(), Credit: decimal.Zero, Currency: in.Currency, Memo: "Deductions payable",
		})
	}
	return lines
}

// splitDeductionsByCode resolves the (Deduction.Code → liability
// account code) mapping for every code present in the slip
// rollups. Codes absent from accountMap are excluded from the
// returned slice — the caller's `unmapped` balance picks them up
// and posts to salary_payable so the journal entry stays balanced.
// A nil / empty accountMap returns an empty slice, which makes
// PostPayRun's deduction-split branch a no-op and the journal
// entry shape identical to the pre-Phase-M2 catch-all behaviour.
//
// Amounts are returned *with sign* so PostPayRun can emit a Cr
// line for positive aggregates and a Dr line for negative ones —
// the latter is reached only when a slip carried a credit-style
// (negative-amount) deduction component for the same code. Zero
// aggregates are filtered out so the journal entry doesn't carry
// cosmetic empty lines, and a blank `accountMap[code]` is treated
// as "no mapping" so a misconfigured tenant doesn't end up
// posting to account "".
func splitDeductionsByCode(perCode map[string]decimal.Decimal, accountMap map[string]string) []deductionSplit {
	if len(perCode) == 0 || len(accountMap) == 0 {
		return nil
	}
	codes := make([]string, 0, len(perCode))
	for c := range perCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	out := make([]deductionSplit, 0, len(codes))
	for _, c := range codes {
		account, ok := accountMap[c]
		if !ok || account == "" {
			continue
		}
		amt := perCode[c]
		if amt.IsZero() {
			continue
		}
		out = append(out, deductionSplit{code: c, account: account, amount: amt})
	}
	return out
}

// parsePayrollDate accepts the canonical `YYYY-MM-DD` pay-period
// format plus RFC3339 so callers authoring the pay_run via agent
// tools with `time.Now().Format(time.RFC3339)` also work.
func parsePayrollDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty date")
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unparsable date %q", s)
}

// Internal projections.

type payRunData struct {
	Name                     string          `json:"name"`
	PayPeriodStart           string          `json:"pay_period_start"`
	PayPeriodEnd             string          `json:"pay_period_end"`
	Department               string          `json:"department"`
	Currency                 string          `json:"currency"`
	PayslipCount             int             `json:"payslip_count"`
	TotalGross               decimal.Decimal `json:"total_gross"`
	TotalNet                 decimal.Decimal `json:"total_net"`
	SalaryExpenseAccountCode string          `json:"salary_expense_account_code"`
	SalaryPayableAccountCode string          `json:"salary_payable_account_code"`
	JournalEntryID           string          `json:"journal_entry_id"`
	Status                   string          `json:"status"`

	// DeductionAccountMap optionally maps a statutory
	// Deduction.Code (e.g. "MY_EPF", "SG_CPF_EMPLOYEE",
	// "SA_GOSI_PENSION") to a distinct liability account on the
	// chart so the journal entry splits employee withholdings by
	// remittance authority instead of rolling everything into the
	// catch-all `salary_payable` line. Required for real-world
	// compliance: EPF goes to KWSP, CPF goes to CPF Board, GOSI
	// goes to GOSI, etc., and finance teams need separate
	// liability balances to reconcile each remittance run.
	//
	// Codes not present in the map fall back to
	// SalaryPayableAccountCode — exactly the legacy roll-up
	// behaviour, so a tenant that hasn't configured per-code
	// accounts keeps producing the same journal entry shape it
	// did before this field existed.
	//
	// JSON shape on the KRecord:
	//   {"deduction_account_map": {"MY_EPF": "2305", ...}}
	//
	// Empty / nil means "roll up everything into salary_payable"
	// for backward compatibility with pre-Phase-M2 pay_runs.
	DeductionAccountMap map[string]string `json:"deduction_account_map,omitempty"`
}

type payslipData struct {
	PayRunID        string          `json:"pay_run_id"`
	EmployeeID      string          `json:"employee_id"`
	PayPeriodStart  string          `json:"pay_period_start"`
	PayPeriodEnd    string          `json:"pay_period_end"`
	StructureID     string          `json:"structure_id"`
	Currency        string          `json:"currency"`
	GrossPay        decimal.Decimal `json:"gross_pay"`
	TotalDeductions decimal.Decimal `json:"total_deductions"`
	NetPay          decimal.Decimal `json:"net_pay"`
	JournalEntryID  string          `json:"journal_entry_id"`
	Status          string          `json:"status"`
	// Deductions mirrors the slip's `deductions` array as
	// (code, amount) pairs so PostPayRun can split the journal
	// entry by statutory deduction code instead of rolling
	// every deduction into salary_payable. The lines are
	// already persisted on the slip — this projection just
	// surfaces them for the posting path. `omitempty` so
	// legacy slips that came through before this projection
	// existed still decode without errors.
	//
	// Wire-format coupling: this field reads from the same
	// `deductions` JSON array written by `linesToShimJSON` (see
	// the slip generation path, search for `"deductions"`).
	// `linesToShimJSON` emits a `map[string]any` per line with
	// keys `code`, `name`, `amount`; `deductionLine` declares
	// only `Code` and `Amount` so Go's json.Unmarshal silently
	// drops the extra keys. Amounts round-trip cleanly because
	// the writer uses `decimalFloat()` and decimal.Decimal parses
	// both float64 and string JSON numbers. If either side of
	// this contract ever drifts (e.g. linesToShimJSON renames a
	// key, or deductionLine adds a tag that conflicts), the
	// PostPayRun deduction-split path silently loses data, so
	// the symmetric tests in payroll_engine_*_test.go pin the
	// round-trip.
	Deductions []deductionLine `json:"deductions,omitempty"`
}

// deductionLine is the minimal projection of a slip deduction
// row used by PostPayRun's per-code roll-up. Code matches the
// canonical Deduction.Code emitted by the tax pack (e.g.
// "MY_EPF", "SG_CPF_EMPLOYEE", "FICA_OASDI") so the
// DeductionAccountMap lookup uses the same key the pack writes.
type deductionLine struct {
	Code   string          `json:"code"`
	Amount decimal.Decimal `json:"amount"`
}

type employeeData struct {
	Status     string `json:"status"`
	Department string `json:"department"`
	// DateOfJoining drives D1 proration: when it falls inside the pay
	// period the recurring earnings are scaled to the employed fraction
	// of the period. Empty / unparsable → no proration (full period).
	DateOfJoining string `json:"date_of_joining,omitempty"`
	// Tax-pack inputs. Optional; pre-Phase-M employee KRecords
	// don't carry these and the packs degrade gracefully.
	FilingType string          `json:"filing_type,omitempty"`
	Allowances int             `json:"allowances,omitempty"`
	Resident   *bool           `json:"resident,omitempty"`
	HasTFN     *bool           `json:"has_tfn,omitempty"`
	YTDGross   decimal.Decimal `json:"ytd_gross,omitempty"`

	// Phase-M2 jurisdiction-specific inputs. Every field is
	// `omitempty` so a pre-Phase-M2 KRecord serialises back
	// identically after a round-trip. Packs that don't care
	// about a field simply ignore it (e.g. the US pack never
	// reads Canton).
	Canton        string          `json:"canton,omitempty"`
	Nationality   string          `json:"nationality,omitempty"`
	TaxRegime     string          `json:"tax_regime,omitempty"`
	KiwiSaverRate decimal.Decimal `json:"kiwisaver_rate,omitempty"`
	NumDependents int             `json:"num_dependents,omitempty"`
	Age           int             `json:"age,omitempty"`
	PermitType    string          `json:"permit_type,omitempty"`

	// Phase-M3 (CA + LATAM) inputs. Same `omitempty` contract
	// as the Phase-M2 block above: packs that don't read these
	// fields ignore them, pre-Phase-M3 KRecords serialise back
	// identically, and the CA pack defaults missing Province
	// to federal-only computation rather than crashing.
	Province  string `json:"province,omitempty"`
	CPPExempt bool   `json:"cpp_exempt,omitempty"`
	EIExempt  bool   `json:"ei_exempt,omitempty"`

	// Cumulative-withholding input. Number of months the employee
	// has received employment income this calendar year, including
	// the slip's own month. Read by the CN pack for its 累计预扣预缴
	// month index; `omitempty` keeps pre-existing KRecords intact
	// and the pack falls back to the pay-period end month when 0.
	MonthsEmployedYTD int `json:"months_employed_ytd,omitempty"`
}

type structureData struct {
	EmployeeID       string               `json:"employee_id"`
	EffectiveFrom    string               `json:"effective_from"`
	EffectiveUntil   string               `json:"effective_until"`
	Currency         string               `json:"currency"`
	BaseSalary       decimal.Decimal      `json:"base_salary"`
	PaymentFrequency string               `json:"payment_frequency"`
	Components       []structureComponent `json:"components"`
	Status           string               `json:"status"`
}

type structureComponent struct {
	ComponentID        string          `json:"component_id"`
	Code               string          `json:"code"`
	Name               string          `json:"name"`
	Type               string          `json:"type"`
	Amount             decimal.Decimal `json:"amount"`
	AmountType         string          `json:"amount_type"`
	OverrideAmount     decimal.Decimal `json:"override_amount"`
	OverrideAmountType string          `json:"override_amount_type"`

	// Taxable marks whether an EARNING component contributes to the
	// statutory taxable base. nil → true (the salary_component default),
	// so pre-D2 structures keep their existing behaviour where every
	// earning is taxable. Ignored for deduction components.
	Taxable *bool `json:"taxable,omitempty"`
	// PreTax marks a DEDUCTION component as salary-sacrifice / pre-tax:
	// it reduces taxable_gross BEFORE statutory withholding (D2). Absent
	// → false (post-tax), so pre-D2 structures keep applying their
	// deductions after tax exactly as before. Ignored for earnings.
	PreTax bool `json:"pre_tax,omitempty"`
	// PreTaxReducesContributionBase additionally lets a PreTax deduction
	// reduce the social-security / contribution base, not just the
	// income-tax base. Default false models the common 401(k) case
	// (pre-tax for income tax but FICA still runs on the full gross);
	// true models a US Section-125 cafeteria plan (pre-tax for both).
	// Ignored when PreTax is false or for earning components.
	PreTaxReducesContributionBase bool `json:"pretax_reduces_contribution_base,omitempty"`
}

type structureView struct {
	ID            uuid.UUID
	EffectiveFrom time.Time
	Data          structureData
}

// decimalFloat collapses a decimal to a float64 so the surrounding
// JSON is emitted as a JSON number. The KRecord schema validator
// rejects strings for number-typed fields.
func decimalFloat(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

// linesToShimJSON renders persisted typed payslip lines of the given
// kind(s) into the KType payslip's earnings/deductions arrays, matching
// linesToJSON's shape (code / name / amount as a JSON number) so existing
// KType readers and PostPayRun's per-code split keep working unchanged.
func linesToShimJSON(lines []PayslipLine, kinds ...string) []map[string]any {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		if !want[l.Kind] {
			continue
		}
		out = append(out, map[string]any{
			"code":   l.Code,
			"name":   l.Label,
			"amount": decimalFloat(l.Amount),
		})
	}
	return out
}

// rollupTypedLines reduces persisted typed payslip lines into the
// posting totals: gross = Σ earnings; deductions = Σ (pretax + tax +
// posttax); net = gross − deductions; and the per-code deduction map the
// journal builder splits across liability accounts. This is how
// PostPayRun sources its journal entry from the persisted lines rather
// than recomputing from the KType shim.
func rollupTypedLines(lines []PayslipLine) (gross, deductions, net decimal.Decimal, perCode map[string]decimal.Decimal) {
	perCode = map[string]decimal.Decimal{}
	for _, l := range lines {
		switch l.Kind {
		case LineKindEarning:
			gross = gross.Add(l.Amount)
		case LineKindPretaxDeduction, LineKindTax, LineKindPosttaxDeduction:
			deductions = deductions.Add(l.Amount)
			if l.Code != "" {
				perCode[l.Code] = perCode[l.Code].Add(l.Amount)
			}
		}
	}
	net = gross.Sub(deductions)
	return gross, deductions, net, perCode
}
