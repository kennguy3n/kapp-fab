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

// fx_revaluation.go is the canonical FX revaluation engine: it
// revalues every open foreign-currency monetary balance (AR/AP and
// other asset/liability accounts) at a period-end rate and posts the
// unrealized gain/loss to configurable adjustment accounts. The
// scheduled UnrealizedGainLossJob and the on-demand API runner both
// delegate to runFXRevaluation so there is a single, well-tested
// implementation.

// RevaluationConfig tunes which accounts a revaluation run posts to
// and which accounts it sweeps. The gain/loss accounts default to
// the package constants (4910 / 5910) when left empty, so existing
// callers need no changes.
type RevaluationConfig struct {
	// GainAccount / LossAccount receive the unrealized adjustment.
	// Empty falls back to AccountCodeUnrealizedFXGain / ...Loss.
	GainAccount string
	LossAccount string
	// AccountAllowList optionally narrows the sweep to a subset of
	// account codes; empty means every open foreign-currency
	// asset/liability account.
	AccountAllowList []string
}

func (c RevaluationConfig) withDefaults() RevaluationConfig {
	if c.GainAccount == "" {
		c.GainAccount = AccountCodeUnrealizedFXGain
	}
	if c.LossAccount == "" {
		c.LossAccount = AccountCodeUnrealizedFXLoss
	}
	return c
}

// RevaluationLine is the per-(account, currency) outcome of a
// revaluation run: the foreign balance, the rate applied, the base
// value before and after, the posted delta, and the journal entry
// the delta was booked through.
type RevaluationLine struct {
	AccountCode     string          `json:"account_code"`
	Currency        string          `json:"currency"`
	BaseCurrency    string          `json:"base_currency"`
	ForeignNet      decimal.Decimal `json:"foreign_net"`
	CurrentRate     decimal.Decimal `json:"current_rate"`
	RecordedBase    decimal.Decimal `json:"recorded_base"`
	RevaluedBase    decimal.Decimal `json:"revalued_base"`
	Delta           decimal.Decimal `json:"delta"`
	GainLossAccount string          `json:"gain_loss_account"`
	EntryID         uuid.UUID       `json:"entry_id"`
}

// RevaluationSkip records an open foreign-currency balance the run
// could not revalue because no rate was available for its currency as
// of the run date. Surfacing these explicitly (rather than letting
// the balance silently fall out of Lines) lets an operator see which
// positions still need a rate before the period can close.
type RevaluationSkip struct {
	AccountCode  string          `json:"account_code"`
	Currency     string          `json:"currency"`
	BaseCurrency string          `json:"base_currency"`
	ForeignNet   decimal.Decimal `json:"foreign_net"`
	Reason       string          `json:"reason"`
}

// RevaluationResult is the envelope returned by a revaluation run and
// persisted to fx_revaluation_runs. TotalGain/TotalLoss are
// magnitudes (both non-negative); Net = TotalGain − TotalLoss.
// Skipped lists balances left unrevalued for want of a rate.
type RevaluationResult struct {
	TenantID  uuid.UUID         `json:"tenant_id"`
	AsOf      time.Time         `json:"as_of"`
	Lines     []RevaluationLine `json:"lines"`
	Skipped   []RevaluationSkip `json:"skipped"`
	TotalGain decimal.Decimal   `json:"total_gain"`
	TotalLoss decimal.Decimal   `json:"total_loss"`
	Net       decimal.Decimal   `json:"net"`
}

// fxBalance is one open foreign-currency balance gathered by the
// sweep: the line currency net, and the base-currency value the
// ledger currently records for those lines.
type fxBalance struct {
	currency        string
	base            string
	account         string
	foreignNet      decimal.Decimal
	recordedBaseNet decimal.Decimal
}

