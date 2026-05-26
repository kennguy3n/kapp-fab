// Package finance — Phase N5 budget surface.
//
// Budgets are an annual planning artefact: a finance team enters
// twelve monthly target amounts per (account_code, cost_center)
// tuple, then the platform reports actual vs. plan variance over
// any sub-period and raises notifications when MTD variance
// crosses a configurable threshold.
//
// Storage shape (migration 000062):
//
//   - `budgets`        — header per (tenant, budget). Carries the
//     fiscal year, status, optional default cost_center, and the
//     per-budget variance_threshold (NULL = use platform default).
//
//   - `budget_lines`   — one row per (tenant, budget, account,
//     cost_center) with 12 `amount_<mmm>` columns covering Jan
//     through Dec of the fiscal year, plus a STORED generated
//     `annual_total`.
//
// The KType mirror (finance.budget) gives the metadata-driven UI,
// agent tools, and slash command a single source of truth for the
// shape; the typed tables back the variance query that JOINs against
// journal_lines.
package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/notifications"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// KType identifiers for the budget surface.
const (
	KTypeBudget     = "finance.budget"
	KTypeBudgetLine = "finance.budget_line"
)

// Budget status values. Matches the CHECK constraint in the
// migration; the variance alerter only walks `active` budgets.
const (
	BudgetStatusDraft  = "draft"
	BudgetStatusActive = "active"
	BudgetStatusClosed = "closed"
)

// ActionTypeBudgetVariance is the scheduled_actions.action_type
// the variance alerter registers under. One row per tenant; the
// per-budget cadence/threshold lives on the budget row itself.
const ActionTypeBudgetVariance = "budget_variance"

// DefaultBudgetVarianceIntervalSeconds is the cadence the wizard
// seeds the alerter with — daily. The cost of a run with zero
// active budgets is one SELECT against `budgets`, so the cadence
// is dominated by alert latency for tenants who DO use budgets.
const DefaultBudgetVarianceIntervalSeconds = 86400

// DefaultVarianceThreshold is the fraction of plan that the alerter
// treats as the cutoff when a budget does not override it.
// 10% is the conventional ERP / CFO ops threshold (see ERPNext
// Budget Variance Alert) — wide enough to skip seasonal noise,
// tight enough that real overruns surface within a month.
var DefaultVarianceThreshold = decimal.NewFromFloat(0.10)

