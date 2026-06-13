package hr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Payslip-line kinds. P1 emits earning / pretax_deduction / tax /
// posttax_deduction in this pipeline order. er_contribution is reserved
// for P2 (employer contributions) — the migration CHECK already permits
// it so P2 needs no schema change, but the P1 engine never writes it.
const (
	LineKindEarning          = "earning"
	LineKindPretaxDeduction  = "pretax_deduction"
	LineKindTax              = "tax"
	LineKindPosttaxDeduction = "posttax_deduction"
	LineKindERContribution   = "er_contribution"
)

// Pay-input types mirrored from the payroll_pay_inputs CHECK constraint.
const (
	PayInputHours         = "hours"
	PayInputOvertime      = "overtime"
	PayInputBonus         = "bonus"
	PayInputReimbursement = "reimbursement"
	PayInputLOPDays       = "lop_days"
	PayInputAdjustment    = "adjustment"
)

// PayInput is one variable per-employee input for a run, read from
// payroll_pay_inputs by the pipeline's D1 gather step.
type PayInput struct {
	ID         uuid.UUID
	RunID      uuid.UUID
	EmployeeID uuid.UUID
	Type       string
	Code       string
	Label      string
	Qty        decimal.Decimal
	Rate       decimal.Decimal
	Amount     decimal.Decimal
	Taxable    bool
	Note       string
}

// PayslipLine is one resolved slip line in pipeline order. Base/Rate are
// informational (nil → NULL in the DB) and Amount is the resolved money
// figure. Seq is assigned by the store when the slip is persisted.
type PayslipLine struct {
	Seq           int
	Kind          string
	Code          string
	Label         string
	Base          *decimal.Decimal
	Rate          *decimal.Decimal
	Amount        decimal.Decimal
	Taxable       bool
	GLAccountCode string
}

// YTD is the engine-owned year-to-date accumulator for one
// (employee, tax_year), read from payroll_ytd.
//
// CumulativeTaxable is the cumulative *income-tax* base after pre-tax
// deductions (fed to packs as EmployeeInfo.YTDGross for cumulative-
// withholding income-tax methods like China's 累计预扣预缴).
// CumulativeContributionGross is the cumulative *contribution* base (fed
// as EmployeeInfo.YTDContributionGross for statutory contribution wage
// caps / surtax thresholds). CumulativeGross is the literal full gross
// paid (for reporting); it coincides with CumulativeContributionGross
// unless a component is flagged to also reduce the contribution base.
// The income-tax and contribution bases diverge once an employee has any
// pre-tax / salary-sacrifice line.
type YTD struct {
	CumulativeGross             decimal.Decimal
	CumulativeTaxable           decimal.Decimal
	CumulativeContributionGross decimal.Decimal
	CumulativeTax               decimal.Decimal
	PerCodeBase                 map[string]decimal.Decimal
	Exists                      bool
}

// RunHeader is the typed payroll_runs row the engine upserts when a run
// starts generating slips.
type RunHeader struct {
	ID                       uuid.UUID
	Name                     string
	PeriodStart              time.Time
	PeriodEnd                time.Time
	Freq                     string
	Currency                 string
	Status                   string
	RunType                  string
	Department               string
	SalaryExpenseAccountCode string
	SalaryPayableAccountCode string
	DeductionAccountMap      map[string]string
	CreatedBy                uuid.UUID
}

