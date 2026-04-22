package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// TrialBalanceRow is a single per-account row of the trial balance.
// Debit / Credit are the sums for the account up to AsOf; Balance is
// a signed number (positive for debit balances, negative for credit).
type TrialBalanceRow struct {
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name"`
	Type        string          `json:"type"`
	Debit       decimal.Decimal `json:"debit"`
	Credit      decimal.Decimal `json:"credit"`
	Balance     decimal.Decimal `json:"balance"`
}

// TrialBalance is the report-level aggregate. TotalDebit and
// TotalCredit MUST be equal for a well-formed ledger; the engine
// surfaces the residual so integration tests can assert = 0 cheaply.
type TrialBalance struct {
	TenantID    uuid.UUID         `json:"tenant_id"`
	AsOf        time.Time         `json:"as_of"`
	Rows        []TrialBalanceRow `json:"rows"`
	TotalDebit  decimal.Decimal   `json:"total_debit"`
	TotalCredit decimal.Decimal   `json:"total_credit"`
	Residual    decimal.Decimal   `json:"residual"`
}

// AgingBucket is a coarse-grained outstanding-invoice bucket.
type AgingBucket struct {
	Label  string          `json:"label"`  // "current", "1-30", "31-60", "61-90", "90+"
	Amount decimal.Decimal `json:"amount"`
	Count  int             `json:"count"`
}

// AgingRow is a per-customer (AR) or per-supplier (AP) aging row.
type AgingRow struct {
	PartyID string          `json:"party_id"`
	Total   decimal.Decimal `json:"total"`
	Buckets []AgingBucket   `json:"buckets"`
}

// AgingReport wraps the per-party rows and the report-level totals.
type AgingReport struct {
	TenantID uuid.UUID       `json:"tenant_id"`
	AsOf     time.Time       `json:"as_of"`
	Rows     []AgingRow      `json:"rows"`
	Totals   []AgingBucket   `json:"totals"`
	Total    decimal.Decimal `json:"total"`
}

// IncomeStatementRow is a single income-statement line.
type IncomeStatementRow struct {
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name"`
	Amount      decimal.Decimal `json:"amount"`
}

// IncomeStatement summarises revenue / expense over a period. NetIncome
// = TotalRevenue - TotalExpense.
type IncomeStatement struct {
	TenantID     uuid.UUID            `json:"tenant_id"`
	From         time.Time            `json:"from"`
	To           time.Time            `json:"to"`
	Revenue      []IncomeStatementRow `json:"revenue"`
	Expense      []IncomeStatementRow `json:"expense"`
	TotalRevenue decimal.Decimal      `json:"total_revenue"`
	TotalExpense decimal.Decimal      `json:"total_expense"`
	NetIncome    decimal.Decimal      `json:"net_income"`
}

// ---------------------------------------------------------------------------
// Trial balance
// ---------------------------------------------------------------------------

