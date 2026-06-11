package ledger

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
)

// Phase M Task 7 / Workstream 3 — multi-entity consolidation for SMEs.
//
// A ConsolidationGroup is operator-scoped (no tenant_id, no RLS) so
// a parent company can consolidate trial balances across several
// subsidiaries (2–10 entities) into a single presentation currency.
// The store reads the member tenants' journal_lines through the
// admin pool (role kapp_admin, BYPASSRLS) so one Run call can
// collect every member's trial balance without juggling per-tenant
// connection contexts.
//
// Currency translation follows the IAS 21 / ASC 830 current-rate
// (closing-rate) method: balance-sheet accounts (asset, liability,
// equity) translate at the period-end closing rate while income-
// statement accounts (revenue, expense) translate at the period
// average rate. The resulting translation difference is parked in a
// Cumulative Translation Adjustment (CTA) equity line so the
// consolidated balance sheet still balances — see consolidate() in
// consolidation_translate.go.
//
// Intercompany elimination removes ONLY the matched intercompany
// contributions between a pair's two tenants (not the entire
// account-code balance), so third-party balances booked to the same
// account code survive the consolidation.

// AccountCodeCTA is the default Cumulative Translation Adjustment
// equity account used to absorb currency-translation differences
// when a group does not configure its own cta_account_code. The CTA
// row is synthetic to the consolidated report — it is never posted
// to any member tenant's ledger — so the code only needs to be a
// stable label, not a row in any chart of accounts.
const AccountCodeCTA = "3900"

