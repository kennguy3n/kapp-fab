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
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeUnrealizedGainLoss is the scheduled-action key the wizard
// seeds on tenants whose plan includes finance. The worker registers
// UnrealizedGainLossJob against this key.
const ActionTypeUnrealizedGainLoss = "unrealized_gain_loss"

// AccountCodeUnrealizedFXGain / AccountCodeUnrealizedFXLoss name the
// adjustment accounts the revaluation entry posts to. The wizard's
// COA template seeds these for finance-enabled plans (mirroring
// ERPNext's "Exchange Gain/Loss" account convention).
const (
	AccountCodeUnrealizedFXGain = "4910"
	AccountCodeUnrealizedFXLoss = "5910"
)

// UnrealizedGainLossJob walks every open AR/AP invoice with a
// foreign-currency balance, fetches the current rate, and posts a
// single revaluation journal entry per pair (currency, account).
//
// Mirrors ERPNext's "Exchange Rate Revaluation" doctype: it does NOT
// reverse on the next run; instead each run posts a delta against the
// previously-revalued figure. Because we store base_amount on every
// journal line (migration 000029), the running base value of an
// open invoice is always recoverable from the line history without
// re-running rate lookups, so the delta computation is exact.
type UnrealizedGainLossJob struct {
	ledger      *PGStore
	rates       *ExchangeRateStore
	systemActor uuid.UUID
}

// NewUnrealizedGainLossJob wires the job from its collaborators.
// ledger and rates are required; passing nil panics at registration
// time so misconfiguration surfaces during boot rather than at the
// first scheduled run.
//
// systemActor stamps CreatedBy on revaluation entries — the worker
// passes its `workerSystemActor` constant so audit logs attribute
// the revaluation to a deterministic synthetic actor (matches the
// recurring-invoice handler pattern in services/worker/main.go).
func NewUnrealizedGainLossJob(ledger *PGStore, rates *ExchangeRateStore, systemActor uuid.UUID) *UnrealizedGainLossJob {
	if ledger == nil || rates == nil {
		panic("ledger: UnrealizedGainLossJob requires non-nil ledger + rates")
	}
	if systemActor == uuid.Nil {
		panic("ledger: UnrealizedGainLossJob requires non-nil systemActor")
	}
	return &UnrealizedGainLossJob{ledger: ledger, rates: rates, systemActor: systemActor}
}

// Handle implements scheduler.ActionHandler. The action.Payload is
// reserved for future configuration (cadence is already encoded on
// the scheduled_actions row); right now the job runs unconditionally
// for the supplied tenant.
func (j *UnrealizedGainLossJob) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if tenantID == uuid.Nil {
		return errors.New("unrealized gain/loss: tenant id required")
	}
	asOf := time.Now().UTC()

	// First pass: read all open foreign-currency balances under the
	// tenant's RLS context. The aggregation is done in SQL so a
	// tenant with thousands of open invoices does not need to ship
	// every row over the wire.
	type fxBalance struct {
		currency string
		base     string
		account  string
		// foreignNet is the unposted balance in the line currency
		// (debits minus credits). Positive means the tenant owes
		// (AP) or is owed (AR) — the direction is preserved in
		// the post below by re-using net's sign on the dr/cr legs.
		foreignNet decimal.Decimal
		// recordedBaseNet is what the ledger currently holds for
		// the same set of lines, in base currency. Subtracting
		// (foreignNet × currentRate) − recordedBaseNet yields the
		// revaluation delta we need to post.
		recordedBaseNet decimal.Decimal
	}
	var balances []fxBalance
	err := dbutil.WithTenantTx(ctx, j.ledger.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
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
			  GROUP BY jl.account_code, jl.currency
			  HAVING SUM(jl.debit - jl.credit) <> 0`,
			tenantID, baseCurrency,
		)
		if err != nil {
			return fmt.Errorf("scan open fx balances: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b fxBalance
			b.base = baseCurrency
			if err := rows.Scan(&b.account, &b.currency, &b.foreignNet, &b.recordedBaseNet); err != nil {
				return fmt.Errorf("scan fx row: %w", err)
			}
			balances = append(balances, b)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if len(balances) == 0 {
		return nil
	}

	// Second pass: compute deltas and post a single revaluation
	// entry per (account, currency) pair. Each entry has two legs:
	// the open account itself for the delta in base currency, and
	// the matching gain/loss adjustment account.
	for _, b := range balances {
		currentRate, err := j.rates.GetRate(ctx, tenantID, b.currency, b.base, asOf)
		if err != nil {
			// Skip this pair rather than abort the whole sweep —
			// a missing rate for one currency should not stop
			// revaluation of the others.
			continue
		}
		currentBase := b.foreignNet.Mul(currentRate)
		delta := currentBase.Sub(b.recordedBaseNet)
		if delta.IsZero() {
			continue
		}
		entry := JournalEntry{
			TenantID:    tenantID,
			PostedAt:    asOf,
			Memo:        fmt.Sprintf("FX-REVAL %s %s→%s on %s", b.account, b.currency, b.base, asOf.Format("2006-01-02")),
			SourceKType: "finance.fx_revaluation",
			CreatedBy:   j.systemActor,
		}
		gainLossAccount := AccountCodeUnrealizedFXGain
		if delta.IsNegative() {
			gainLossAccount = AccountCodeUnrealizedFXLoss
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
		if _, err := j.ledger.PostJournalEntry(ctx, entry); err != nil {
			return fmt.Errorf("post fx revaluation %s/%s: %w", b.account, b.currency, err)
		}
	}
	return nil
}

// fxRevaluationPayload is the (currently empty) JSON shape stored on
// the scheduled action row. Reserved so future iterations can ship
// per-tenant overrides (e.g. account allow-list) without a schema
// migration.
type fxRevaluationPayload struct {
	// AccountAllowList optionally narrows the revaluation sweep to
	// a subset of accounts; empty means "every open AR/AP account".
	AccountAllowList []string `json:"account_allow_list,omitempty"`
}

// MarshalDefaultPayload returns the default payload the wizard seeds
// on the unrealized_gain_loss scheduled action — currently empty so
// the sweep covers every account.
func MarshalDefaultPayload() ([]byte, error) {
	return json.Marshal(fxRevaluationPayload{})
}
