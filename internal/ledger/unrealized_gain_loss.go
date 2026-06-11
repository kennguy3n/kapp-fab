package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

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

// Handle implements scheduler.ActionHandler. It decodes the optional
// account allow-list from the action payload and delegates to the
// shared runFXRevaluation core (also used by the on-demand
// RevaluationRunner), so the scheduled sweep and the API run share a
// single, tested implementation. The scheduled path does not persist
// a run row — the journal entries it posts are the durable record.
func (j *UnrealizedGainLossJob) Handle(ctx context.Context, tenantID uuid.UUID, action scheduler.ScheduledAction) error {
	if tenantID == uuid.Nil {
		return errors.New("unrealized gain/loss: tenant id required")
	}
	cfg := RevaluationConfig{}
	if len(action.Payload) > 0 {
		var p fxRevaluationPayload
		if err := json.Unmarshal(action.Payload, &p); err != nil {
			return fmt.Errorf("unrealized gain/loss: decode payload: %w", err)
		}
		cfg.AccountAllowList = p.AccountAllowList
	}
	_, err := runFXRevaluation(ctx, j.ledger, j.rates, j.systemActor, tenantID, time.Now().UTC(), cfg)
	return err
}

// fxRevaluationPayload is the JSON shape stored on the scheduled
// action row, allowing per-tenant overrides without a schema
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
