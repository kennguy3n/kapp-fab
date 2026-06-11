package ledger

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// consolidation_statements.go derives the three consolidated
// financial statements — trial balance, income statement (P&L), and
// balance sheet — from a single balanced ConsolidatedTrialBalance.
//
// Because the consolidated trial balance is forced to balance (the
// CTA equity plug absorbs every translation and elimination
// difference), the accounting identity
//
//	Assets = Liabilities + Equity + NetIncome
//
// holds exactly, so the derived balance sheet always balances. The
// statements reuse the already-translated, already-eliminated rows,
// so currency translation and intercompany elimination are applied
// consistently across all three reports.

// ConsolidatedStatementRow is one line of a consolidated P&L or
// balance-sheet section. Amount is normalised to the section's
// natural sign (debit-positive for assets/expenses, credit-positive
// for liabilities/equity/revenue).
type ConsolidatedStatementRow struct {
	AccountCode string          `json:"account_code"`
	AccountName string          `json:"account_name,omitempty"`
	Amount      decimal.Decimal `json:"amount"`
}

// ConsolidatedIncomeStatement is the group P&L. NetIncome =
// TotalRevenue − TotalExpense.
type ConsolidatedIncomeStatement struct {
	GroupID              uuid.UUID                  `json:"group_id"`
	AsOf                 time.Time                  `json:"as_of"`
	PresentationCurrency string                     `json:"presentation_currency"`
	Revenue              []ConsolidatedStatementRow `json:"revenue"`
	Expense              []ConsolidatedStatementRow `json:"expense"`
	TotalRevenue         decimal.Decimal            `json:"total_revenue"`
	TotalExpense         decimal.Decimal            `json:"total_expense"`
	NetIncome            decimal.Decimal            `json:"net_income"`
}

// ConsolidatedBalanceSheet is the group balance sheet. The period's
// net income is surfaced as a synthetic "Current Period Earnings"
// equity line (P&L accounts do not themselves sit on the balance
// sheet), so TotalEquity already includes it. Balanced reports
// whether Assets == Liabilities + Equity (Difference == 0).
type ConsolidatedBalanceSheet struct {
	GroupID              uuid.UUID                  `json:"group_id"`
	AsOf                 time.Time                  `json:"as_of"`
	PresentationCurrency string                     `json:"presentation_currency"`
	Assets               []ConsolidatedStatementRow `json:"assets"`
	Liabilities          []ConsolidatedStatementRow `json:"liabilities"`
	Equity               []ConsolidatedStatementRow `json:"equity"`
	TotalAssets          decimal.Decimal            `json:"total_assets"`
	TotalLiabilities     decimal.Decimal            `json:"total_liabilities"`
	TotalEquity          decimal.Decimal            `json:"total_equity"`
	NetIncome            decimal.Decimal            `json:"net_income"`
	Difference           decimal.Decimal            `json:"difference"`
	Balanced             bool                       `json:"balanced"`
}

// ConsolidatedStatements bundles the trial balance plus the two
// derived statements so a single API call returns the whole pack.
type ConsolidatedStatements struct {
	TrialBalance    *ConsolidatedTrialBalance    `json:"trial_balance"`
	IncomeStatement *ConsolidatedIncomeStatement `json:"income_statement"`
	BalanceSheet    *ConsolidatedBalanceSheet    `json:"balance_sheet"`
}

// CurrentPeriodEarningsCode labels the synthetic equity line that
// carries net income onto the consolidated balance sheet.
const CurrentPeriodEarningsCode = "3999"