// FinalizeInput carries everything FinalizePayslip needs to persist one
// slip + its lines and advance YTD atomically. The earnings / pre-tax /
// post-tax lines are computed by the pure pipeline before the
// transaction opens; only the statutory tax lines depend on the persisted
// YTD, so they are produced inside the transaction via ComputeTax (which
// receives the prior-period cumulative income-tax base and cumulative
// contribution base — the real persisted YTD bases).
type FinalizeInput struct {
	TenantID          uuid.UUID
	RunID             uuid.UUID
	PayslipID         uuid.UUID
	EmployeeID        uuid.UUID
	StructureID       uuid.UUID
	Currency          string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	TaxYear           int
	Gross             decimal.Decimal
	TaxableGross      decimal.Decimal
	ContributionGross decimal.Decimal

	Earnings          []PayslipLine
	PretaxDeductions  []PayslipLine
	PosttaxDeductions []PayslipLine

	// ComputeTax returns the statutory tax lines and their total given the
	// prior-period cumulative bases: ytdTaxable is the cumulative income-tax
	// base (→ EmployeeInfo.YTDGross, for cumulative-withholding income-tax
	// methods) and ytdContribution is the cumulative contribution base
	// (→ EmployeeInfo.YTDContributionGross, for statutory contribution wage
	// caps / surtax thresholds). Both exclude this slip's own prior
	// contribution (reversed before the call) so a draft re-run is idempotent.
	ComputeTax func(ytdTaxable, ytdContribution decimal.Decimal) ([]PayslipLine, decimal.Decimal, error)
}

// FinalizedPayslip is the result of FinalizePayslip: the persisted slip's
// rollup plus the ordered lines (with Seq assigned).
type FinalizedPayslip struct {
	PayslipID         uuid.UUID
	Gross             decimal.Decimal
	TaxableGross      decimal.Decimal
	ContributionGross decimal.Decimal
	TaxTotal          decimal.Decimal
	TotalEEDeductions decimal.Decimal
	Net               decimal.Decimal
	Lines             []PayslipLine
	Replaced          bool
}

// PayrollStore owns the typed payroll tables (runs, payslips, lines,
// pay_inputs, ytd). It is constructed from the shared pgx pool and uses
// dbutil.WithTenantTx so every statement runs under the tenant's RLS
// context.
type PayrollStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPayrollStore binds the store to a pool.
func NewPayrollStore(pool *pgxpool.Pool) *PayrollStore {
	return &PayrollStore{pool: pool, now: time.Now}
}

// WithClock overrides now() for deterministic timestamps in tests.
func (s *PayrollStore) WithClock(now func() time.Time) *PayrollStore {
	if now != nil {
		s.now = now
	}
	return s
}