// Budget is the header row.
type Budget struct {
	TenantID           uuid.UUID        `json:"tenant_id"`
	ID                 uuid.UUID        `json:"id"`
	Name               string           `json:"name"`
	FiscalYear         int              `json:"fiscal_year"`
	Status             string           `json:"status"`
	CostCenter         string           `json:"cost_center,omitempty"`
	Notes              string           `json:"notes,omitempty"`
	VarianceThreshold  *decimal.Decimal `json:"variance_threshold,omitempty"`
	CreatedBy          *uuid.UUID       `json:"created_by,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
}

// BudgetLine is a per-account × per-cost-centre × monthly grid row.
// Months[i] is the planned amount for calendar month i+1
// (Months[0] = January, Months[11] = December). The MVP scope is
// calendar-year fiscal years: a tenant on a non-January FY start
// (e.g. India's April-March) is supported by ledger fiscal_periods
// (lockout) but the budget grid is still calendar-aligned today.
// Adding non-calendar fiscal-year support is the natural extension
// point at budgetFiscalWindow / VarianceQuery.
type BudgetLine struct {
	TenantID    uuid.UUID         `json:"tenant_id"`
	ID          uuid.UUID         `json:"id"`
	BudgetID    uuid.UUID         `json:"budget_id"`
	AccountCode string            `json:"account_code"`
	CostCenter  string            `json:"cost_center,omitempty"`
	Months      [12]decimal.Decimal `json:"months"`
	AnnualTotal decimal.Decimal   `json:"annual_total"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// VarianceRow is one row of the budget-vs-actual report. AccountCode
// + CostCenter identify the budget line; Period is the calendar
// month label ("2025-01") or "YTD" for the period total. AccountType
// carries the chart-of-accounts classification ("asset" / "liability"
// / "equity" / "revenue" / "expense") so renderers can pick colour
// and "exceeded plan = good/bad" semantics without re-deriving the
// debit-vs-credit normal — the backend has already sign-normalised
// the Actual / Variance amounts so that positive = exceeded plan
// for every account type, but whether exceeding plan is desirable
// only the account type can answer (over-earning revenue is good;
// over-spending an expense is bad).
type VarianceRow struct {
	BudgetID    uuid.UUID       `json:"budget_id"`
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name,omitempty"`
	AccountType string          `json:"account_type,omitempty"`
	CostCenter  string          `json:"cost_center,omitempty"`
	Period      string          `json:"period"`
	Budgeted    decimal.Decimal `json:"budgeted"`
	Actual      decimal.Decimal `json:"actual"`
	Variance    decimal.Decimal `json:"variance"`
	VariancePct decimal.Decimal `json:"variance_pct"`
	// Favourable is true when the row's variance represents a
	// better-than-plan outcome (revenue over-perform or expense
	// under-spend). False covers the worse-than-plan side
	// (expense over-spend, revenue under-perform). For
	// asset / liability / equity account types — which are not
	// natural planning targets — Favourable is true iff variance
	// >= 0, matching the as-recorded sign convention. The footer
	// rollups use this flag to bucket TotalFavourableVariance vs
	// TotalUnfavourableVariance so finance dashboards can colour
	// the period summary without re-deriving account-type
	// semantics from the row level.
	Favourable bool `json:"favourable"`
}

// VarianceReport is the budget-vs-actual rollup over a period.
//
// Total accounting:
//
//   - TotalBudgeted / TotalActual / TotalVariance are gross sums
//     across every account type AFTER the credit-normal sign flip,
//     so positive variance always means "exceeded plan". These are
//     useful as a single-number rollup but obscure favourability
//     because they conflate over-earning revenue (good) with
//     over-spending an expense (bad).
//
//   - TotalFavourableVariance / TotalUnfavourableVariance split the
//     gross variance into "better than plan" / "worse than plan"
//     buckets using each row's Favourable flag. A revenue line
//     where actual exceeds plan contributes to favourable; an
//     expense line where actual exceeds plan contributes to
//     unfavourable. Finance dashboards should prefer these two
//     numbers over the gross TotalVariance for at-a-glance
//     red/green colouring at the footer.
type VarianceReport struct {
	TenantID                  uuid.UUID       `json:"tenant_id"`
	BudgetID                  uuid.UUID       `json:"budget_id"`
	BudgetName                string          `json:"budget_name"`
	FiscalYear                int             `json:"fiscal_year"`
	From                      time.Time       `json:"from"`
	To                        time.Time       `json:"to"`
	Rows                      []VarianceRow   `json:"rows"`
	TotalBudgeted             decimal.Decimal `json:"total_budgeted"`
	TotalActual               decimal.Decimal `json:"total_actual"`
	TotalVariance             decimal.Decimal `json:"total_variance"`
	TotalFavourableVariance   decimal.Decimal `json:"total_favourable_variance"`
	TotalUnfavourableVariance decimal.Decimal `json:"total_unfavourable_variance"`
}

// budgetSchema — the KType mirror of the typed budgets table.
var budgetSchema = []byte(`{
  "name": "finance.budget",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "fiscal_year", "type": "number", "required": true},
    {"name": "status", "type": "enum", "values": ["draft", "active", "closed"], "default": "draft"},
    {"name": "cost_center", "type": "string", "max_length": 32},
    {"name": "notes", "type": "text"},
    {"name": "variance_threshold", "type": "number"}
  ],
  "views": {
    "list": {"columns": ["name", "fiscal_year", "status", "cost_center"]},
    "form": {"sections": [
      {"title": "Budget", "fields": ["name", "fiscal_year", "status", "cost_center"]},
      {"title": "Alerts", "fields": ["variance_threshold"]},
      {"title": "Notes", "fields": ["notes"]}
    ]}
  },
  "cards": {"summary": "{{name}} — FY{{fiscal_year}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "agent_tools": ["finance.create_budget", "finance.budget_vs_actual"]
}`)

// budgetLineSchema — the KType mirror of the typed budget_lines
// table. The 12 monthly amounts are exposed as a single "months"
// number-array field for the visual builder; the typed table
// stores them as discrete amount_<mmm> columns under the hood so
// JOIN against journal_lines per fiscal month stays cheap.
var budgetLineSchema = []byte(`{
  "name": "finance.budget_line",
  "version": 1,
  "fields": [
    {"name": "budget_id", "type": "ref", "ktype": "finance.budget", "required": true},
    {"name": "account_code", "type": "string", "required": true, "max_length": 32},
    {"name": "cost_center", "type": "string", "max_length": 32},
    {"name": "months", "type": "array", "required": true},
    {"name": "annual_total", "type": "number"}
  ],
  "views": {
    "list": {"columns": ["account_code", "cost_center", "annual_total"]},
    "form": {"sections": [
      {"title": "Line", "fields": ["budget_id", "account_code", "cost_center"]},
      {"title": "Monthly Plan", "fields": ["months", "annual_total"]}
    ]}
  },
  "cards": {"summary": "{{account_code}} — {{annual_total}}"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]}
}`)

// BudgetKTypes returns the KTypes the budget surface registers.
// Kept distinct from finance.All() so callers who only need the
// core finance surface (e.g. legacy migrations or unit tests that
// never wire the budget store) can opt out cleanly.
func BudgetKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeBudget, Version: 1, Schema: budgetSchema},
		{Name: KTypeBudgetLine, Version: 1, Schema: budgetLineSchema},
	}
}

// ---------------------------------------------------------------------------
// Sentinel errors.
// ---------------------------------------------------------------------------

// Phase N5 sentinel errors. These are returned by the BudgetStore so
// the HTTP layer can map them to 404 / 400 and the agent layer can
// surface them verbatim to the LLM caller.
var (
	// ErrBudgetNotFound signals that no budget with the supplied id
	// exists in the active tenant scope.
	ErrBudgetNotFound = errors.New("finance: budget not found")
	// ErrBudgetLineNotFound signals that no line with the supplied
	// id exists on the supplied budget in the active tenant scope.
	ErrBudgetLineNotFound = errors.New("finance: budget line not found")
	// ErrInvalidBudget is returned when the supplied budget header
	// fails validation (e.g. empty name, fiscal_year unset, status
	// not one of draft/active/closed).
	ErrInvalidBudget = errors.New("finance: invalid budget")
	// ErrInvalidBudgetLine is returned when the supplied line fails
	// validation (e.g. empty account_code, monthly amount negative).
	ErrInvalidBudgetLine = errors.New("finance: invalid budget line")
)

// ---------------------------------------------------------------------------
// Store.
// ---------------------------------------------------------------------------

