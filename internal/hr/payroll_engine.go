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
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/hr/taxpacks"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// CountryResolver returns the ISO 3166-1 alpha-2 country code for a
// tenant. Implemented by *tenant.PGStore via a thin adapter so the
// hr package doesn't import tenant directly (and keeps the existing
// no-cycle invariant). Empty + ErrNoPack are treated identically by
// the engine — the slip simply skips statutory withholding.
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
}

// NewPayrollEngine binds the engine to a record store. Pass the
// ledger store to enable PostPayRun.
func NewPayrollEngine(records *record.PGStore, ledgerStore *ledger.PGStore) *PayrollEngine {
	return &PayrollEngine{records: records, ledger: ledgerStore, now: time.Now}
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
// pre-Phase-M behaviour); resolvers that return "" or
// taxpacks.ErrNoPack also fall back to the no-pack code path.
func (e *PayrollEngine) WithCountryResolver(r CountryResolver) *PayrollEngine {
	e.resolver = r
	return e
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
		return nil, fmt.Errorf("%w: %s", ErrPayRunNotFound, err)
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
	// shares the same jurisdiction. Failures here fail-soft: the
	// engine logs nothing (no logger plumbed) and the slips run
	// without statutory deductions, which matches the pre-Phase-M
	// behaviour for tenants without a country code.
	var pack taxpacks.TaxPack
	if e.resolver != nil {
		country, err := e.resolver(ctx, tenantID)
		if err == nil && country != "" {
			if p, err := taxpacks.Lookup(country); err == nil {
				pack = p
			}
		}
	}
	period := taxpacks.PayPeriod{Start: periodStart, End: periodEnd}
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
		earnings, deductions, gross, deduct, net := rollStructure(sv.Data, slipCurrency)

		// Statutory tax-pack deductions are appended after the
		// structure-driven lines so the slip's `deductions` array
		// keeps a stable ordering: structure components first,
		// then federal / FICA / PAYG. Engine totals are
		// recomputed below so the rollup catches the new lines.
		if pack != nil {
			info := taxpacks.EmployeeInfo{
				ID:         empIDStr,
				FilingType: ed.FilingType,
				Allowances: ed.Allowances,
				Resident:   ed.Resident == nil || *ed.Resident, // default resident=true
				HasTFN:     ed.HasTFN == nil || *ed.HasTFN,     // default has_tfn=true
				YTDGross:   ed.YTDGross,
				Currency:   slipCurrency,

				// Phase-M2 fields. Each defaults to its zero
				// value; the packs apply their own "most
				// common" fallbacks so pre-Phase-M2
				// KRecords still produce correct slips:
				//   - CH pack: empty Canton → federal-only.
				//   - GCC packs: empty Nationality → "expat"
				//     (no employee SS withholding).
				//   - IN pack: empty TaxRegime → "new".
				//   - NZ pack: zero KiwiSaverRate → no KS line.
				//   - SG pack: zero Age → treat as ≤55 tier.
				Canton:        ed.Canton,
				Nationality:   ed.Nationality,
				TaxRegime:     ed.TaxRegime,
				KiwiSaverRate: ed.KiwiSaverRate,
				NumDependents: ed.NumDependents,
				Age:           ed.Age,
				PermitType:    ed.PermitType,
			}
			extraLines, err := pack.ComputeWithholding(ctx, info, gross, period)
			if err != nil {
				return nil, fmt.Errorf("hr: tax pack %s: %w", pack.Country(), err)
			}
			for _, d := range extraLines {
				deductions = append(deductions, lineOut{
					Code:   d.Code,
					Name:   d.Name,
					Amount: d.Amount,
				})
				deduct = deduct.Add(d.Amount)
			}
			net = gross.Sub(deduct)
		}
		slipData := map[string]any{
			"pay_run_id":       payRunID.String(),
			"employee_id":      empIDStr,
			"pay_period_start": run.PayPeriodStart,
			"pay_period_end":   run.PayPeriodEnd,
			"structure_id":     sv.ID.String(),
			"currency":         slipCurrency,
			"earnings":         linesToJSON(earnings),
			"deductions":       linesToJSON(deductions),
			"gross_pay":        decimalFloat(gross),
			"total_deductions": decimalFloat(deduct),
			"net_pay":          decimalFloat(net),
			"status":           "draft",
		}
		body, err := json.Marshal(slipData)
		if err != nil {
			return nil, fmt.Errorf("hr: marshal payslip for %s: %w", empIDStr, err)
		}
		created, err := e.records.Create(ctx, record.KRecord{
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
		totalGross = totalGross.Add(gross)
		totalDeductions = totalDeductions.Add(deduct)
		totalNet = totalNet.Add(net)
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
		return nil, fmt.Errorf("%w: %s", ErrPayRunNotFound, err)
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
	// back to the catch-all salary_payable roll-up. Codes only
	// appear in the map if at least one slip carried a positive
	// deduction line for them, so the journal entry never emits
	// zero-value lines.
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
		gross = gross.Add(sd.GrossPay)
		deductions = deductions.Add(sd.TotalDeductions)
		net = net.Add(sd.NetPay)
		for _, d := range sd.Deductions {
			if d.Code == "" || !d.Amount.IsPositive() {
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
		lines := []ledger.JournalLine{
			{AccountCode: run.SalaryExpenseAccountCode, Debit: gross, Credit: decimal.Zero, Currency: currency, Memo: "Payroll expense"},
			{AccountCode: run.SalaryPayableAccountCode, Debit: decimal.Zero, Credit: net, Currency: currency, Memo: "Net payable"},
		}
		if deductions.IsPositive() {
			// Split deductions by code when run.DeductionAccountMap is
			// configured: each mapped code credits its own liability
			// account so finance can reconcile remittances per
			// authority (EPF→KWSP, CPF→CPF Board, GOSI→GOSI, …).
			// Unmapped codes roll up into the catch-all salary_payable
			// line so the entry stays balanced regardless of which
			// codes the tenant has explicitly configured.
			//
			// The sum across `mappedSplits + unmapped` must equal
			// `deductions` (and therefore `gross - net`) exactly,
			// otherwise the entry won't balance and ledger.Post will
			// reject it. perCodeDeductions sums to the same total as
			// `deductions` by construction (both are the sum of every
			// positive deduction line on every approved slip), so the
			// arithmetic is closed-form correct as long as we use
			// `decimal.Decimal` (not float) for the splits.
			mappedSplits := splitDeductionsByCode(perCodeDeductions, run.DeductionAccountMap)
			unmapped := deductions
			for _, ms := range mappedSplits {
				unmapped = unmapped.Sub(ms.amount)
				lines = append(lines, ledger.JournalLine{
					AccountCode: ms.account,
					Debit:       decimal.Zero,
					Credit:      ms.amount,
					Currency:    currency,
					Memo:        fmt.Sprintf("Deductions payable: %s", ms.code),
				})
			}
			if unmapped.IsPositive() {
				lines = append(lines, ledger.JournalLine{
					AccountCode: run.SalaryPayableAccountCode, Debit: decimal.Zero, Credit: unmapped, Currency: currency, Memo: "Deductions payable",
				})
			}
		}
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

// splitDeductionsByCode resolves the (Deduction.Code → liability
// account code) mapping for every code present in the slip
// rollups. Codes absent from accountMap are excluded from the
// returned slice — the caller's `unmapped` balance picks them up
// and credits salary_payable so the journal entry stays balanced.
// A nil / empty accountMap returns an empty slice, which makes
// PostPayRun's deduction-split branch a no-op and the journal
// entry shape identical to the pre-Phase-M2 catch-all behaviour.
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
		if !amt.IsPositive() {
			continue
		}
		out = append(out, deductionSplit{code: c, account: account, amount: amt})
	}
	return out
}

// rollStructure expands a salary_structure's components into
// resolved earnings/deductions lines and returns gross/deductions/net.
// Percentage components are resolved against base_salary; fixed
// components pass through. Component overrides on the structure are
// honoured — when an override amount is present it replaces the
// catalog amount.
func rollStructure(sv structureData, _ string) ([]lineOut, []lineOut, decimal.Decimal, decimal.Decimal, decimal.Decimal) {
	base := sv.BaseSalary
	var earnings, deductions []lineOut
	// The base salary itself is the canonical earning line so a
	// structure with no components still produces a sensible slip.
	if base.IsPositive() {
		earnings = append(earnings, lineOut{
			ComponentID: "",
			Code:        "BASE",
			Name:        "Base Salary",
			Amount:      base,
		})
	}
	for _, c := range sv.Components {
		amt := c.OverrideAmount
		if !amt.IsPositive() {
			amt = c.Amount
		}
		amountType := c.OverrideAmountType
		if amountType == "" {
			amountType = c.AmountType
		}
		if amountType == "percentage" {
			amt = base.Mul(amt).Div(decimal.NewFromInt(100)).Round(2)
		}
		line := lineOut{
			ComponentID: c.ComponentID,
			Code:        c.Code,
			Name:        c.Name,
			Amount:      amt,
		}
		switch c.Type {
		case "deduction":
			deductions = append(deductions, line)
		default:
			earnings = append(earnings, line)
		}
	}
	var gross, deduct decimal.Decimal
	for _, e := range earnings {
		gross = gross.Add(e.Amount)
	}
	for _, d := range deductions {
		deduct = deduct.Add(d.Amount)
	}
	net := gross.Sub(deduct)
	return earnings, deductions, gross, deduct, net
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
}

type structureView struct {
	ID            uuid.UUID
	EffectiveFrom time.Time
	Data          structureData
}

type lineOut struct {
	ComponentID string          `json:"component_id,omitempty"`
	Code        string          `json:"code"`
	Name        string          `json:"name"`
	Amount      decimal.Decimal `json:"amount"`
}

// decimalFloat collapses a decimal to a float64 so the surrounding
// JSON is emitted as a JSON number. The KRecord schema validator
// rejects strings for number-typed fields.
func decimalFloat(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

// linesToJSON renders a list of resolved component lines with
// `amount` as a JSON number (not a quoted decimal string).
func linesToJSON(ls []lineOut) []map[string]any {
	out := make([]map[string]any, 0, len(ls))
	for _, l := range ls {
		row := map[string]any{
			"code":   l.Code,
			"name":   l.Name,
			"amount": decimalFloat(l.Amount),
		}
		if l.ComponentID != "" {
			row["component_id"] = l.ComponentID
		}
		out = append(out, row)
	}
	return out
}
