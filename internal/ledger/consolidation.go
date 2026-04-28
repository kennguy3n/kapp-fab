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

// Phase M Task 7 — multi-tenant consolidation.
//
// A ConsolidationGroup is operator-scoped (no tenant_id, no RLS) so
// a parent company can consolidate trial balances across several
// subsidiaries into a single presentation currency. The store
// reads the member tenants' journal_lines through the admin pool
// (role kapp_admin, BYPASSRLS) so one Run call can collect every
// member's trial balance without juggling per-tenant connection
// contexts.
//
// Foreign-currency lines are converted via the existing
// ExchangeRateStore against the group's presentation_currency.
// Elimination pairs net inter-company AR/AP balances so they don't
// inflate the combined balance sheet.

// ConsolidationGroup is a stored aggregate of member tenants plus
// the elimination map. Persisted in the consolidation_groups table.
type ConsolidationGroup struct {
	ID                   uuid.UUID         `json:"id"`
	Name                 string            `json:"name"`
	PresentationCurrency string            `json:"presentation_currency"`
	MemberTenantIDs      []uuid.UUID       `json:"member_tenant_ids"`
	EliminationPairs     []EliminationPair `json:"elimination_pairs"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// EliminationPair describes an inter-company balance that nets to
// zero in the consolidated trial balance. The from/to tenants
// usually carry mirror AR / AP balances; a single pair captures
// both sides.
type EliminationPair struct {
	FromTenant  uuid.UUID `json:"from_tenant"`
	ToTenant    uuid.UUID `json:"to_tenant"`
	AccountCode string    `json:"account_code"`
}

// ConsolidatedRow is one row of the combined trial balance. The
// per-tenant Contributions slice retains the source amounts so the
// UI can drill down.
type ConsolidatedRow struct {
	AccountCode   string             `json:"account_code"`
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

// ConsolidatedTrialBalance is the report-level aggregate.
type ConsolidatedTrialBalance struct {
	GroupID              uuid.UUID         `json:"group_id"`
	AsOf                 time.Time         `json:"as_of"`
	PresentationCurrency string            `json:"presentation_currency"`
	Rows                 []ConsolidatedRow `json:"rows"`
	Eliminated           []ConsolidatedRow `json:"eliminated"`
	TotalDebit           decimal.Decimal   `json:"total_debit"`
	TotalCredit          decimal.Decimal   `json:"total_credit"`
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
		INSERT INTO consolidation_groups (id, name, presentation_currency, members, elimination_pairs, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, g.ID, g.Name, g.PresentationCurrency, g.MemberTenantIDs, pairs, now)
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
	err := s.adminPool.QueryRow(ctx, `
		SELECT id, name, presentation_currency, members, elimination_pairs, created_at, updated_at
		FROM consolidation_groups
		WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&g.ID, &g.Name, &g.PresentationCurrency, &g.MemberTenantIDs, &pairs, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("consolidation: group %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("consolidation: load group: %w", err)
	}
	if len(pairs) > 0 {
		if err := json.Unmarshal(pairs, &g.EliminationPairs); err != nil {
			return nil, fmt.Errorf("consolidation: decode elimination pairs: %w", err)
		}
	}
	return &g, nil
}