// TrialBalance returns the per-account debit/credit totals up to asOf.
// For a well-formed ledger Residual = 0; a non-zero residual indicates
// a posted entry slipped past validateLines (integration-test guard).
func (s *PGStore) TrialBalance(ctx context.Context, tenantID uuid.UUID, asOf time.Time) (*TrialBalance, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if asOf.IsZero() {
		asOf = s.now()
	}

	tb := &TrialBalance{TenantID: tenantID, AsOf: asOf, Rows: []TrialBalanceRow{}}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// We pre-filter journal_entries in a CTE so the subsequent
		// LEFT JOIN from accounts never eliminates a chart-of-accounts
		// row whose only journal lines happen to belong to an entry
		// posted after asOf. Those accounts legitimately have a zero
		// balance for the report and must still appear.
		rows, err := tx.Query(ctx,
			`WITH in_range AS (
			     SELECT id FROM journal_entries
			     WHERE tenant_id = $1
			       AND posted_at <= $2
			 )
			 SELECT a.code, a.name, a.type,
			        COALESCE(SUM(jl.debit), 0)  AS debit,
			        COALESCE(SUM(jl.credit), 0) AS credit
			 FROM accounts a
			 LEFT JOIN journal_lines jl
			   ON jl.tenant_id = a.tenant_id
			  AND jl.account_code = a.code
			  AND jl.entry_id IN (SELECT id FROM in_range)
			 WHERE a.tenant_id = $1
			 GROUP BY a.code, a.name, a.type
			 ORDER BY a.code`,
			tenantID, asOf,
		)
		if err != nil {
			return fmt.Errorf("ledger: trial balance query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r TrialBalanceRow
			if err := rows.Scan(&r.AccountCode, &r.AccountName, &r.Type, &r.Debit, &r.Credit); err != nil {
				return fmt.Errorf("ledger: scan trial balance: %w", err)
			}
			r.Balance = r.Debit.Sub(r.Credit)
			tb.Rows = append(tb.Rows, r)
			tb.TotalDebit = tb.TotalDebit.Add(r.Debit)
			tb.TotalCredit = tb.TotalCredit.Add(r.Credit)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	tb.Residual = tb.TotalDebit.Sub(tb.TotalCredit)
	return tb, nil
}

// ---------------------------------------------------------------------------
// AR / AP aging
// ---------------------------------------------------------------------------

// agingCandidate is a row pulled from the krecords table for aging
// calculation. Payment status is tracked on the invoice KRecord
// itself (status=paid) so the aging report simply excludes those.
type agingCandidate struct {
	party   string
	total   decimal.Decimal
	dueDate time.Time
	hasDue  bool
}

// ARAgingReport groups posted-but-unpaid finance.ar_invoice KRecords by
// customer and aging bucket. Uses invoice.due_date (or issue_date when
// absent) to compute age relative to asOf.
func (s *PGStore) ARAgingReport(ctx context.Context, tenantID uuid.UUID, asOf time.Time) (*AgingReport, error) {
	return s.agingReport(ctx, tenantID, asOf, "finance.ar_invoice", "customer_id")
}

// APAgingReport is the supplier-side equivalent of ARAgingReport.
func (s *PGStore) APAgingReport(ctx context.Context, tenantID uuid.UUID, asOf time.Time) (*AgingReport, error) {
	return s.agingReport(ctx, tenantID, asOf, "finance.ap_bill", "supplier_id")
}

func (s *PGStore) agingReport(ctx context.Context, tenantID uuid.UUID, asOf time.Time, ktype, partyField string) (*AgingReport, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if asOf.IsZero() {
		asOf = s.now()
	}

	// Posted + not yet paid. status=cancelled also excluded. We read
	// from krecords because the invoice/bill lifecycle (posted → paid)
	// is tracked on the KRecord, not on the ledger.
	candidates := make([]agingCandidate, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT data
			 FROM krecords
			 WHERE tenant_id = $1
			   AND ktype = $2
			   AND status = 'active'
			   AND data->>'status' IN ('posted', 'pending_approval', 'draft')
			   AND COALESCE(data->>'status', '') != 'paid'
			   AND COALESCE(data->>'status', '') != 'cancelled'`,
			tenantID, ktype,
		)
		if err != nil {
			return fmt.Errorf("ledger: aging query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var data json.RawMessage
			if err := rows.Scan(&data); err != nil {
				return fmt.Errorf("ledger: scan aging: %w", err)
			}
			c, ok := parseAgingRecord(data, partyField)
			if !ok {
				continue
			}
			candidates = append(candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	return buildAgingReport(tenantID, asOf, candidates), nil
}

func parseAgingRecord(raw json.RawMessage, partyField string) (agingCandidate, bool) {
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blob); err != nil {
		return agingCandidate{}, false
	}
	var party string
	if v, ok := blob[partyField]; ok {
		_ = json.Unmarshal(v, &party)
	}
	if party == "" {
		party = "unknown"
	}
	var status string
	if v, ok := blob["status"]; ok {
		_ = json.Unmarshal(v, &status)
	}
	// Include "posted" invoices/bills; exclude paid + cancelled in SQL.
	if status != "posted" {
		return agingCandidate{}, false
	}
	var total decimal.Decimal
	if v, ok := blob["total"]; ok {
		_ = json.Unmarshal(v, &total)
	}
	var due time.Time
	var hasDue bool
	if v, ok := blob["due_date"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		if t, err := time.Parse("2006-01-02", s); err == nil {
			due, hasDue = t.UTC(), true
		}
	}
	if !hasDue {
		if v, ok := blob["issue_date"]; ok {
			var s string
			_ = json.Unmarshal(v, &s)
			if t, err := time.Parse("2006-01-02", s); err == nil {
				due, hasDue = t.UTC(), true
			}
		}
	}
	return agingCandidate{party: party, total: total, dueDate: due, hasDue: hasDue}, true
}

func buildAgingReport(tenantID uuid.UUID, asOf time.Time, candidates []agingCandidate) *AgingReport {
	bucketLabels := []string{"current", "1-30", "31-60", "61-90", "90+"}
	perParty := make(map[string]map[string]*AgingBucket)
	partyTotals := make(map[string]decimal.Decimal)
	totalByBucket := map[string]*AgingBucket{}
	for _, l := range bucketLabels {
		totalByBucket[l] = &AgingBucket{Label: l}
	}

	for _, c := range candidates {
		label := "current"
		if c.hasDue {
			days := int(asOf.Sub(c.dueDate).Hours() / 24)
			switch {
			case days <= 0:
				label = "current"
			case days <= 30:
				label = "1-30"
			case days <= 60:
				label = "31-60"
			case days <= 90:
				label = "61-90"
			default:
				label = "90+"
			}
		}
		if _, ok := perParty[c.party]; !ok {
			perParty[c.party] = map[string]*AgingBucket{}
			for _, l := range bucketLabels {
				perParty[c.party][l] = &AgingBucket{Label: l}
			}
		}
		perParty[c.party][label].Amount = perParty[c.party][label].Amount.Add(c.total)
		perParty[c.party][label].Count++
		partyTotals[c.party] = partyTotals[c.party].Add(c.total)

		totalByBucket[label].Amount = totalByBucket[label].Amount.Add(c.total)
		totalByBucket[label].Count++
	}

	report := &AgingReport{
		TenantID: tenantID,
		AsOf:     asOf,
		Rows:     []AgingRow{},
		Totals:   []AgingBucket{},
	}
	for party, buckets := range perParty {
		row := AgingRow{PartyID: party, Total: partyTotals[party]}
		for _, label := range bucketLabels {
			row.Buckets = append(row.Buckets, *buckets[label])
		}
		report.Rows = append(report.Rows, row)
		report.Total = report.Total.Add(partyTotals[party])
	}
	for _, label := range bucketLabels {
		report.Totals = append(report.Totals, *totalByBucket[label])
	}
	return report
}

// ---------------------------------------------------------------------------
// Income statement
// ---------------------------------------------------------------------------

// IncomeStatement aggregates revenue and expense posting activity
// between [from, to]. Revenue accounts carry credit-normal balances so
// we surface credit - debit; expense accounts are debit-normal so we
// surface debit - credit. NetIncome = TotalRevenue - TotalExpense.
func (s *PGStore) IncomeStatement(ctx context.Context, tenantID uuid.UUID, from, to time.Time) (*IncomeStatement, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil, errors.New("ledger: invalid report window")
	}

	out := &IncomeStatement{
		TenantID: tenantID, From: from, To: to,
		Revenue: []IncomeStatementRow{},
		Expense: []IncomeStatementRow{},
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// CTE pre-filter mirrors TrialBalance — a revenue/expense
		// account with no in-range activity should still appear with
		// a zero amount so the statement reflects the full chart.
		rows, err := tx.Query(ctx,
			`WITH in_range AS (
			     SELECT id FROM journal_entries
			     WHERE tenant_id = $1
			       AND posted_at BETWEEN $2 AND $3
			 )
			 SELECT a.code, a.name, a.type,
			        COALESCE(SUM(jl.debit), 0)  AS debit,
			        COALESCE(SUM(jl.credit), 0) AS credit
			 FROM accounts a
			 LEFT JOIN journal_lines jl
			   ON jl.tenant_id = a.tenant_id
			  AND jl.account_code = a.code
			  AND jl.entry_id IN (SELECT id FROM in_range)
			 WHERE a.tenant_id = $1
			   AND a.type IN ('revenue', 'expense')
			 GROUP BY a.code, a.name, a.type
			 ORDER BY a.type, a.code`,
			tenantID, from, to,
		)
		if err != nil {
			return fmt.Errorf("ledger: income statement query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				code, name, typ string
				debit, credit   decimal.Decimal
			)
			if err := rows.Scan(&code, &name, &typ, &debit, &credit); err != nil {
				return fmt.Errorf("ledger: scan income statement: %w", err)
			}
			if typ == AccountTypeRevenue {
				amt := credit.Sub(debit)
				out.Revenue = append(out.Revenue, IncomeStatementRow{AccountCode: code, AccountName: name, Amount: amt})
				out.TotalRevenue = out.TotalRevenue.Add(amt)
			} else {
				amt := debit.Sub(credit)
				out.Expense = append(out.Expense, IncomeStatementRow{AccountCode: code, AccountName: name, Amount: amt})
				out.TotalExpense = out.TotalExpense.Add(amt)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out.NetIncome = out.TotalRevenue.Sub(out.TotalExpense)
	return out, nil
}