// BudgetStore is the Postgres-backed persistence for budgets +
// budget_lines + the variance computation. All reads/writes route
// through dbutil.WithTenantTx so RLS enforces tenant isolation just
// like every other typed table in the platform.
type BudgetStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewBudgetStore wires a store against the shared pool.
func NewBudgetStore(pool *pgxpool.Pool) *BudgetStore {
	return &BudgetStore{
		pool: pool,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the store's time source. Useful in tests.
func (s *BudgetStore) WithClock(now func() time.Time) *BudgetStore {
	if now != nil {
		s.now = now
	}
	return s
}

// CreateBudget inserts a new budget header. ID is generated when
// in.ID is the zero UUID. Returns the inserted row with created_at /
// updated_at populated by the database.
func (s *BudgetStore) CreateBudget(ctx context.Context, in Budget) (*Budget, error) {
	var out *Budget
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := createBudgetTx(ctx, tx, in)
		if err != nil {
			return err
		}
		out = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateBudgetWithLines inserts a budget header and its line items
// in a single tenant transaction so a partial failure does not leave
// an orphan header in the database. Callers that need atomic
// create-with-lines semantics (e.g. the finance.create_budget agent
// tool) should use this entrypoint rather than chaining
// CreateBudget + UpsertBudgetLine, each of which spans its own
// WithTenantTx and would otherwise commit the header before the
// first line failure surfaces. Returns the inserted header with
// created_at/updated_at populated, and the inserted line rows in
// the same order they were supplied. Validation errors (e.g. empty
// name, line[i].account_code missing) roll back the entire
// transaction so an aborted create never partially commits.
func (s *BudgetStore) CreateBudgetWithLines(ctx context.Context, header Budget, lines []BudgetLine) (*Budget, []BudgetLine, error) {
	var (
		outHeader *Budget
		outLines  []BudgetLine
	)
	err := dbutil.WithTenantTx(ctx, s.pool, header.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := createBudgetTx(ctx, tx, header)
		if err != nil {
			return err
		}
		outHeader = b
		outLines = make([]BudgetLine, 0, len(lines))
		for i := range lines {
			line := lines[i]
			line.TenantID = b.TenantID
			line.BudgetID = b.ID
			inserted, err := upsertBudgetLineTx(ctx, tx, line)
			if err != nil {
				return fmt.Errorf("line[%d]: %w", i, err)
			}
			outLines = append(outLines, *inserted)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return outHeader, outLines, nil
}

// createBudgetTx is the transaction-bound budget insert used by
// both the public CreateBudget and the atomic CreateBudgetWithLines
// entrypoints. It performs the same validation as the public Create
// path so the two surfaces produce identical error messages.
func createBudgetTx(ctx context.Context, tx pgx.Tx, in Budget) (*Budget, error) {
	if in.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidBudget)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: name required", ErrInvalidBudget)
	}
	if in.FiscalYear < 1900 || in.FiscalYear > 2200 {
		return nil, fmt.Errorf("%w: fiscal_year out of range (got %d)", ErrInvalidBudget, in.FiscalYear)
	}
	if in.Status == "" {
		in.Status = BudgetStatusDraft
	}
	switch in.Status {
	case BudgetStatusDraft, BudgetStatusActive, BudgetStatusClosed:
	default:
		return nil, fmt.Errorf("%w: status %q not one of draft/active/closed", ErrInvalidBudget, in.Status)
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	out := in
	err := tx.QueryRow(ctx,
		`INSERT INTO budgets
		    (tenant_id, id, name, fiscal_year, status, cost_center, notes,
		     variance_threshold, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING created_at, updated_at`,
		in.TenantID, in.ID, in.Name, in.FiscalYear, in.Status,
		nullIfEmpty(in.CostCenter), nullIfEmpty(in.Notes),
		in.VarianceThreshold, in.CreatedBy,
	).Scan(&out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("finance: insert budget: %w", err)
	}
	return &out, nil
}

// UpdateBudget mutates an existing budget header. The fiscal_year
// column is intentionally NOT updatable — once budget_lines have
// been entered for FY2025, changing the year to FY2026 would
// silently re-key every line; callers who need to "copy a budget
// forward" should call CopyBudget instead.
func (s *BudgetStore) UpdateBudget(ctx context.Context, in Budget) (*Budget, error) {
	if in.TenantID == uuid.Nil || in.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id and id required", ErrInvalidBudget)
	}
	// Mirror the CreateBudget contract: name is required and may
	// not be all-whitespace. The DB column is TEXT NOT NULL so a
	// raw NULL is impossible, but the empty string would still
	// satisfy the constraint and render an unnameable row in the
	// list view, so we reject it at the Go layer the same way
	// Create does.
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: name required", ErrInvalidBudget)
	}
	switch in.Status {
	case BudgetStatusDraft, BudgetStatusActive, BudgetStatusClosed:
	default:
		return nil, fmt.Errorf("%w: status %q not one of draft/active/closed", ErrInvalidBudget, in.Status)
	}
	var out Budget
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`UPDATE budgets SET
			     name = $3,
			     status = $4,
			     cost_center = $5,
			     notes = $6,
			     variance_threshold = $7
			 WHERE tenant_id = $1 AND id = $2
			 RETURNING tenant_id, id, name, fiscal_year, status,
			           COALESCE(cost_center, ''), COALESCE(notes, ''),
			           variance_threshold, created_by, created_at, updated_at`,
			in.TenantID, in.ID, in.Name, in.Status,
			nullIfEmpty(in.CostCenter), nullIfEmpty(in.Notes),
			in.VarianceThreshold,
		).Scan(
			&out.TenantID, &out.ID, &out.Name, &out.FiscalYear, &out.Status,
			&out.CostCenter, &out.Notes, &out.VarianceThreshold,
			&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBudgetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("finance: update budget: %w", err)
	}
	return &out, nil
}

// GetBudget loads a single budget by id within the tenant's scope.
func (s *BudgetStore) GetBudget(ctx context.Context, tenantID, id uuid.UUID) (*Budget, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id and id required", ErrInvalidBudget)
	}
	var out *Budget
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := getBudgetTx(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		out = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// getBudgetTx is the transaction-bound budget loader used by both
// the public GetBudget and the single-tx BudgetVsActual path so the
// variance report observes a consistent snapshot of the header +
// lines + actuals at one point in time.
func getBudgetTx(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (*Budget, error) {
	var out Budget
	err := tx.QueryRow(ctx,
		`SELECT tenant_id, id, name, fiscal_year, status,
		        COALESCE(cost_center, ''), COALESCE(notes, ''),
		        variance_threshold, created_by, created_at, updated_at
		 FROM budgets WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	).Scan(
		&out.TenantID, &out.ID, &out.Name, &out.FiscalYear, &out.Status,
		&out.CostCenter, &out.Notes, &out.VarianceThreshold,
		&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBudgetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("finance: get budget: %w", err)
	}
	return &out, nil
}

// ListBudgets returns every budget for the tenant, ordered by
// fiscal_year DESC then name ASC. Variance dashboards filter by
// status client-side because the typical tenant has < 20 budgets.
func (s *BudgetStore) ListBudgets(ctx context.Context, tenantID uuid.UUID) ([]Budget, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidBudget)
	}
	out := make([]Budget, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, fiscal_year, status,
			        COALESCE(cost_center, ''), COALESCE(notes, ''),
			        variance_threshold, created_by, created_at, updated_at
			 FROM budgets WHERE tenant_id = $1
			 ORDER BY fiscal_year DESC, name ASC`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b Budget
			if err := rows.Scan(
				&b.TenantID, &b.ID, &b.Name, &b.FiscalYear, &b.Status,
				&b.CostCenter, &b.Notes, &b.VarianceThreshold,
				&b.CreatedBy, &b.CreatedAt, &b.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("finance: list budgets: %w", err)
	}
	return out, nil
}

// DeleteBudget removes a budget header and (via the ON DELETE
// CASCADE on budget_lines) all of its lines. Returns
// ErrBudgetNotFound when the row is absent.
func (s *BudgetStore) DeleteBudget(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return fmt.Errorf("%w: tenant_id and id required", ErrInvalidBudget)
	}
	var rowCount int64
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`DELETE FROM budgets WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		rowCount = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("finance: delete budget: %w", err)
	}
	if rowCount == 0 {
		return ErrBudgetNotFound
	}
	return nil
}

// UpsertBudgetLine inserts or updates a single budget_lines row.
// The unique key is (tenant, budget, account_code, COALESCE(cost_center, ''))
// — supplying the same triple twice replaces the prior monthly grid.
func (s *BudgetStore) UpsertBudgetLine(ctx context.Context, in BudgetLine) (*BudgetLine, error) {
	var out *BudgetLine
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		l, err := upsertBudgetLineTx(ctx, tx, in)
		if err != nil {
			return err
		}
		out = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// upsertBudgetLineTx is the transaction-bound budget_line upsert
// used by both the public UpsertBudgetLine and the atomic
// CreateBudgetWithLines entrypoints. Same validation contract as
// the public path.
func upsertBudgetLineTx(ctx context.Context, tx pgx.Tx, in BudgetLine) (*BudgetLine, error) {
	if in.TenantID == uuid.Nil || in.BudgetID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id and budget_id required", ErrInvalidBudgetLine)
	}
	if strings.TrimSpace(in.AccountCode) == "" {
		return nil, fmt.Errorf("%w: account_code required", ErrInvalidBudgetLine)
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	out := in
	err := tx.QueryRow(ctx,
		`INSERT INTO budget_lines
		    (tenant_id, id, budget_id, account_code, cost_center,
		     amount_jan, amount_feb, amount_mar, amount_apr,
		     amount_may, amount_jun, amount_jul, amount_aug,
		     amount_sep, amount_oct, amount_nov, amount_dec)
		 VALUES ($1, $2, $3, $4, $5,
		         $6, $7, $8, $9,
		         $10, $11, $12, $13,
		         $14, $15, $16, $17)
		 ON CONFLICT (tenant_id, budget_id, account_code, COALESCE(cost_center, ''))
		 DO UPDATE SET
		     amount_jan = EXCLUDED.amount_jan,
		     amount_feb = EXCLUDED.amount_feb,
		     amount_mar = EXCLUDED.amount_mar,
		     amount_apr = EXCLUDED.amount_apr,
		     amount_may = EXCLUDED.amount_may,
		     amount_jun = EXCLUDED.amount_jun,
		     amount_jul = EXCLUDED.amount_jul,
		     amount_aug = EXCLUDED.amount_aug,
		     amount_sep = EXCLUDED.amount_sep,
		     amount_oct = EXCLUDED.amount_oct,
		     amount_nov = EXCLUDED.amount_nov,
		     amount_dec = EXCLUDED.amount_dec
		 RETURNING id, annual_total, created_at, updated_at`,
		in.TenantID, in.ID, in.BudgetID, in.AccountCode, nullIfEmpty(in.CostCenter),
		in.Months[0], in.Months[1], in.Months[2], in.Months[3],
		in.Months[4], in.Months[5], in.Months[6], in.Months[7],
		in.Months[8], in.Months[9], in.Months[10], in.Months[11],
	).Scan(&out.ID, &out.AnnualTotal, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("finance: upsert budget_line: %w", err)
	}
	return &out, nil
}

// ListBudgetLines returns every line for a budget, ordered by
// account_code then cost_center for a stable spreadsheet render.
func (s *BudgetStore) ListBudgetLines(ctx context.Context, tenantID, budgetID uuid.UUID) ([]BudgetLine, error) {
	if tenantID == uuid.Nil || budgetID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id and budget_id required", ErrInvalidBudgetLine)
	}
	var out []BudgetLine
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		lines, err := listBudgetLinesTx(ctx, tx, tenantID, budgetID)
		if err != nil {
			return err
		}
		out = lines
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// listBudgetLinesTx is the transaction-bound list used by both the
// public ListBudgetLines and the single-tx BudgetVsActual path.
func listBudgetLinesTx(ctx context.Context, tx pgx.Tx, tenantID, budgetID uuid.UUID) ([]BudgetLine, error) {
	out := make([]BudgetLine, 0)
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, budget_id, account_code, COALESCE(cost_center, ''),
		        amount_jan, amount_feb, amount_mar, amount_apr,
		        amount_may, amount_jun, amount_jul, amount_aug,
		        amount_sep, amount_oct, amount_nov, amount_dec,
		        annual_total, created_at, updated_at
		 FROM budget_lines
		 WHERE tenant_id = $1 AND budget_id = $2
		 ORDER BY account_code ASC, COALESCE(cost_center, '') ASC`,
		tenantID, budgetID,
	)
	if err != nil {
		return nil, fmt.Errorf("finance: list budget_lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b BudgetLine
		if err := rows.Scan(
			&b.TenantID, &b.ID, &b.BudgetID, &b.AccountCode, &b.CostCenter,
			&b.Months[0], &b.Months[1], &b.Months[2], &b.Months[3],
			&b.Months[4], &b.Months[5], &b.Months[6], &b.Months[7],
			&b.Months[8], &b.Months[9], &b.Months[10], &b.Months[11],
			&b.AnnualTotal, &b.CreatedAt, &b.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("finance: list budget_lines: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finance: list budget_lines: %w", err)
	}
	return out, nil
}

// DeleteBudgetLine removes a single line, scoped to the supplied
// budget parent. The budgetID is part of the call signature so the
// REST URL `/budgets/{budgetID}/lines/{lineID}` semantics are
// enforced at the store boundary: deleting a line via the URL of
// a different budget returns ErrBudgetLineNotFound rather than
// silently succeeding. Returns ErrBudgetLineNotFound when no row
// matches the (tenant, budget, id) triple.
func (s *BudgetStore) DeleteBudgetLine(ctx context.Context, tenantID, budgetID, id uuid.UUID) error {
	if tenantID == uuid.Nil || budgetID == uuid.Nil || id == uuid.Nil {
		return fmt.Errorf("%w: tenant_id, budget_id, and id required", ErrInvalidBudgetLine)
	}
	var rowCount int64
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`DELETE FROM budget_lines
			 WHERE tenant_id = $1 AND budget_id = $2 AND id = $3`,
			tenantID, budgetID, id,
		)
		if err != nil {
			return err
		}
		rowCount = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("finance: delete budget_line: %w", err)
	}
	if rowCount == 0 {
		return ErrBudgetLineNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Variance computation.
// ---------------------------------------------------------------------------

// VarianceQuery narrows a BudgetVsActual call. From/To bound the
// actuals window; an empty range defaults to the whole fiscal year
// of the supplied budget.
type VarianceQuery struct {
	BudgetID uuid.UUID
	From     time.Time
	To       time.Time
}

// BudgetVsActual returns the variance report for a single budget
// over the [From, To] window. Actuals come from journal_lines
// joined against accounts, aggregated by (account, cost_center,
// fiscal month). The signed convention used is:
//
//   - For expense accounts: budgeted = planned spend, actual =
//     sum(debit - credit); variance > 0 means OVER-spend (bad).
//   - For revenue accounts: budgeted = planned income, actual =
//     sum(credit - debit); variance < 0 means UNDER-perform (bad).
//   - For asset / liability / equity accounts: variance = actual -
//     budgeted, sign-as-recorded.
//
// The variance_pct field is variance / budgeted, returning
// decimal.Zero when budgeted is zero (avoids div-by-zero — a line
// with no plan still surfaces actuals so the user notices).
func (s *BudgetStore) BudgetVsActual(ctx context.Context, tenantID uuid.UUID, q VarianceQuery) (*VarianceReport, error) {
	if tenantID == uuid.Nil || q.BudgetID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id and budget_id required", ErrInvalidBudget)
	}

	var (
		budget      *Budget
		lines       []BudgetLine
		accountMeta map[string]accountMeta
		actuals     map[actualKey]decimal.Decimal
	)
	// Wrap the four reads in a single tenant tx so the report
	// observes a consistent snapshot. Without this a concurrent
	// budget line upsert (or a journal post landing mid-report)
	// could surface as a header-vs-lines or lines-vs-actuals
	// mismatch. The four helpers are pure functions of (tx,
	// tenant, ...) so the wrap is mechanical.
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := getBudgetTx(ctx, tx, tenantID, q.BudgetID)
		if err != nil {
			return err
		}
		budget = b
		if q.From.IsZero() {
			q.From = time.Date(budget.FiscalYear, time.January, 1, 0, 0, 0, 0, time.UTC)
		}
		if q.To.IsZero() {
			// End of fiscal year inclusive of the final nanosecond
			// so the `je.posted_at <= $to` filter in loadActualsTx
			// covers every entry posted on Dec 31 regardless of
			// sub-second precision (see budget_handlers.endOfDay).
			q.To = time.Date(budget.FiscalYear, time.December, 31, 23, 59, 59, int(time.Second-1), time.UTC)
		}
		lines, err = listBudgetLinesTx(ctx, tx, tenantID, q.BudgetID)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			// No lines to load metadata or actuals for; leave the
			// maps nil so the loop below short-circuits naturally.
			return nil
		}
		// Resolve account metadata (name + type) per code in a
		// single round-trip so the signed-variance logic and the
		// renderer don't issue per-line lookups.
		accountMeta, err = loadAccountMetaTx(ctx, tx, tenantID, distinctAccountCodes(lines))
		if err != nil {
			return err
		}
		// Aggregate actuals by (account_code, cost_center,
		// fiscal_month). fiscal_month is 1..12 in calendar order;
		// the caller's date range may exclude some months so we
		// left-outer-join from the budget side, not the actuals
		// side.
		actuals, err = loadActualsTx(ctx, tx, tenantID, q.From, q.To, budget.FiscalYear, lines)
		return err
	})
	if err != nil {
		return nil, err
	}

	report := &VarianceReport{
		TenantID:   tenantID,
		BudgetID:   q.BudgetID,
		BudgetName: budget.Name,
		FiscalYear: budget.FiscalYear,
		From:       q.From,
		To:         q.To,
		Rows:       []VarianceRow{},
	}
	if len(lines) == 0 {
		return report, nil
	}

	for i := range lines {
		line := &lines[i]
		// Resolve effective cost_center for this line. The three
		// cases here encode an intentional asymmetry that finance
		// users sometimes ask about:
		//
		//   line.CostCenter == ""  AND  budget.CostCenter == ""
		//     → no CC scope at all on either side. Lookup uses the
		//       wildcard bucket which the SQL also writes into, so
		//       this line aggregates actuals across EVERY cost
		//       center — the "default unfiled budget" semantic.
		//
		//   line.CostCenter == ""  AND  budget.CostCenter != ""
		//     → the budget header pins the whole budget to a single
		//       CC (e.g. "MARKETING"). The line inherits that CC
		//       and looks up actuals ONLY in the "MARKETING" bucket;
		//       a journal entry posted with no cost_center goes to
		//       the "" bucket and is deliberately NOT counted —
		//       posting expense to a CC-scoped budget without
		//       tagging the CC is treated as a data-entry error,
		//       not a silent absorb.
		//
		//   line.CostCenter != ""
		//     → the line's own CC overrides any header default and
		//       the lookup queries that exact CC bucket.
		ccLabel := line.CostCenter
		if ccLabel == "" {
			ccLabel = budget.CostCenter
		}
		lookupCC := ccLabel
		if lookupCC == "" {
			lookupCC = wildcardCostCenter
		}
		meta := accountMeta[line.AccountCode]
		accountType := meta.Type
		for monthIdx := 0; monthIdx < 12; monthIdx++ {
			month := time.Month(monthIdx + 1)
			periodLabel := fmt.Sprintf("%04d-%02d", budget.FiscalYear, monthIdx+1)
			// Skip months entirely outside the requested window.
			monthStart := time.Date(budget.FiscalYear, month, 1, 0, 0, 0, 0, time.UTC)
			monthEnd := monthStart.AddDate(0, 1, 0).Add(-time.Nanosecond)
			if monthEnd.Before(q.From) || monthStart.After(q.To) {
				continue
			}
			budgeted := line.Months[monthIdx]
			actual := actuals[actualKey{line.AccountCode, lookupCC, monthIdx + 1}]
			if isCreditNormal(accountType) {
				// Revenue accounts: actuals are credit-positive,
				// so the variance is sign-flipped to "vs plan".
				actual = actual.Neg()
			}
			variance := actual.Sub(budgeted)
			variancePct := decimal.Zero
			if !budgeted.IsZero() {
				variancePct = variance.Div(budgeted).Round(4)
			}
			fav := isFavourableVariance(accountType, variance)
			report.Rows = append(report.Rows, VarianceRow{
				BudgetID:    q.BudgetID,
				AccountCode: line.AccountCode,
				AccountName: meta.Name,
				AccountType: accountType,
				CostCenter:  ccLabel,
				Period:      periodLabel,
				Budgeted:    budgeted,
				Actual:      actual,
				Variance:    variance,
				VariancePct: variancePct,
				Favourable:  fav,
			})
			report.TotalBudgeted = report.TotalBudgeted.Add(budgeted)
			report.TotalActual = report.TotalActual.Add(actual)
			report.TotalVariance = report.TotalVariance.Add(variance)
			if fav {
				report.TotalFavourableVariance = report.TotalFavourableVariance.Add(variance.Abs())
			} else {
				report.TotalUnfavourableVariance = report.TotalUnfavourableVariance.Add(variance.Abs())
			}
		}
	}
	return report, nil
}