// gatherFXBalances reads every open foreign-currency asset/liability
// balance for the tenant under its RLS context. The aggregation runs
// in SQL so a tenant with thousands of open invoices does not ship
// every row over the wire. When allowList is non-empty the sweep is
// restricted to those account codes.
func gatherFXBalances(ctx context.Context, ledger *PGStore, tenantID uuid.UUID, allowList []string) ([]fxBalance, error) {
	var balances []fxBalance
	err := dbutil.WithTenantTx(ctx, ledger.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var baseCurrency string
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(base_currency, 'USD') FROM tenants WHERE id = $1`,
			tenantID,
		).Scan(&baseCurrency); err != nil {
			return fmt.Errorf("read base currency: %w", err)
		}
		rows, err := tx.Query(ctx,
			`SELECT jl.account_code, jl.currency,
			        SUM(jl.debit - jl.credit) AS foreign_net,
			        SUM(COALESCE(jl.base_amount, jl.debit - jl.credit)) AS base_net
			   FROM journal_lines jl
			   JOIN accounts a ON a.tenant_id = jl.tenant_id AND a.code = jl.account_code
			  WHERE jl.tenant_id = $1
			    AND jl.currency <> $2
			    AND a.type IN ('asset', 'liability')
			    AND ($3::text[] IS NULL OR jl.account_code = ANY($3))
			  GROUP BY jl.account_code, jl.currency
			  HAVING SUM(jl.debit - jl.credit) <> 0`,
			tenantID, baseCurrency, allowListParam(allowList),
		)
		if err != nil {
			return fmt.Errorf("scan open fx balances: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			b := fxBalance{base: baseCurrency}
			if err := rows.Scan(&b.account, &b.currency, &b.foreignNet, &b.recordedBaseNet); err != nil {
				return fmt.Errorf("scan fx row: %w", err)
			}
			balances = append(balances, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return balances, nil
}

// allowListParam normalises an allow-list to a value the pgx driver
// encodes as a nullable text[]: nil for "all accounts".
func allowListParam(allowList []string) any {
	if len(allowList) == 0 {
		return nil
	}
	return allowList
}

// runFXRevaluation is the shared sweep+post core. It gathers open
// foreign-currency balances, computes the delta between the
// re-translated base value and the recorded base value for each
// (account, currency) pair, and posts a single balanced revaluation
// entry per pair to the configured gain/loss account. A missing rate
// for one currency skips that pair rather than aborting the sweep.
func runFXRevaluation(ctx context.Context, ledger *PGStore, rates *ExchangeRateStore, systemActor, tenantID uuid.UUID, asOf time.Time, cfg RevaluationConfig) (*RevaluationResult, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("fx revaluation: tenant id required")
	}
	cfg = cfg.withDefaults()
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	balances, err := gatherFXBalances(ctx, ledger, tenantID, cfg.AccountAllowList)
	if err != nil {
		return nil, err
	}
	res := &RevaluationResult{TenantID: tenantID, AsOf: asOf, Lines: []RevaluationLine{}, Skipped: []RevaluationSkip{}}
	for _, b := range balances {
		currentRate, err := rates.GetRate(ctx, tenantID, b.currency, b.base, asOf)
		if err != nil {
			// No rate for this currency as of the run date: record the
			// untouched balance so the gap is visible to the operator
			// rather than aborting the whole sweep.
			res.Skipped = append(res.Skipped, RevaluationSkip{
				AccountCode:  b.account,
				Currency:     b.currency,
				BaseCurrency: b.base,
				ForeignNet:   b.foreignNet,
				Reason:       fmt.Sprintf("no %s→%s rate as of %s", b.currency, b.base, asOf.Format("2006-01-02")),
			})
			continue
		}
		currentBase := b.foreignNet.Mul(currentRate)
		delta := currentBase.Sub(b.recordedBaseNet)
		if delta.IsZero() {
			continue
		}
		gainLossAccount := cfg.GainAccount
		if delta.IsNegative() {
			gainLossAccount = cfg.LossAccount
		}
		entry := JournalEntry{
			TenantID:    tenantID,
			PostedAt:    asOf,
			Memo:        fmt.Sprintf("FX-REVAL %s %s→%s on %s", b.account, b.currency, b.base, asOf.Format("2006-01-02")),
			SourceKType: "finance.fx_revaluation",
			CreatedBy:   systemActor,
		}
		abs := delta.Abs()
		if delta.IsPositive() {
			entry.Lines = []JournalLine{
				{TenantID: tenantID, AccountCode: b.account, Debit: abs, Currency: b.base},
				{TenantID: tenantID, AccountCode: gainLossAccount, Credit: abs, Currency: b.base},
			}
		} else {
			entry.Lines = []JournalLine{
				{TenantID: tenantID, AccountCode: gainLossAccount, Debit: abs, Currency: b.base},
				{TenantID: tenantID, AccountCode: b.account, Credit: abs, Currency: b.base},
			}
		}
		posted, err := ledger.PostJournalEntry(ctx, entry)
		if err != nil {
			return nil, fmt.Errorf("post fx revaluation %s/%s: %w", b.account, b.currency, err)
		}
		res.Lines = append(res.Lines, RevaluationLine{
			AccountCode:     b.account,
			Currency:        b.currency,
			BaseCurrency:    b.base,
			ForeignNet:      b.foreignNet,
			CurrentRate:     currentRate,
			RecordedBase:    b.recordedBaseNet,
			RevaluedBase:    currentBase,
			Delta:           delta,
			GainLossAccount: gainLossAccount,
			EntryID:         posted.ID,
		})
		if delta.IsPositive() {
			res.TotalGain = res.TotalGain.Add(abs)
		} else {
			res.TotalLoss = res.TotalLoss.Add(abs)
		}
	}
	res.Net = res.TotalGain.Sub(res.TotalLoss)
	return res, nil
}

// RevaluationRunner is the on-demand (API-triggered) FX revaluation
// entry point. It posts the same revaluation entries as the
// scheduled job but returns a structured result and persists an
// audit row to fx_revaluation_runs.
type RevaluationRunner struct {
	ledger      *PGStore
	rates       *ExchangeRateStore
	systemActor uuid.UUID
	cfg         RevaluationConfig
}

// NewRevaluationRunner wires the runner. ledger and rates are
// required; systemActor stamps CreatedBy on posted entries.
func NewRevaluationRunner(ledger *PGStore, rates *ExchangeRateStore, systemActor uuid.UUID, cfg RevaluationConfig) *RevaluationRunner {
	if ledger == nil || rates == nil {
		panic("ledger: RevaluationRunner requires non-nil ledger + rates")
	}
	if systemActor == uuid.Nil {
		panic("ledger: RevaluationRunner requires non-nil systemActor")
	}
	return &RevaluationRunner{ledger: ledger, rates: rates, systemActor: systemActor, cfg: cfg.withDefaults()}
}

// Run revalues the tenant's open foreign-currency balances as of
// asOf, posts the adjustments, persists the run, and returns the
// result. The optional override merges onto the runner's base config
// so a caller can revalue a single account or use bespoke gain/loss
// accounts for one run without rebuilding the runner.
func (r *RevaluationRunner) Run(ctx context.Context, tenantID uuid.UUID, asOf time.Time, override RevaluationConfig) (*RevaluationResult, error) {
	cfg := r.cfg
	if override.GainAccount != "" {
		cfg.GainAccount = override.GainAccount
	}
	if override.LossAccount != "" {
		cfg.LossAccount = override.LossAccount
	}
	if len(override.AccountAllowList) > 0 {
		cfg.AccountAllowList = override.AccountAllowList
	}
	res, err := runFXRevaluation(ctx, r.ledger, r.rates, r.systemActor, tenantID, asOf, cfg)
	if err != nil {
		return nil, err
	}
	if err := r.persist(ctx, tenantID, res); err != nil {
		return nil, err
	}
	return res, nil
}

// persist writes the run envelope to fx_revaluation_runs under the
// tenant's RLS context so the row is owned by the tenant and visible
// only within its scope.
func (r *RevaluationRunner) persist(ctx context.Context, tenantID uuid.UUID, res *RevaluationResult) error {
	payload, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("fx revaluation: marshal result: %w", err)
	}
	return dbutil.WithTenantTx(ctx, r.ledger.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO fx_revaluation_runs
			     (tenant_id, id, as_of, total_gain, total_loss, net, result, created_by, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
			tenantID, uuid.New(), res.AsOf, res.TotalGain, res.TotalLoss, res.Net, payload, r.systemActor,
		)
		if err != nil {
			return fmt.Errorf("fx revaluation: persist run: %w", err)
		}
		return nil
	})
}
