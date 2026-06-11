package ledger

import (
	"sort"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// consolidation_translate.go holds the pure (DB-free) heart of the
// multi-entity consolidation: currency translation, cross-entity
// summation, intercompany elimination, and the Cumulative
// Translation Adjustment (CTA) balancing plug. Keeping it free of
// any pool/transaction dependency lets the math be unit-tested
// exhaustively without a database.

// entityTrialBalance is one member entity's per-account trial
// balance (in its own base currency) plus the two rates used to
// translate it into the group's presentation currency.
type entityTrialBalance struct {
	tenantID     uuid.UUID
	baseCurrency string
	// closingRate translates balance-sheet accounts (asset,
	// liability, equity) at the period-end spot rate.
	closingRate decimal.Decimal
	// averageRate translates income-statement accounts (revenue,
	// expense) at the period average rate. Equal to closingRate when
	// no separate average is supplied.
	averageRate decimal.Decimal
	rows        []TrialBalanceRow
}

// isIncomeStatementType reports whether an account type is a P&L
// (income-statement) account translated at the average rate.
func isIncomeStatementType(t string) bool {
	return t == AccountTypeRevenue || t == AccountTypeExpense
}

// ctaAccount resolves the CTA account code, falling back to the
// package default when a group leaves it unset.
func ctaAccount(code string) string {
	if code == "" {
		return AccountCodeCTA
	}
	return code
}

// consolidate is the pure consolidation pipeline. It:
//
//  1. Translates every entity's rows into the presentation currency
//     (closing rate for balance-sheet accounts, average rate for
//     income-statement accounts) and books a per-entity CTA equity
//     adjustment equal to that entity's translation residual, so
//     each translated entity remains internally balanced.
//  2. Sums contributions across entities into one ConsolidatedRow
//     per account code, retaining per-tenant Contributions for
//     drill-down.
//  3. Eliminates ONLY the matched intercompany contributions named
//     by each EliminationPair (the from-tenant's leg on its account
//     and the to-tenant's leg on its account), leaving third-party
//     balances on the same account code intact.
//  4. Folds any residual left by an FX mismatch between the two
//     eliminated legs into the CTA so the combined trial balance
//     balances exactly (TotalDebit == TotalCredit).
//
// The returned ConsolidatedTrialBalance has GroupID/AsOf/Presentation
// currency left zero for the caller to stamp.
func consolidate(entities []entityTrialBalance, pairs []EliminationPair, ctaCode string) *ConsolidatedTrialBalance {
	combined := map[string]*ConsolidatedRow{}

	add := func(code, name, typ string, tenant uuid.UUID, debit, credit decimal.Decimal) {
		c, ok := combined[code]
		if !ok {
			c = &ConsolidatedRow{AccountCode: code}
			combined[code] = c
		}
		if c.Type == "" {
			c.Type = typ
		}
		if c.AccountName == "" {
			c.AccountName = name
		}
		c.Debit = c.Debit.Add(debit)
		c.Credit = c.Credit.Add(credit)
		c.Balance = c.Debit.Sub(c.Credit)
		c.Contributions = append(c.Contributions, TenantBalanceRow{TenantID: tenant, Debit: debit, Credit: credit})
	}

	// 1 + 2 — translate and accumulate, with a per-entity CTA so each
	// entity's translated trial balance stays balanced.
	for _, e := range entities {
		entityDebit := decimal.Zero
		entityCredit := decimal.Zero
		for _, row := range e.rows {
			rate := e.closingRate
			if isIncomeStatementType(row.Type) {
				rate = e.averageRate
			}
			debit := row.Debit.Mul(rate)
			credit := row.Credit.Mul(rate)
			add(row.AccountCode, row.AccountName, row.Type, e.tenantID, debit, credit)
			entityDebit = entityDebit.Add(debit)
			entityCredit = entityCredit.Add(credit)
		}
		if cta := entityDebit.Sub(entityCredit); !cta.IsZero() {
			// residual > 0 means translated debits exceed credits, so
			// a CTA credit restores balance (and vice-versa).
			d, c := decimal.Zero, decimal.Zero
			if cta.IsPositive() {
				c = cta
			} else {
				d = cta.Neg()
			}
			add(ctaCode, "Cumulative Translation Adjustment", AccountTypeEquity, e.tenantID, d, c)
		}
	}

	// 3 — intercompany elimination. Remove only the named tenants'
	// contributions to the named accounts; record what was removed.
	eliminated := map[string]*ConsolidatedRow{}
	recordElim := func(code, name, typ string, tenant uuid.UUID, d, c decimal.Decimal) {
		er, ok := eliminated[code]
		if !ok {
			er = &ConsolidatedRow{AccountCode: code, AccountName: name, Type: typ}
			eliminated[code] = er
		}
		er.Debit = er.Debit.Add(d)
		er.Credit = er.Credit.Add(c)
		er.Balance = er.Debit.Sub(er.Credit)
		er.Contributions = append(er.Contributions, TenantBalanceRow{TenantID: tenant, Debit: d, Credit: c})
	}
	eliminateLeg := func(code string, tenant uuid.UUID) {
		c := combined[code]
		if c == nil {
			return
		}
		idx := -1
		for i, contr := range c.Contributions {
			if contr.TenantID == tenant {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		contr := c.Contributions[idx]
		c.Debit = c.Debit.Sub(contr.Debit)
		c.Credit = c.Credit.Sub(contr.Credit)
		c.Balance = c.Debit.Sub(c.Credit)
		c.Contributions = append(c.Contributions[:idx], c.Contributions[idx+1:]...)
		recordElim(code, c.AccountName, c.Type, tenant, contr.Debit, contr.Credit)
		if len(c.Contributions) == 0 {
			delete(combined, code)
		}
	}
	for _, p := range pairs {
		from, to := p.from(), p.to()
		if from == "" && to == "" {
			continue
		}
		if from != "" {
			eliminateLeg(from, p.FromTenant)
		}
		if to != "" {
			eliminateLeg(to, p.ToTenant)
		}
	}

	// 4 — fold any post-elimination residual (an FX mismatch between
	// the two eliminated legs) into the CTA so the combined TB
	// balances exactly. Attributed to the nil tenant because it is a
	// group-level reconciling item, not any one entity's adjustment.
	totalD, totalC := sumRows(combined)
	if residual := totalD.Sub(totalC); !residual.IsZero() {
		d, c := decimal.Zero, decimal.Zero
		if residual.IsPositive() {
			c = residual
		} else {
			d = residual.Neg()
		}
		add(ctaCode, "Cumulative Translation Adjustment", AccountTypeEquity, uuid.Nil, d, c)
	}

	out := &ConsolidatedTrialBalance{
		Rows:       make([]ConsolidatedRow, 0, len(combined)),
		Eliminated: make([]ConsolidatedRow, 0, len(eliminated)),
	}
	for _, c := range combined {
		out.Rows = append(out.Rows, *c)
		out.TotalDebit = out.TotalDebit.Add(c.Debit)
		out.TotalCredit = out.TotalCredit.Add(c.Credit)
	}
	for _, er := range eliminated {
		out.Eliminated = append(out.Eliminated, *er)
	}
	sortRows(out.Rows)
	sortRows(out.Eliminated)
	out.Residual = out.TotalDebit.Sub(out.TotalCredit)
	if cta := combined[ctaCode]; cta != nil {
		out.CTA = cta.Credit.Sub(cta.Debit)
	}
	return out
}

// sumRows totals debit and credit across a combined-row map.
func sumRows(combined map[string]*ConsolidatedRow) (totalDebit, totalCredit decimal.Decimal) {
	for _, row := range combined {
		totalDebit = totalDebit.Add(row.Debit)
		totalCredit = totalCredit.Add(row.Credit)
	}
	return totalDebit, totalCredit
}

// sortRows orders rows by account code so the report (and its
// persisted JSON) is deterministic regardless of Go map iteration
// order.
func sortRows(rows []ConsolidatedRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].AccountCode < rows[j].AccountCode })
}