// actualKey is the composite key the actuals map is sharded on —
// (account_code, cost_center, fiscal_month_1based). The empty
// cost_center maps to the budget header's default CC at the report
// layer; we keep them separate inside the map so the caller can
// distinguish "no CC on the JL" from "wildcard CC on the line".
type actualKey struct {
	AccountCode string
	CostCenter  string
	Month       int
}

// loadActualsTx is the transaction-bound helper invoked by
// BudgetVsActual so the actuals aggregation observes the same
// snapshot as the budget header / lines reads above. It is unused
// outside the variance report path; if a caller ever needs the
// public-method form it can wrap this in dbutil.WithTenantTx the
// way the other store methods do.
func loadActualsTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, from, to time.Time, fiscalYear int, lines []BudgetLine) (map[actualKey]decimal.Decimal, error) {
	codes := distinctAccountCodes(lines)
	if len(codes) == 0 {
		return map[actualKey]decimal.Decimal{}, nil
	}
	out := make(map[actualKey]decimal.Decimal, len(lines)*4)
	// The fiscal_year filter guards against cross-year date ranges
	// (e.g. From=2024-11 .. To=2025-02 for a FY2025 budget)
	// misattributing November-2024 entries to the FY2025 November
	// budget line: EXTRACT(MONTH FROM posted_at) alone loses the
	// year component. By constraining EXTRACT(YEAR ...) to the
	// budget's fiscal year, any actuals outside the budget's plan
	// window are excluded regardless of how generous the caller's
	// From / To range is. (The budget itself is calendar-FY today
	// — see the BudgetLine doc comment for the non-calendar FY
	// extension point.)
	rows, err := tx.Query(ctx,
		`SELECT jl.account_code,
		        COALESCE(jl.cost_center, ''),
		        EXTRACT(MONTH FROM je.posted_at)::int AS month,
		        SUM(jl.debit - jl.credit)
		   FROM journal_lines jl
		   JOIN journal_entries je
		     ON je.tenant_id = jl.tenant_id
		    AND je.id = jl.entry_id
		  WHERE jl.tenant_id = $1
		    AND je.posted_at >= $2
		    AND je.posted_at <= $3
		    AND jl.account_code = ANY($4)
		    AND EXTRACT(YEAR FROM je.posted_at)::int = $5
		  GROUP BY jl.account_code, COALESCE(jl.cost_center, ''), month`,
		tenantID, from, to, codes, fiscalYear,
	)
	if err != nil {
		return nil, fmt.Errorf("finance: load actuals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code  string
			cc    string
			month int
			sum   decimal.Decimal
		)
		if err := rows.Scan(&code, &cc, &month, &sum); err != nil {
			return nil, fmt.Errorf("finance: load actuals: %w", err)
		}
		out[actualKey{code, cc, month}] = sum
		// Also accumulate into the wildcard bucket so a budget
		// line with no cost_center scope still matches actuals
		// posted with explicit CCs. The wildcardCostCenter
		// sentinel is reserved and cannot appear on a real
		// journal line (the SQL groups by
		// COALESCE(cost_center,'') which produces "" not the
		// sentinel).
		wild := actualKey{code, wildcardCostCenter, month}
		out[wild] = out[wild].Add(sum)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finance: load actuals: %w", err)
	}
	return out, nil
}