// RunConsolidation produces a combined trial balance for the given
// group as of `asOf`. Steps:
//
//  1. For each member tenant, fetch its per-account TrialBalance
//     using the existing ledger.PGStore.TrialBalance call. The
//     call runs with SET LOCAL app.tenant_id pinned to that
//     tenant; the admin pool drives every per-tenant query.
//  2. Resolve each tenant's base currency from the `tenants` row
//     and convert every per-account amount into the group's
//     presentation currency via the ExchangeRateStore.
//  3. Sum across tenants per-account_code into a single
//     ConsolidatedRow with per-tenant Contributions.
//  4. For every elimination pair, zero the AR side on the from
//     tenant and the matching AP side on the to tenant — the
//     account_code in the pair drives the row that gets removed
//     and parked under the Eliminated slice for audit.
//  5. Persist the run to consolidation_runs and return.
func (s *ConsolidationStore) RunConsolidation(ctx context.Context, groupID uuid.UUID, asOf time.Time, actor uuid.UUID) (*ConsolidatedTrialBalance, error) {
	if s.adminPool == nil || s.ledger == nil || s.rates == nil {
		return nil, errors.New("consolidation: store not fully wired")
	}
	g, err := s.GetGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if asOf.IsZero() {
		asOf = s.now().UTC()
	}
	combined := map[string]*ConsolidatedRow{}
	tenantCurrency := map[uuid.UUID]string{}
	// 1 + 2 — gather and convert.
	for _, tn := range g.MemberTenantIDs {
		tb, err := s.ledger.TrialBalance(ctx, tn, asOf)
		if err != nil {
			return nil, fmt.Errorf("consolidation: trial balance for %s: %w", tn, err)
		}
		base, err := s.tenantBaseCurrency(ctx, tn)
		if err != nil {
			return nil, err
		}
		tenantCurrency[tn] = base
		for _, row := range tb.Rows {
			debit, err := s.convertAmount(ctx, tn, row.Debit, base, g.PresentationCurrency, asOf)
			if err != nil {
				return nil, err
			}
			credit, err := s.convertAmount(ctx, tn, row.Credit, base, g.PresentationCurrency, asOf)
			if err != nil {
				return nil, err
			}
			c, ok := combined[row.AccountCode]
			if !ok {
				c = &ConsolidatedRow{AccountCode: row.AccountCode, Debit: decimal.Zero, Credit: decimal.Zero, Balance: decimal.Zero}
				combined[row.AccountCode] = c
			}
			c.Debit = c.Debit.Add(debit)
			c.Credit = c.Credit.Add(credit)
			c.Contributions = append(c.Contributions, TenantBalanceRow{
				TenantID: tn, Debit: debit, Credit: credit,
			})
		}
	}
	// 3 — finalise balances.
	for _, c := range combined {
		c.Balance = c.Debit.Sub(c.Credit)
	}
	// 4 — apply eliminations. Each pair removes one account row
	// from the combined map; the deduction is captured under
	// `Eliminated` so the auditor can reconcile.
	eliminated := []ConsolidatedRow{}
	for _, pair := range g.EliminationPairs {
		if pair.AccountCode == "" {
			continue
		}
		c, ok := combined[pair.AccountCode]
		if !ok {
			continue
		}
		eliminated = append(eliminated, *c)
		delete(combined, pair.AccountCode)
	}

	totalD := decimal.Zero
	totalC := decimal.Zero
	out := &ConsolidatedTrialBalance{
		GroupID:              groupID,
		AsOf:                 asOf,
		PresentationCurrency: g.PresentationCurrency,
		Rows:                 make([]ConsolidatedRow, 0, len(combined)),
		Eliminated:           eliminated,
	}
	for _, c := range combined {
		totalD = totalD.Add(c.Debit)
		totalC = totalC.Add(c.Credit)
		out.Rows = append(out.Rows, *c)
	}
	out.TotalDebit = totalD
	out.TotalCredit = totalC
	// 5 — persist.
	resultJSON, _ := json.Marshal(out)
	runID := uuid.New()
	_, err = s.adminPool.Exec(ctx, `
		INSERT INTO consolidation_runs (id, group_id, as_of, result, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, runID, groupID, asOf, resultJSON, s.now().UTC(), actor)
	if err != nil {
		return nil, fmt.Errorf("consolidation: persist run: %w", err)
	}
	return out, nil
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

// convertAmount routes through ExchangeRateStore.Convert when the
// tenant's base currency differs from the group presentation
// currency. The rate row is read in the *member* tenant's RLS
// scope so the admin pool's BYPASSRLS doesn't accidentally pull a
// rate from a different tenant.
func (s *ConsolidationStore) convertAmount(ctx context.Context, tenantID uuid.UUID, amount decimal.Decimal, from, to string, asOf time.Time) (decimal.Decimal, error) {
	if amount.IsZero() {
		return decimal.Zero, nil
	}
	if from == "" || to == "" || from == to {
		return amount, nil
	}
	converted, err := s.rates.Convert(ctx, tenantID, amount, from, to, asOf)
	if err != nil {
		return decimal.Zero, fmt.Errorf("consolidation: convert %s→%s for %s: %w", from, to, tenantID, err)
	}
	return converted, nil
}