// UpsertRun writes (or refreshes) the typed run header. Called at the
// start of GeneratePayslips so slips/lines have a parent row to FK to.
func (s *PayrollStore) UpsertRun(ctx context.Context, tenantID uuid.UUID, h RunHeader) error {
	mapJSON, err := json.Marshal(h.DeductionAccountMap)
	if err != nil {
		return fmt.Errorf("hr: marshal deduction_account_map: %w", err)
	}
	if len(h.DeductionAccountMap) == 0 {
		mapJSON = []byte("{}")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO payroll_runs
				(tenant_id, id, name, period_start, period_end, freq, currency,
				 status, run_type, department, salary_expense_account_code,
				 salary_payable_account_code, deduction_account_map, created_by,
				 created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				name = EXCLUDED.name,
				period_start = EXCLUDED.period_start,
				period_end = EXCLUDED.period_end,
				freq = EXCLUDED.freq,
				currency = EXCLUDED.currency,
				status = EXCLUDED.status,
				run_type = EXCLUDED.run_type,
				department = EXCLUDED.department,
				salary_expense_account_code = EXCLUDED.salary_expense_account_code,
				salary_payable_account_code = EXCLUDED.salary_payable_account_code,
				deduction_account_map = EXCLUDED.deduction_account_map,
				updated_at = EXCLUDED.updated_at`,
			tenantID, h.ID, h.Name, h.PeriodStart, h.PeriodEnd, freqOrDefault(h.Freq),
			currencyOrDefault(h.Currency), statusOrDraft(h.Status), runTypeOrRegular(h.RunType),
			nullableString(h.Department), nullableString(h.SalaryExpenseAccountCode),
			nullableString(h.SalaryPayableAccountCode), mapJSON, nullableUUID(h.CreatedBy),
			s.now().UTC(),
		)
		if err != nil {
			return fmt.Errorf("hr: upsert payroll_run: %w", err)
		}
		return nil
	})
}

// UpdateRunTotals refreshes the denormalized run rollup + status after a
// generation pass.
func (s *PayrollStore) UpdateRunTotals(
	ctx context.Context, tenantID, runID uuid.UUID,
	payslipCount int, gross, taxable, eeDeductions, net decimal.Decimal, status string,
) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE payroll_runs
			   SET payslip_count = $3, total_gross = $4, total_taxable = $5,
			       total_ee_deductions = $6, total_net = $7, status = $8,
			       updated_at = $9
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, runID, payslipCount, gross, taxable, eeDeductions, net,
			statusOrDraft(status), s.now().UTC(),
		)
		if err != nil {
			return fmt.Errorf("hr: update payroll_run totals: %w", err)
		}
		return nil
	})
}

// MarkRunPosted links the posted JE to the run and flips the posted
// payslips to paid. Only the payslips in paidPayslipIDs — the slips that
// were actually included in the journal entry (PostPayRun posts only
// approved slips) — are promoted, so a partially-approved run does not
// mark its still-draft slips paid against a JE that excluded them.
func (s *PayrollStore) MarkRunPosted(ctx context.Context, tenantID, runID, jeID uuid.UUID, paidPayslipIDs []uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE payroll_runs
			   SET status = 'paid', posted_je_id = $3, updated_at = $4
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, runID, jeID, s.now().UTC(),
		); err != nil {
			return fmt.Errorf("hr: mark payroll_run posted: %w", err)
		}
		if len(paidPayslipIDs) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payroll_payslips
			   SET status = 'paid', posted_je_id = $3, updated_at = $4
			 WHERE tenant_id = $1 AND run_id = $2 AND id = ANY($5) AND status <> 'paid'`,
			tenantID, runID, jeID, s.now().UTC(), paidPayslipIDs,
		); err != nil {
			return fmt.Errorf("hr: mark payroll_payslips paid: %w", err)
		}
		return nil
	})
}

// RunTotals is the denormalized rollup of a run's persisted payslips.
type RunTotals struct {
	Count        int
	Gross        decimal.Decimal
	Taxable      decimal.Decimal
	EEDeductions decimal.Decimal
	Net          decimal.Decimal
}