// wildcardCostCenter is the reserved sentinel used as the actualKey
// CostCenter when a budget line has no CC scope. Because the SQL in
// loadActuals applies COALESCE(cost_center, '') the bucket "*" can
// never collide with a real CC value coming back from the DB.
const wildcardCostCenter = "*"

// accountMeta carries the per-account chart-of-accounts attributes
// needed by the variance renderer: type drives the credit-normal
// sign flip and the favourability rule, name surfaces on each
// VarianceRow so the frontend can render "4000 — Sales Revenue"
// instead of an opaque account code.
type accountMeta struct {
	Name string
	Type string
}

// loadAccountMetaTx is the transaction-bound helper invoked by
// BudgetVsActual. Like loadActualsTx it has no public-method
// counterpart today because no other call site needs to look up
// account metadata in isolation.
func loadAccountMetaTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, codes []string) (map[string]accountMeta, error) {
	out := make(map[string]accountMeta, len(codes))
	if len(codes) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT code, name, type FROM accounts
		 WHERE tenant_id = $1 AND code = ANY($2)`,
		tenantID, codes,
	)
	if err != nil {
		return nil, fmt.Errorf("finance: load account meta: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code, name, typ string
		if err := rows.Scan(&code, &name, &typ); err != nil {
			return nil, fmt.Errorf("finance: load account meta: %w", err)
		}
		out[code] = accountMeta{Name: name, Type: typ}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finance: load account meta: %w", err)
	}
	return out, nil
}

func distinctAccountCodes(lines []BudgetLine) []string {
	seen := make(map[string]struct{}, len(lines))
	out := make([]string, 0, len(lines))
	for i := range lines {
		code := lines[i].AccountCode
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	return out
}

// isCreditNormal reports whether the account type carries a credit
// balance in the natural reading of the ledger. Revenue and
// liability / equity accounts are credit-normal; asset and expense
// accounts are debit-normal.
func isCreditNormal(accountType string) bool {
	switch accountType {
	case "revenue", "liability", "equity":
		return true
	}
	return false
}

// isFavourableVariance reports whether a sign-normalised variance
// (positive = exceeded plan) represents a better-than-plan outcome
// for the given account type. Revenue: exceeding plan is favourable
// (good). Expense: exceeding plan is unfavourable (bad). Asset /
// liability / equity: variance is treated favourably when >= 0,
// matching the as-recorded sign convention used by the row's
// Variance field. A zero variance is reported as favourable so it
// rolls into the optimistic bucket rather than appearing as a
// regression.
func isFavourableVariance(accountType string, variance decimal.Decimal) bool {
	switch accountType {
	case "revenue":
		return !variance.IsNegative()
	case "expense":
		return !variance.IsPositive()
	}
	return !variance.IsNegative()
}

// ---------------------------------------------------------------------------
// Variance alert handler.
// ---------------------------------------------------------------------------

// VarianceAlertHandler is the scheduler.ActionHandler that sweeps
// every active budget for the tenant once per day, computes
// month-to-date variance against the current calendar month, and
// emits a `budget_variance` notification when the variance fraction
// crosses the budget's threshold (or the platform default).
type VarianceAlertHandler struct {
	store     *BudgetStore
	notify    *notifications.Store
	now       func() time.Time
	threshold decimal.Decimal
}

// NewVarianceAlertHandler wires the handler.
func NewVarianceAlertHandler(store *BudgetStore, notify *notifications.Store) *VarianceAlertHandler {
	return &VarianceAlertHandler{
		store:     store,
		notify:    notify,
		now:       func() time.Time { return time.Now().UTC() },
		threshold: DefaultVarianceThreshold,
	}
}

// WithClock substitutes the handler's time source.
func (h *VarianceAlertHandler) WithClock(now func() time.Time) *VarianceAlertHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// WithDefaultThreshold overrides the platform-default threshold.
func (h *VarianceAlertHandler) WithDefaultThreshold(t decimal.Decimal) *VarianceAlertHandler {
	if t.IsPositive() {
		h.threshold = t
	}
	return h
}

// Handle implements scheduler.ActionHandler. Errors on a single
// budget are logged-and-skipped so one bad row does not stall the
// whole sweep.
func (h *VarianceAlertHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.store == nil {
		return errors.New("finance: variance alert handler not wired")
	}
	budgets, err := h.store.ListBudgets(ctx, tenantID)
	if err != nil {
		return err
	}
	now := h.now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := range budgets {
		b := &budgets[i]
		if b.Status != BudgetStatusActive {
			continue
		}
		// Only emit alerts for the budget whose fiscal window
		// currently contains `now`. The fiscal window is computed
		// via budgetFiscalWindow so the same definition is used
		// everywhere — when non-calendar fiscal years are added
		// later (April-March, July-June, etc.), the gate updates
		// automatically.
		windowStart, windowEnd := budgetFiscalWindow(b)
		if now.Before(windowStart) || !now.Before(windowEnd) {
			continue
		}
		threshold := h.threshold
		if b.VarianceThreshold != nil && b.VarianceThreshold.IsPositive() {
			threshold = *b.VarianceThreshold
		}
		report, err := h.store.BudgetVsActual(ctx, tenantID, VarianceQuery{
			BudgetID: b.ID,
			From:     monthStart,
			To:       now,
		})
		if err != nil {
			log.Printf("finance: variance alert: budget %s: %v", b.ID, err)
			continue
		}
		for j := range report.Rows {
			row := &report.Rows[j]
			if !rowMatchesCurrentMonth(*row, now) {
				continue
			}
			if row.VariancePct.Abs().LessThan(threshold) {
				continue
			}
			if h.notify != nil {
				payload, _ := json.Marshal(map[string]any{
					"budget_id":     b.ID,
					"budget_name":   b.Name,
					"account_code":  row.AccountCode,
					"cost_center":   row.CostCenter,
					"period":        row.Period,
					"budgeted":      row.Budgeted.String(),
					"actual":        row.Actual.String(),
					"variance":      row.Variance.String(),
					"variance_pct":  row.VariancePct.String(),
					"threshold":     threshold.String(),
				})
				_, _ = h.notify.Create(ctx, notifications.CreateInput{
					TenantID: tenantID,
					Type:     "budget_variance",
					Title:    fmt.Sprintf("Budget variance: %s / %s", b.Name, row.AccountCode),
					Body:     fmt.Sprintf("MTD variance %s on %s (%s vs plan %s).", row.VariancePct.Mul(decimal.NewFromInt(100)).Round(1).String()+"%", row.AccountCode, row.Actual.String(), row.Budgeted.String()),
					Payload:  payload,
				})
			}
		}
	}
	return nil
}

// rowMatchesCurrentMonth returns true when the row.Period label
// ("YYYY-MM") encodes the same month as `now`. Used by Handle to
// only emit notifications for the in-progress fiscal month — past
// months are reported on the dashboard but are not actionable.
func rowMatchesCurrentMonth(row VarianceRow, now time.Time) bool {
	want := fmt.Sprintf("%04d-%02d", now.Year(), int(now.Month()))
	return row.Period == want
}

// budgetFiscalWindow returns the half-open `[start, end)` window
// during which a budget is considered "in flight" — i.e. the
// interval over which the variance alerter should still emit
// notifications. The MVP treats every budget as a calendar-year
// budget (Jan 1 → Jan 1 next year). When non-calendar fiscal
// years are wired through (first-fiscal-month per budget or per
// tenant), this helper is the single place that needs to learn
// about the new start month — the alerter, the dashboard, and
// any future scheduled-period reports all go through this.
func budgetFiscalWindow(b *Budget) (start, end time.Time) {
	start = time.Date(b.FiscalYear, time.January, 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(1, 0, 0)
	return start, end
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// nullIfEmpty returns nil for an empty string so the underlying
// column stores NULL rather than '' — matches the existing
// nullIfEmpty in internal/ledger.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