// buildConsolidatedIncomeStatement derives the group P&L from the
// consolidated trial balance. Revenue is credit-positive, expense is
// debit-positive; zero-amount accounts are omitted.
func buildConsolidatedIncomeStatement(tb *ConsolidatedTrialBalance) *ConsolidatedIncomeStatement {
	is := &ConsolidatedIncomeStatement{
		GroupID:              tb.GroupID,
		AsOf:                 tb.AsOf,
		PresentationCurrency: tb.PresentationCurrency,
		Revenue:              []ConsolidatedStatementRow{},
		Expense:              []ConsolidatedStatementRow{},
	}
	for _, r := range tb.Rows {
		switch r.Type {
		case AccountTypeRevenue:
			amt := r.Credit.Sub(r.Debit)
			if amt.IsZero() {
				continue
			}
			is.Revenue = append(is.Revenue, ConsolidatedStatementRow{AccountCode: r.AccountCode, AccountName: r.AccountName, Amount: amt})
			is.TotalRevenue = is.TotalRevenue.Add(amt)
		case AccountTypeExpense:
			amt := r.Debit.Sub(r.Credit)
			if amt.IsZero() {
				continue
			}
			is.Expense = append(is.Expense, ConsolidatedStatementRow{AccountCode: r.AccountCode, AccountName: r.AccountName, Amount: amt})
			is.TotalExpense = is.TotalExpense.Add(amt)
		}
	}
	is.NetIncome = is.TotalRevenue.Sub(is.TotalExpense)
	return is
}

// buildConsolidatedBalanceSheet derives the group balance sheet from
// the consolidated trial balance and the period net income. Assets
// are debit-positive; liabilities and equity are credit-positive.
// Net income is appended as a synthetic equity line so the statement
// balances against assets.
func buildConsolidatedBalanceSheet(tb *ConsolidatedTrialBalance, netIncome decimal.Decimal) *ConsolidatedBalanceSheet {
	bs := &ConsolidatedBalanceSheet{
		GroupID:              tb.GroupID,
		AsOf:                 tb.AsOf,
		PresentationCurrency: tb.PresentationCurrency,
		Assets:               []ConsolidatedStatementRow{},
		Liabilities:          []ConsolidatedStatementRow{},
		Equity:               []ConsolidatedStatementRow{},
		NetIncome:            netIncome,
	}
	for _, r := range tb.Rows {
		switch r.Type {
		case AccountTypeAsset:
			amt := r.Debit.Sub(r.Credit)
			if amt.IsZero() {
				continue
			}
			bs.Assets = append(bs.Assets, ConsolidatedStatementRow{AccountCode: r.AccountCode, AccountName: r.AccountName, Amount: amt})
			bs.TotalAssets = bs.TotalAssets.Add(amt)
		case AccountTypeLiability:
			amt := r.Credit.Sub(r.Debit)
			if amt.IsZero() {
				continue
			}
			bs.Liabilities = append(bs.Liabilities, ConsolidatedStatementRow{AccountCode: r.AccountCode, AccountName: r.AccountName, Amount: amt})
			bs.TotalLiabilities = bs.TotalLiabilities.Add(amt)
		case AccountTypeEquity:
			amt := r.Credit.Sub(r.Debit)
			if amt.IsZero() {
				continue
			}
			bs.Equity = append(bs.Equity, ConsolidatedStatementRow{AccountCode: r.AccountCode, AccountName: r.AccountName, Amount: amt})
			bs.TotalEquity = bs.TotalEquity.Add(amt)
		}
	}
	// Net income lives in the P&L accounts, which are not on the
	// balance sheet; surface it as retained earnings for the period
	// so equity reconciles against assets.
	if !netIncome.IsZero() {
		bs.Equity = append(bs.Equity, ConsolidatedStatementRow{
			AccountCode: CurrentPeriodEarningsCode,
			AccountName: "Current Period Earnings",
			Amount:      netIncome,
		})
		bs.TotalEquity = bs.TotalEquity.Add(netIncome)
	}
	bs.Difference = bs.TotalAssets.Sub(bs.TotalLiabilities.Add(bs.TotalEquity))
	bs.Balanced = bs.Difference.IsZero()
	return bs
}

// RunStatements runs the consolidation and returns the full
// statement pack (trial balance + P&L + balance sheet). It reuses
// RunConsolidationWithOptions so translation, elimination, and CTA
// handling are identical to a bare consolidation run.
func (s *ConsolidationStore) RunStatements(ctx context.Context, groupID, actor uuid.UUID, opts ConsolidationOptions) (*ConsolidatedStatements, error) {
	tb, err := s.RunConsolidationWithOptions(ctx, groupID, actor, opts)
	if err != nil {
		return nil, err
	}
	is := buildConsolidatedIncomeStatement(tb)
	bs := buildConsolidatedBalanceSheet(tb, is.NetIncome)
	return &ConsolidatedStatements{TrialBalance: tb, IncomeStatement: is, BalanceSheet: bs}, nil
}