// ConsolidationGroup is a stored aggregate of member tenants plus
// the elimination map. Persisted in the consolidation_groups table.
type ConsolidationGroup struct {
	ID                   uuid.UUID         `json:"id"`
	Name                 string            `json:"name"`
	PresentationCurrency string            `json:"presentation_currency"`
	MemberTenantIDs      []uuid.UUID       `json:"member_tenant_ids"`
	EliminationPairs     []EliminationPair `json:"elimination_pairs"`
	// CTAAccountCode names the equity account the consolidation
	// parks currency-translation differences in. Empty falls back
	// to AccountCodeCTA.
	CTAAccountCode string    `json:"cta_account_code,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// EliminationPair describes an inter-company balance that nets to
// zero in the consolidated trial balance. The from/to tenants carry
// mirror balances (typically AR on one side / AP on the other, or
// intercompany revenue / expense); a single pair captures both
// sides.
//
// FromAccount / ToAccount name the intercompany account on each
// side. When both are empty the legacy AccountCode is used for both
// sides (the two tenants book to the same shared code). Only the
// named tenants' contributions to those accounts are eliminated, so
// third-party balances on the same account code are left intact.
type EliminationPair struct {
	FromTenant  uuid.UUID `json:"from_tenant"`
	ToTenant    uuid.UUID `json:"to_tenant"`
	AccountCode string    `json:"account_code"`
	FromAccount string    `json:"from_account,omitempty"`
	ToAccount   string    `json:"to_account,omitempty"`
}

// from returns the intercompany account on the from-tenant side,
// falling back to the shared AccountCode.
func (p EliminationPair) from() string {
	if p.FromAccount != "" {
		return p.FromAccount
	}
	return p.AccountCode
}

// to returns the intercompany account on the to-tenant side,
// falling back to the shared AccountCode.
func (p EliminationPair) to() string {
	if p.ToAccount != "" {
		return p.ToAccount
	}
	return p.AccountCode
}

// ConsolidatedRow is one row of the combined trial balance. The
// per-tenant Contributions slice retains the source amounts so the
// UI can drill down.
type ConsolidatedRow struct {
	AccountCode   string             `json:"account_code"`
	AccountName   string             `json:"account_name,omitempty"`
	Type          string             `json:"type,omitempty"`
	Debit         decimal.Decimal    `json:"debit"`
	Credit        decimal.Decimal    `json:"credit"`
	Balance       decimal.Decimal    `json:"balance"`
	Contributions []TenantBalanceRow `json:"contributions"`
}

// TenantBalanceRow carries the per-tenant slice of a consolidated
// row. The amounts here are POST-currency-conversion so the UI can
// render them additively against the group total.
type TenantBalanceRow struct {
	TenantID uuid.UUID       `json:"tenant_id"`
	Debit    decimal.Decimal `json:"debit"`
	Credit   decimal.Decimal `json:"credit"`
}

// ConsolidatedTrialBalance is the report-level aggregate. For a
// well-formed consolidation TotalDebit == TotalCredit (Residual == 0):
// the CTA equity row absorbs translation and elimination differences
// so the combined trial balance always balances.
type ConsolidatedTrialBalance struct {
	GroupID              uuid.UUID         `json:"group_id"`
	AsOf                 time.Time         `json:"as_of"`
	PresentationCurrency string            `json:"presentation_currency"`
	Rows                 []ConsolidatedRow `json:"rows"`
	Eliminated           []ConsolidatedRow `json:"eliminated"`
	TotalDebit           decimal.Decimal   `json:"total_debit"`
	TotalCredit          decimal.Decimal   `json:"total_credit"`
	// Residual = TotalDebit − TotalCredit. Should be exactly zero;
	// surfaced so integration tests can assert balance cheaply.
	Residual decimal.Decimal `json:"residual"`
	// CTA is the net cumulative translation adjustment posted to the
	// equity CTA account (credit-positive). Zero when every entity
	// already reports in the presentation currency.
	CTA decimal.Decimal `json:"cta"`
}

// ConsolidationStore persists groups and runs. Reads use the admin
// pool because a single call has to span multiple tenants — RLS
// would otherwise short-circuit each per-tenant trial balance
// fetch the moment we left the first tenant's context.
type ConsolidationStore struct {
	adminPool *pgxpool.Pool
	ledger    *PGStore
	rates     *ExchangeRateStore
	now       func() time.Time
}

// NewConsolidationStore wires the dependencies. adminPool MUST be
// the BYPASSRLS pool — a regular pool will silently return zero
// rows for any tenant not pinned via SET LOCAL app.tenant_id and
// the consolidated balance will look mysteriously short.
func NewConsolidationStore(adminPool *pgxpool.Pool, ledger *PGStore, rates *ExchangeRateStore) *ConsolidationStore {
	return &ConsolidationStore{adminPool: adminPool, ledger: ledger, rates: rates, now: time.Now}
}

// CreateGroup inserts a new consolidation group. The caller MUST
// have admin privileges (enforced by the HTTP middleware on
// /api/v1/admin/consolidation/*).
func (s *ConsolidationStore) CreateGroup(ctx context.Context, g ConsolidationGroup) (*ConsolidationGroup, error) {
	if s.adminPool == nil {
		return nil, errors.New("consolidation: admin pool required")
	}
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	if g.Name == "" {
		return nil, errors.New("consolidation: group name required")
	}
	if g.PresentationCurrency == "" {
		return nil, errors.New("consolidation: presentation_currency required")
	}
	if len(g.MemberTenantIDs) == 0 {
		return nil, errors.New("consolidation: at least one member tenant required")
	}
	pairs, _ := json.Marshal(g.EliminationPairs)
	now := s.now().UTC()
	_, err := s.adminPool.Exec(ctx, `
		INSERT INTO consolidation_groups (id, name, presentation_currency, members, elimination_pairs, cta_account_code, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
	`, g.ID, g.Name, g.PresentationCurrency, g.MemberTenantIDs, pairs, nullIfEmpty(g.CTAAccountCode), now)
	if err != nil {
		return nil, fmt.Errorf("consolidation: insert: %w", err)
	}
	g.CreatedAt = now
	g.UpdatedAt = now
	return &g, nil
}

// GetGroup loads a single group by id.
func (s *ConsolidationStore) GetGroup(ctx context.Context, id uuid.UUID) (*ConsolidationGroup, error) {
	if s.adminPool == nil {
		return nil, errors.New("consolidation: admin pool required")
	}
	var (
		g     ConsolidationGroup
		pairs []byte
	)
	var cta *string
	err := s.adminPool.QueryRow(ctx, `
		SELECT id, name, presentation_currency, members, elimination_pairs, cta_account_code, created_at, updated_at
		FROM consolidation_groups
		WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&g.ID, &g.Name, &g.PresentationCurrency, &g.MemberTenantIDs, &pairs, &cta, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("consolidation: group %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("consolidation: load group: %w", err)
	}
	if cta != nil {
		g.CTAAccountCode = *cta
	}
	if len(pairs) > 0 {
		if err := json.Unmarshal(pairs, &g.EliminationPairs); err != nil {
			return nil, fmt.Errorf("consolidation: decode elimination pairs: %w", err)
		}
	}
	return &g, nil
}

// ConsolidationOptions tunes a consolidation run beyond the group's
// stored configuration.
type ConsolidationOptions struct {
	// AsOf is the period-end the run is taken at (closing rates are
	// resolved here). Zero means as-of now (UTC).
	AsOf time.Time
	// AverageRates optionally supplies the period average rate for
	// translating income-statement accounts, keyed by the entity's
	// base currency code (e.g. "EUR"). The value is the rate from
	// that currency INTO the presentation currency. When a currency
	// is absent the run falls back to the closing rate for that
	// currency, which collapses the CTA contribution to zero — the
	// correct behaviour when no separate average rate is known.
	AverageRates map[string]decimal.Decimal
	// Persist controls whether the run is written to
	// consolidation_runs. RunConsolidation persists; the statements
	// endpoint may re-run without persisting.
	Persist bool
}

// RunConsolidation produces a combined, balanced trial balance for
// the group as of `asOf` and persists the run. It is the backward-
// compatible entry point used by the admin HTTP handler; richer
// control (average rates for P&L translation) is available via
// RunConsolidationWithOptions.
func (s *ConsolidationStore) RunConsolidation(ctx context.Context, groupID uuid.UUID, asOf time.Time, actor uuid.UUID) (*ConsolidatedTrialBalance, error) {
	return s.RunConsolidationWithOptions(ctx, groupID, actor, ConsolidationOptions{AsOf: asOf, Persist: true})
}

// RunConsolidationWithOptions is the full consolidation pipeline:
//
//  1. For each member tenant, fetch its per-account TrialBalance via
//     ledger.PGStore.TrialBalance (pinned to that tenant's RLS scope)
//     and resolve its base currency.
//  2. Resolve the closing rate (base→presentation, as-of period end)
//     and the average rate (from opts, falling back to closing) for
//     each entity.
//  3. consolidate() translates each entity (closing rate for
//     balance-sheet accounts, average for income-statement
//     accounts), books a per-entity CTA equity adjustment so each
//     translated entity stays balanced, sums across entities,
//     eliminates only the matched intercompany contributions, and
//     folds any elimination FX difference into the CTA so the
//     combined trial balance balances exactly.
//  4. Optionally persist the run to consolidation_runs.
func (s *ConsolidationStore) RunConsolidationWithOptions(ctx context.Context, groupID, actor uuid.UUID, opts ConsolidationOptions) (*ConsolidatedTrialBalance, error) {
	if s.adminPool == nil || s.ledger == nil || s.rates == nil {
		return nil, errors.New("consolidation: store not fully wired")
	}
	g, err := s.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	asOf := opts.AsOf
	if asOf.IsZero() {
		asOf = s.now().UTC()
	}

	entities := make([]entityTrialBalance, 0, len(g.MemberTenantIDs))
	for _, tn := range g.MemberTenantIDs {
		tb, err := s.ledger.TrialBalance(ctx, tn, asOf)
		if err != nil {
			return nil, fmt.Errorf("consolidation: trial balance for %s: %w", tn, err)
		}
		base, err := s.tenantBaseCurrency(ctx, tn)
		if err != nil {
			return nil, err
		}
		closing, err := s.rateOrIdentity(ctx, tn, base, g.PresentationCurrency, asOf)
		if err != nil {
			return nil, err
		}
		average := closing
		if r, ok := opts.AverageRates[base]; ok && r.IsPositive() {
			average = r
		}
		entities = append(entities, entityTrialBalance{
			tenantID:     tn,
			baseCurrency: base,
			closingRate:  closing,
			averageRate:  average,
			rows:         tb.Rows,
		})
	}

	out := consolidate(entities, g.EliminationPairs, ctaAccount(g.CTAAccountCode))
	out.GroupID = groupID
	out.AsOf = asOf
	out.PresentationCurrency = g.PresentationCurrency

	if opts.Persist {
		resultJSON, _ := json.Marshal(out)
		runID := uuid.New()
		if _, err = s.adminPool.Exec(ctx, `
			INSERT INTO consolidation_runs (id, group_id, as_of, result, created_at, created_by)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, runID, groupID, asOf, resultJSON, s.now().UTC(), actor); err != nil {
			return nil, fmt.Errorf("consolidation: persist run: %w", err)
		}
	}
	return out, nil
}

// rateOrIdentity resolves the base→presentation rate for an entity,
// returning 1 when the currencies match (avoiding a needless lookup
// and the same-currency error from GetRate's validation path). The
// lookup runs in the member tenant's RLS scope so the admin pool's
// BYPASSRLS does not pull a rate from a different tenant.
func (s *ConsolidationStore) rateOrIdentity(ctx context.Context, tenantID uuid.UUID, from, to string, asOf time.Time) (decimal.Decimal, error) {
	if from == "" || to == "" || from == to {
		return decimal.NewFromInt(1), nil
	}
	rate, err := s.rates.GetRate(ctx, tenantID, from, to, asOf)
	if err != nil {
		return decimal.Zero, fmt.Errorf("consolidation: closing rate %s→%s for %s: %w", from, to, tenantID, err)
	}
	return rate, nil
}

// tenantBaseCurrency reads tenants.base_currency for the given id
// via the admin pool. Falls back to "USD" if the column is null
// (mirrors the same default the ledger uses on JE posting).
func (s *ConsolidationStore) tenantBaseCurrency(ctx context.Context, tenantID uuid.UUID) (string, error) {
	var cur string
	err := s.adminPool.QueryRow(ctx,
		`SELECT COALESCE(base_currency, 'USD') FROM tenants WHERE id = $1`,
		tenantID,
	).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return "USD", nil
	}
	if err != nil {
		return "", fmt.Errorf("consolidation: load tenant currency: %w", err)
	}
	return cur, nil
}