// SumRunTotals aggregates the persisted payslips for a run into the
// denormalized header rollup. The typed payslips are the source of truth
// (in particular total_taxable has no KType equivalent).
func (s *PayrollStore) SumRunTotals(ctx context.Context, tenantID, runID uuid.UUID) (RunTotals, error) {
	var tot RunTotals
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT COUNT(*),
			       COALESCE(SUM(gross), 0),
			       COALESCE(SUM(taxable_gross), 0),
			       COALESCE(SUM(total_ee_deductions), 0),
			       COALESCE(SUM(net), 0)
			  FROM payroll_payslips
			 WHERE tenant_id = $1 AND run_id = $2`,
			tenantID, runID,
		)
		if err := row.Scan(&tot.Count, &tot.Gross, &tot.Taxable, &tot.EEDeductions, &tot.Net); err != nil {
			return fmt.Errorf("hr: sum run totals: %w", err)
		}
		return nil
	})
	if err != nil {
		return RunTotals{}, err
	}
	return tot, nil
}

// GatherPayInputs reads every variable input for one (run, employee),
// ordered deterministically by created_at then id.
func (s *PayrollStore) GatherPayInputs(ctx context.Context, tenantID, runID, employeeID uuid.UUID) ([]PayInput, error) {
	var out []PayInput
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, run_id, employee_id, type, code, label, qty, rate, amount, taxable, COALESCE(note, '')
			  FROM payroll_pay_inputs
			 WHERE tenant_id = $1 AND run_id = $2 AND employee_id = $3
			 ORDER BY created_at, id`,
			tenantID, runID, employeeID,
		)
		if err != nil {
			return fmt.Errorf("hr: query pay_inputs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var in PayInput
			if err := rows.Scan(&in.ID, &in.RunID, &in.EmployeeID, &in.Type, &in.Code,
				&in.Label, &in.Qty, &in.Rate, &in.Amount, &in.Taxable, &in.Note); err != nil {
				return fmt.Errorf("hr: scan pay_input: %w", err)
			}
			out = append(out, in)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AddPayInput inserts a single variable input. Returns the new row id.
func (s *PayrollStore) AddPayInput(ctx context.Context, tenantID uuid.UUID, in PayInput, actorID uuid.UUID) (uuid.UUID, error) {
	id := uuid.New()
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO payroll_pay_inputs
				(tenant_id, id, run_id, employee_id, type, code, label, qty, rate,
				 amount, taxable, note, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			tenantID, id, in.RunID, in.EmployeeID, in.Type, in.Code, in.Label,
			in.Qty, in.Rate, in.Amount, in.Taxable, nullableString(in.Note),
			nullableUUID(actorID), s.now().UTC(),
		)
		if err != nil {
			return fmt.Errorf("hr: insert pay_input: %w", err)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// LoadYTD reads the accumulator for (employee, tax_year). A missing row
// returns a zero-valued YTD with Exists=false (the first run of the year).
func (s *PayrollStore) LoadYTD(ctx context.Context, tenantID, employeeID uuid.UUID, taxYear int) (YTD, error) {
	var y YTD
	y.PerCodeBase = map[string]decimal.Decimal{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var perCode []byte
		row := tx.QueryRow(ctx, `
			SELECT cumulative_gross, cumulative_taxable, cumulative_contribution_gross,
			       cumulative_tax, per_code_base
			  FROM payroll_ytd
			 WHERE tenant_id = $1 AND employee_id = $2 AND tax_year = $3`,
			tenantID, employeeID, taxYear,
		)
		if err := row.Scan(&y.CumulativeGross, &y.CumulativeTaxable, &y.CumulativeContributionGross,
			&y.CumulativeTax, &perCode); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("hr: load ytd: %w", err)
		}
		y.Exists = true
		y.PerCodeBase = decodePerCode(perCode)
		return nil
	})
	if err != nil {
		return YTD{}, err
	}
	return y, nil
}

// FinalizePayslip persists one slip + its ordered lines and advances the
// YTD accumulator in a single transaction. It is idempotent per
// (run, employee): a draft re-run reverses the slip's prior YTD
// contribution before adding the new one, so cumulative gross/tax never
// double-count.
func (s *PayrollStore) FinalizePayslip(ctx context.Context, in FinalizeInput) (*FinalizedPayslip, error) {
	if in.ComputeTax == nil {
		in.ComputeTax = func(_, _ decimal.Decimal) ([]PayslipLine, decimal.Decimal, error) {
			return nil, decimal.Zero, nil
		}
	}
	var result FinalizedPayslip
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// 1. Lock + read the YTD accumulator FIRST. Seed a zero row so
		//    the FOR UPDATE below always has a row to lock: a bare
		//    SELECT ... FOR UPDATE locks nothing when the row is absent,
		//    so two concurrent first-of-year finalizers would both miss
		//    the lock and race to INSERT, with one failing the natural
		//    PK. The ON CONFLICT DO NOTHING makes a concurrent seeder
		//    block on the uncommitted row and then no-op, after which the
		//    FOR UPDATE serializes both transactions on the now-existing
		//    row.
		//
		//    Acquiring this lock BEFORE reading the payslip identity is
		//    what makes two concurrent generations of the SAME
		//    (run, employee) safe: the existence/prior reads below run
		//    only once we hold the YTD lock, so under READ COMMITTED the
		//    second caller sees the first's committed slip + lines and
		//    correctly REPLACES (reverses + re-adds) instead of reading a
		//    stale "not found" — which would otherwise insert lines under
		//    a fresh id that the ON CONFLICT upsert never created (FK
		//    violation) and double-count the YTD.
		seedNow := s.now().UTC()
		if _, err := tx.Exec(ctx, `
			INSERT INTO payroll_ytd
				(tenant_id, employee_id, tax_year, cumulative_gross,
				 cumulative_taxable, cumulative_contribution_gross, cumulative_tax,
				 per_code_base, updated_at)
			VALUES ($1, $2, $3, 0, 0, 0, 0, '{}'::jsonb, $4)
			ON CONFLICT (tenant_id, employee_id, tax_year) DO NOTHING`,
			in.TenantID, in.EmployeeID, in.TaxYear, seedNow,
		); err != nil {
			return fmt.Errorf("hr: seed ytd: %w", err)
		}
		var cumGross, cumTaxable, cumContribution, cumTax decimal.Decimal
		var perCodeRaw []byte
		yrow := tx.QueryRow(ctx, `
			SELECT cumulative_gross, cumulative_taxable, cumulative_contribution_gross,
			       cumulative_tax, per_code_base
			  FROM payroll_ytd
			 WHERE tenant_id = $1 AND employee_id = $2 AND tax_year = $3
			 FOR UPDATE`,
			in.TenantID, in.EmployeeID, in.TaxYear,
		)
		if err := yrow.Scan(&cumGross, &cumTaxable, &cumContribution, &cumTax, &perCodeRaw); err != nil {
			return fmt.Errorf("hr: lock ytd: %w", err)
		}
		perCode := decodePerCode(perCodeRaw)

		// 2. Existing slip for this (run, employee)? Its id is the
		//    canonical payslip id we reuse so the typed slip keeps a
		//    stable identity across re-generations. Read under the YTD
		//    lock (see step 1) so a concurrent same-(run, employee)
		//    finalizer that already committed is visible here.
		var existingID uuid.UUID
		var priorGross, priorTaxable, priorContribution, priorTax decimal.Decimal
		var exists bool
		row := tx.QueryRow(ctx, `
			SELECT id, gross, taxable_gross, contribution_gross, tax_total
			  FROM payroll_payslips
			 WHERE tenant_id = $1 AND run_id = $2 AND employee_id = $3`,
			in.TenantID, in.RunID, in.EmployeeID,
		)
		switch err := row.Scan(&existingID, &priorGross, &priorTaxable, &priorContribution, &priorTax); {
		case err == nil:
			exists = true
		case errors.Is(err, pgx.ErrNoRows):
			exists = false
		default:
			return fmt.Errorf("hr: lookup existing payslip: %w", err)
		}
		payslipID := in.PayslipID
		if exists {
			payslipID = existingID
		}

		// 3. Prior per-code contribution of THIS slip (for the YTD
		//    per_code_base reversal), reconstructed from its lines.
		priorPerCode := map[string]decimal.Decimal{}
		if exists {
			lrows, err := tx.Query(ctx, `
				SELECT code, amount FROM payroll_payslip_lines
				 WHERE tenant_id = $1 AND payslip_id = $2`,
				in.TenantID, payslipID,
			)
			if err != nil {
				return fmt.Errorf("hr: load prior lines: %w", err)
			}
			for lrows.Next() {
				var code string
				var amt decimal.Decimal
				if err := lrows.Scan(&code, &amt); err != nil {
					lrows.Close()
					return fmt.Errorf("hr: scan prior line: %w", err)
				}
				if code != "" {
					priorPerCode[code] = priorPerCode[code].Add(amt)
				}
			}
			lrows.Close()
			if err := lrows.Err(); err != nil {
				return fmt.Errorf("hr: iterate prior lines: %w", err)
			}
		}

		// 4. Reverse this slip's prior contribution so the base reflects
		//    only OTHER runs in the year. On a first generation prior* are
		//    zero, so base == cumulative (sum of earlier runs).
		baseGross := cumGross.Sub(priorGross)
		baseTaxable := cumTaxable.Sub(priorTaxable)
		baseContribution := cumContribution.Sub(priorContribution)
		baseTax := cumTax.Sub(priorTax)
		for code, amt := range priorPerCode {
			perCode[code] = perCode[code].Sub(amt)
		}

		// 5. Statutory tax, computed against the real persisted YTD bases:
		//    cumulative income-tax base (cumulative-withholding methods) +
		//    cumulative contribution base (contribution caps / surtax
		//    thresholds), both excluding this slip.
		taxLines, taxTotal, err := in.ComputeTax(baseTaxable, baseContribution)
		if err != nil {
			return err
		}

		// 6. Assemble lines in pipeline order with monotonic seq.
		ordered := make([]PayslipLine, 0,
			len(in.Earnings)+len(in.PretaxDeductions)+len(taxLines)+len(in.PosttaxDeductions))
		seq := 0
		appendKind := func(src []PayslipLine, kind string) {
			for _, l := range src {
				l.Seq = seq
				l.Kind = kind
				ordered = append(ordered, l)
				seq++
			}
		}
		appendKind(in.Earnings, LineKindEarning)
		appendKind(in.PretaxDeductions, LineKindPretaxDeduction)
		appendKind(taxLines, LineKindTax)
		appendKind(in.PosttaxDeductions, LineKindPosttaxDeduction)

		// 7. Net / totals. total_ee_deductions = pretax + tax + posttax.
		var pretaxSum, posttaxSum decimal.Decimal
		for _, l := range in.PretaxDeductions {
			pretaxSum = pretaxSum.Add(l.Amount)
		}
		for _, l := range in.PosttaxDeductions {
			posttaxSum = posttaxSum.Add(l.Amount)
		}
		totalEE := pretaxSum.Add(taxTotal).Add(posttaxSum)
		net := in.Gross.Sub(totalEE)

		// 8. Upsert the slip row (keyed on the natural run/employee unique
		//    so a re-run updates in place) and replace its lines.
		now := s.now().UTC()
		if _, err := tx.Exec(ctx, `
			INSERT INTO payroll_payslips
				(tenant_id, id, run_id, employee_id, structure_id, currency,
				 period_start, period_end, gross, taxable_gross, contribution_gross,
				 tax_total, total_ee_deductions, net, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'draft', $15, $15)
			ON CONFLICT (tenant_id, run_id, employee_id) DO UPDATE SET
				structure_id = EXCLUDED.structure_id,
				currency = EXCLUDED.currency,
				period_start = EXCLUDED.period_start,
				period_end = EXCLUDED.period_end,
				gross = EXCLUDED.gross,
				taxable_gross = EXCLUDED.taxable_gross,
				contribution_gross = EXCLUDED.contribution_gross,
				tax_total = EXCLUDED.tax_total,
				total_ee_deductions = EXCLUDED.total_ee_deductions,
				net = EXCLUDED.net,
				status = 'draft',
				updated_at = EXCLUDED.updated_at`,
			in.TenantID, payslipID, in.RunID, in.EmployeeID, nullableUUID(in.StructureID),
			currencyOrDefault(in.Currency), in.PeriodStart, in.PeriodEnd, in.Gross,
			in.TaxableGross, in.ContributionGross, taxTotal, totalEE, net, now,
		); err != nil {
			return fmt.Errorf("hr: upsert payslip: %w", err)
		}
		if exists {
			if _, err := tx.Exec(ctx, `
				DELETE FROM payroll_payslip_lines WHERE tenant_id = $1 AND payslip_id = $2`,
				in.TenantID, payslipID,
			); err != nil {
				return fmt.Errorf("hr: clear payslip lines: %w", err)
			}
		}
		for _, l := range ordered {
			if _, err := tx.Exec(ctx, `
				INSERT INTO payroll_payslip_lines
					(tenant_id, id, payslip_id, seq, kind, code, label, base, rate,
					 amount, taxable, gl_account_code, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				in.TenantID, uuid.New(), payslipID, l.Seq, l.Kind, l.Code, l.Label,
				l.Base, l.Rate, l.Amount, l.Taxable, nullableString(l.GLAccountCode), now,
			); err != nil {
				return fmt.Errorf("hr: insert payslip line: %w", err)
			}
		}

		// 9. Advance YTD: cumulative_gross tracks the literal full gross,
		//    cumulative_taxable the cumulative income-tax base,
		//    cumulative_contribution_gross the cumulative contribution base
		//    (contribution caps / surtax thresholds), cumulative_tax the tax
		//    withheld.
		newCumGross := baseGross.Add(in.Gross)
		newCumTaxable := baseTaxable.Add(in.TaxableGross)
		newCumContribution := baseContribution.Add(in.ContributionGross)
		newCumTax := baseTax.Add(taxTotal)
		for _, l := range ordered {
			if l.Code != "" {
				perCode[l.Code] = perCode[l.Code].Add(l.Amount)
			}
		}
		perCodeJSON, err := encodePerCode(perCode)
		if err != nil {
			return err
		}
		// The row was seeded + locked in step 3, so an unconditional
		// UPDATE is always correct here (no INSERT-vs-UPDATE split).
		if _, err := tx.Exec(ctx, `
			UPDATE payroll_ytd
			   SET cumulative_gross = $4, cumulative_taxable = $5,
			       cumulative_contribution_gross = $6, cumulative_tax = $7,
			       per_code_base = $8, updated_at = $9
			 WHERE tenant_id = $1 AND employee_id = $2 AND tax_year = $3`,
			in.TenantID, in.EmployeeID, in.TaxYear, newCumGross, newCumTaxable,
			newCumContribution, newCumTax, perCodeJSON, now,
		); err != nil {
			return fmt.Errorf("hr: update ytd: %w", err)
		}

		result = FinalizedPayslip{
			PayslipID:         payslipID,
			Gross:             in.Gross,
			TaxableGross:      in.TaxableGross,
			ContributionGross: in.ContributionGross,
			TaxTotal:          taxTotal,
			TotalEEDeductions: totalEE,
			Net:               net,
			Lines:             ordered,
			Replaced:          exists,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// GetPayslipLines returns the ordered lines for a payslip. Used by the
// posting path and tests to read back the persisted breakdown.
func (s *PayrollStore) GetPayslipLines(ctx context.Context, tenantID, payslipID uuid.UUID) ([]PayslipLine, error) {
	var out []PayslipLine
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT seq, kind, code, label, base, rate, amount, taxable, COALESCE(gl_account_code, '')
			  FROM payroll_payslip_lines
			 WHERE tenant_id = $1 AND payslip_id = $2
			 ORDER BY seq`,
			tenantID, payslipID,
		)
		if err != nil {
			return fmt.Errorf("hr: query payslip lines: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l PayslipLine
			var base, rate decimal.NullDecimal
			if err := rows.Scan(&l.Seq, &l.Kind, &l.Code, &l.Label, &base, &rate,
				&l.Amount, &l.Taxable, &l.GLAccountCode); err != nil {
				return fmt.Errorf("hr: scan payslip line: %w", err)
			}
			if base.Valid {
				b := base.Decimal
				l.Base = &b
			}
			if rate.Valid {
				r := rate.Decimal
				l.Rate = &r
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- helpers ----------------------------------------------------------

func decodePerCode(raw []byte) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	if len(raw) == 0 {
		return out
	}
	var m map[string]decimal.Decimal
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func encodePerCode(m map[string]decimal.Decimal) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("hr: marshal per_code_base: %w", err)
	}
	return b, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func freqOrDefault(f string) string {
	if f == "" {
		return "monthly"
	}
	return f
}

func currencyOrDefault(c string) string {
	if c == "" {
		return "USD"
	}
	return c
}

func statusOrDraft(s string) string {
	if s == "" {
		return "draft"
	}
	return s
}

func runTypeOrRegular(r string) string {
	if r == "" {
		return "regular"
	}
	return r
}
