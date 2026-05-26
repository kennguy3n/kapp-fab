package finance

// Phase N9c — Landed Cost Vouchers.
//
// A "landed cost" is the cost of bringing purchased goods to the
// receiving warehouse beyond the supplier's invoice amount —
// freight, insurance, duty, import tax, port handling. Without a
// landed-cost flow the moving-average cost of received inventory
// undercosts true acquisition value, COGS at subsequent sale is
// too low, and gross margin is overstated. Every mature ERP
// (ERPNext, Odoo, NetSuite, SAP) ships a "landed cost voucher"
// surface; this file implements the kapp-fab equivalent.
//
// Lifecycle: draft → allocated → posted.
//
//   * Create header + charges + targets (CreateVoucher,
//     UpsertCharge, UpsertTarget) builds the voucher in 'draft'
//     status. Targets snapshot (item, warehouse, qty, unit_cost)
//     from the source receipt so the voucher remains self-
//     contained even if the source AP bill is later voided.
//
//   * AllocateVoucher computes per-target allocated_amount shares
//     using the voucher's allocation_method (by_qty / by_amount /
//     by_weight) and stores them. The voucher transitions to
//     'allocated' and the operator can review per-target shares
//     before posting.
//
//   * PostVoucher walks each target, emits a reversal +
//     forward inventory_moves pair, then writes the booking JE
//     and pins voucher.je_id. Idempotent via the (voucher.status,
//     je_id) tuple — re-running PostVoucher on a posted voucher
//     returns the same JE rather than double-booking.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Status constants. Stored as TEXT on landed_cost_vouchers.status
// with a CHECK constraint that mirrors this set.
const (
	LandedCostStatusDraft     = "draft"
	LandedCostStatusAllocated = "allocated"
	LandedCostStatusPosted    = "posted"
)

// Allocation methods. Stored as TEXT on
// landed_cost_vouchers.allocation_method.
const (
	LandedCostByQty    = "by_qty"
	LandedCostByAmount = "by_amount"
	LandedCostByWeight = "by_weight"
)

// MoveSourceLandedCost is the source_ktype value the poster writes
// on the forward inventory_moves row so a downstream consumer can
// identify which voucher re-based the cost. The (source_ktype,
// source_id) tuple is unique-indexed on inventory_moves, so a
// posting retry within the same voucher is a no-op rather than a
// double-post.
const MoveSourceLandedCost = "finance.landed_cost_voucher"

// DefaultInventoryAccountCode is the GL account the poster debits
// when no per-charge account_code is supplied. The wizard's CoA
// template seeds this account; if a tenant disabled it,
// LedgerBackend.PostLandedCostJournalEntry returns the ledger's
// ErrAccountNotFound / ErrInactiveAccount sentinel (see
// internal/ledger/store.go:265-285) so the post fails with a
// specific account-lookup error rather than silently writing to a
// wrong account. The landed-cost layer surfaces that through the
// existing wrapped-error path; no separate finance-side sentinel
// is required.
const DefaultInventoryAccountCode = "1330"

// DefaultStockAdjustmentAccountCode is the fallback credit account
// when a charge has no account_code set. Real freight/duty bills
// typically credit the supplier AP account directly, but when the
// voucher is materialised inline (e.g. operator types "freight
// $1,200" without an AP bill behind it) the platform falls back
// to a Stock Adjustment account.
const DefaultStockAdjustmentAccountCode = "5800"

// Errors. Sentinel values so HTTP / KChat / agent surfaces can
// map to typed responses. Each sentinel below has at least one
// return site in the store; surface-level handlers (e.g.
// writeLandedCostError in services/api) map each one to a typed
// HTTP status. "Not draft" / "missing inventory account" cases
// are intentionally not separate sentinels: voucher edits demote
// the voucher back to draft (see resetVoucherToDraft) instead of
// rejecting, and missing-account validation already surfaces as
// the ledger's ErrAccountNotFound / ErrInactiveAccount.
var (
	ErrLandedCostNotFound        = errors.New("landed cost voucher not found")
	ErrLandedCostNotAllocated    = errors.New("voucher must be allocated before posting")
	ErrLandedCostAlreadyPosted   = errors.New("voucher already posted")
	ErrLandedCostNoCharges       = errors.New("voucher has no charges to allocate")
	ErrLandedCostNoTargets       = errors.New("voucher has no targets to allocate to")
	ErrLandedCostBadMethod       = errors.New("unknown allocation method")
	ErrLandedCostZeroWeightTotal = errors.New("by_weight allocation requires at least one target with weight > 0")
	// ErrLandedCostPostedJEMissing covers the operational anomaly
	// where a voucher row is in 'posted' status with a non-nil
	// je_id but the referenced journal_entries row has been hard-
	// deleted out of band (e.g. a manual SQL repair script). The
	// state is unrecoverable from inside the poster — re-running
	// PostVoucher would either double-book (if the unique source-
	// tuple index also got cleared) or return ErrLandedCostNoCharges
	// (the empty Phase 1 snapshot collapses into Phase 3's
	// no-charges branch with a misleading message). Surfacing a
	// dedicated sentinel lets the HTTP / agent / KChat layers map
	// it to a 409 with a clear operator-action message instead.
	ErrLandedCostPostedJEMissing = errors.New("posted voucher references a journal entry that no longer exists")
)

// LandedCostVoucher is the header row.
type LandedCostVoucher struct {
	TenantID         uuid.UUID  `json:"tenant_id"`
	ID               uuid.UUID  `json:"id"`
	VoucherNumber    string     `json:"voucher_number"`
	Description      string     `json:"description,omitempty"`
	Status           string     `json:"status"`
	AllocationMethod string     `json:"allocation_method"`
	PostedAt         *time.Time `json:"posted_at,omitempty"`
	JEID             *uuid.UUID `json:"je_id,omitempty"`
	CreatedBy        uuid.UUID  `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// LandedCostCharge is one cost line being absorbed into inventory.
// AccountCode, when empty, defers to DefaultStockAdjustmentAccountCode
// at posting time.
type LandedCostCharge struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	ID          uuid.UUID       `json:"id"`
	VoucherID   uuid.UUID       `json:"voucher_id"`
	Description string          `json:"description"`
	Amount      decimal.Decimal `json:"amount"`
	AccountCode string          `json:"account_code,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// LandedCostTarget is one receipt line getting its cost re-based.
// Qty / UnitCost / Amount snapshot the source inventory_moves row.
// Weight is per-target so the by_weight allocator works even on
// tenants that have not adopted item-master weight tracking.
type LandedCostTarget struct {
	TenantID        uuid.UUID       `json:"tenant_id"`
	ID              uuid.UUID       `json:"id"`
	VoucherID       uuid.UUID       `json:"voucher_id"`
	SourceKType     string          `json:"source_ktype"`
	SourceID        uuid.UUID       `json:"source_id"`
	ItemID          uuid.UUID       `json:"item_id"`
	WarehouseID     uuid.UUID       `json:"warehouse_id"`
	Qty             decimal.Decimal `json:"qty"`
	UnitCost        decimal.Decimal `json:"unit_cost"`
	Amount          decimal.Decimal `json:"amount"`
	Weight          decimal.Decimal `json:"weight"`
	AllocatedAmount decimal.Decimal `json:"allocated_amount"`
	Applied         bool            `json:"applied"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// LandedCostFilter narrows ListVouchers.
type LandedCostFilter struct {
	Status string
	Limit  int
	Offset int
}

// InventoryBackend is the minimal slice of *inventory.PGStore the
// landed-cost poster needs. Accepting an interface here breaks the
// finance → inventory → ledger → finance import cycle: the
// ledger package imports finance for KType constants, and
// inventory imports ledger for its hook signatures, so finance
// cannot import inventory directly. services/api/deps_build.go
// wires the concrete *inventory.PGStore through this interface at
// startup.
type InventoryBackend interface {
	GetMoveBySource(ctx context.Context, tenantID uuid.UUID, sourceKType string, sourceID, itemID, warehouseID uuid.UUID) (*InventoryMoveSnapshot, error)
	ReverseMove(ctx context.Context, tenantID uuid.UUID, moveID int64, actor uuid.UUID, memo string) error
	RecordLandedCostMove(ctx context.Context, in LandedCostMoveInput) error
}

// LedgerBackend is the minimal slice of *ledger.PGStore the
// landed-cost poster needs. Same rationale as InventoryBackend
// — breaks the import cycle.
type LedgerBackend interface {
	GetJournalEntryBySource(ctx context.Context, tenantID uuid.UUID, sourceKType string, sourceID uuid.UUID) (*LandedCostJournalEntry, error)
	GetJournalEntry(ctx context.Context, tenantID, id uuid.UUID) (*LandedCostJournalEntry, error)
	PostLandedCostJournalEntry(ctx context.Context, in LandedCostJEInput) (*LandedCostJournalEntry, error)
}

// InventoryMoveSnapshot is the projection of inventory.Move the
// poster reads from the source receipt. It carries only the four
// fields the poster consumes; the wire shape is kept narrow so
// future inventory.Move additions don't ripple into finance.
type InventoryMoveSnapshot struct {
	ID          int64
	Qty         decimal.Decimal
	UnitCost    decimal.Decimal
	ItemID      uuid.UUID
	WarehouseID uuid.UUID
}

// LandedCostMoveInput is the request sent to InventoryBackend.
// RecordLandedCostMove. The backend wraps it into an
// inventory.Move with source_ktype = MoveSourceLandedCost and
// source_id = TargetID — the per-target id keeps the
// inventory_moves_source_uniq tuple unique even when a single
// voucher has multiple targets on the same item+warehouse pair.
type LandedCostMoveInput struct {
	TenantID    uuid.UUID
	ItemID      uuid.UUID
	WarehouseID uuid.UUID
	Qty         decimal.Decimal
	UnitCost    decimal.Decimal
	VoucherID   uuid.UUID
	TargetID    uuid.UUID
	MovedAt     time.Time
	ActorID     uuid.UUID
}

// LandedCostJournalEntry is the projection of ledger.JournalEntry
// the poster needs back from LedgerBackend. Kept narrow so the
// finance package doesn't depend on ledger directly.
type LandedCostJournalEntry struct {
	ID       uuid.UUID `json:"id"`
	PostedAt time.Time `json:"posted_at"`
}

// LandedCostJELine is one balanced line in the landed-cost booking
// JE. Exactly one of Debit / Credit is positive on a valid line.
type LandedCostJELine struct {
	AccountCode string
	Debit       decimal.Decimal
	Credit      decimal.Decimal
	Currency    string
	Memo        string
}

// LandedCostJEInput is the request sent to
// LedgerBackend.PostLandedCostJournalEntry. The backend writes a
// real ledger.JournalEntry with source_ktype = MoveSourceLandedCost
// and source_id = VoucherID so retries return the same entry.
type LandedCostJEInput struct {
	TenantID  uuid.UUID
	VoucherID uuid.UUID
	ActorID   uuid.UUID
	PostedAt  time.Time
	Memo      string
	Lines     []LandedCostJELine
}

// LandedCostStore is the persistence + posting surface. It depends
// on the inventory + ledger backends through narrow interfaces so
// the package stays free of an import cycle.
type LandedCostStore struct {
	pool   *pgxpool.Pool
	inv    InventoryBackend
	ledger LedgerBackend
	now    func() time.Time
}

// NewLandedCostStore builds a store bound to the given pool +
// inventory + ledger backends.
func NewLandedCostStore(pool *pgxpool.Pool, inv InventoryBackend, led LedgerBackend) *LandedCostStore {
	return &LandedCostStore{
		pool:   pool,
		inv:    inv,
		ledger: led,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// WithClock injects a deterministic clock for tests. Returns the
// same store for fluent composition.
func (s *LandedCostStore) WithClock(now func() time.Time) *LandedCostStore {
	if now != nil {
		s.now = now
	}
	return s
}

// ---------------------------------------------------------------------------
// Voucher CRUD
// ---------------------------------------------------------------------------

// CreateVoucher inserts a draft voucher header.
func (s *LandedCostStore) CreateVoucher(ctx context.Context, v LandedCostVoucher) (*LandedCostVoucher, error) {
	if v.TenantID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id required")
	}
	if strings.TrimSpace(v.VoucherNumber) == "" {
		return nil, errors.New("landed_cost: voucher_number required")
	}
	if v.AllocationMethod == "" {
		v.AllocationMethod = LandedCostByQty
	}
	if !isKnownAllocationMethod(v.AllocationMethod) {
		return nil, ErrLandedCostBadMethod
	}
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	v.Status = LandedCostStatusDraft
	now := s.now()
	v.CreatedAt = now
	v.UpdatedAt = now

	err := dbutil.WithTenantTx(ctx, s.pool, v.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO landed_cost_vouchers
				(tenant_id, id, voucher_number, description, status,
				 allocation_method, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			v.TenantID, v.ID, v.VoucherNumber, v.Description, v.Status,
			v.AllocationMethod, v.CreatedBy, v.CreatedAt, v.UpdatedAt,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: create voucher: %w", err)
	}
	return &v, nil
}

// GetVoucher returns a single voucher header (without charges /
// targets — fetch those separately).
func (s *LandedCostStore) GetVoucher(ctx context.Context, tenantID, id uuid.UUID) (*LandedCostVoucher, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	var out LandedCostVoucher
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanVoucher(tx.QueryRow(ctx, voucherSelectSQL+" WHERE tenant_id = $1 AND id = $2", tenantID, id), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrLandedCostNotFound
		}
		return nil, fmt.Errorf("landed_cost: get voucher: %w", err)
	}
	return &out, nil
}

// ListVouchers returns vouchers sorted by created_at DESC.
func (s *LandedCostStore) ListVouchers(ctx context.Context, tenantID uuid.UUID, f LandedCostFilter) ([]LandedCostVoucher, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id required")
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	out := make([]LandedCostVoucher, 0, limit)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		args := []any{tenantID}
		query := voucherSelectSQL + " WHERE tenant_id = $1"
		if f.Status != "" {
			args = append(args, f.Status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		args = append(args, limit, f.Offset)
		query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v LandedCostVoucher
			if err := scanVoucher(rows, &v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: list vouchers: %w", err)
	}
	return out, nil
}

// UpdateVoucher modifies metadata fields on a draft voucher. Posted
// vouchers are immutable — to revise a posted voucher, void the JE
// and start over.
//
// Empty `allocation_method` defaults to LandedCostByQty so the
// create + update API contract is symmetric: the TS client marks
// `allocation_method?` optional on the shared
// `UpsertLandedCostVoucherInput` type, and CreateVoucher already
// defaults at line 287 above. Without this default, an update call
// that omits the field — perfectly valid per the type — would
// return ErrLandedCostBadMethod (409) instead of preserving / re-
// applying the by-qty default. Tests pin both paths.
func (s *LandedCostStore) UpdateVoucher(ctx context.Context, v LandedCostVoucher) (*LandedCostVoucher, error) {
	if v.TenantID == uuid.Nil || v.ID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	if v.AllocationMethod == "" {
		v.AllocationMethod = LandedCostByQty
	}
	if !isKnownAllocationMethod(v.AllocationMethod) {
		return nil, ErrLandedCostBadMethod
	}
	v.UpdatedAt = s.now()

	var out LandedCostVoucher
	err := dbutil.WithTenantTx(ctx, s.pool, v.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var current LandedCostVoucher
		if err := scanVoucher(tx.QueryRow(ctx,
			voucherSelectSQL+" WHERE tenant_id = $1 AND id = $2 FOR UPDATE",
			v.TenantID, v.ID), &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLandedCostNotFound
			}
			return err
		}
		if current.Status == LandedCostStatusPosted {
			return ErrLandedCostAlreadyPosted
		}
		_, err := tx.Exec(ctx,
			`UPDATE landed_cost_vouchers
			    SET voucher_number = $1, description = $2,
			        allocation_method = $3, updated_at = $4,
			        status = $5
			  WHERE tenant_id = $6 AND id = $7`,
			v.VoucherNumber, v.Description, v.AllocationMethod,
			v.UpdatedAt, LandedCostStatusDraft, v.TenantID, v.ID,
		)
		if err != nil {
			return err
		}
		return scanVoucher(tx.QueryRow(ctx,
			voucherSelectSQL+" WHERE tenant_id = $1 AND id = $2",
			v.TenantID, v.ID), &out)
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: update voucher: %w", err)
	}
	return &out, nil
}

// DeleteVoucher removes a draft voucher. Posted vouchers cannot be
// deleted — void the JE first.
func (s *LandedCostStore) DeleteVoucher(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return errors.New("landed_cost: tenant id + voucher id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// FOR UPDATE serialises against concurrent PostVoucher /
		// AllocateVoucher / UpdateVoucher; without the row lock a
		// poster could flip the voucher to "posted" (and write the
		// inventory_moves) between this status check and the
		// DELETE below, leaving orphaned inventory_moves pointing
		// at a deleted voucher_id.
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM landed_cost_vouchers WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLandedCostNotFound
			}
			return err
		}
		if status == LandedCostStatusPosted {
			return ErrLandedCostAlreadyPosted
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM landed_cost_vouchers WHERE tenant_id = $1 AND id = $2`,
			tenantID, id)
		return err
	})
}

// ---------------------------------------------------------------------------
// Charges
// ---------------------------------------------------------------------------

// UpsertCharge inserts or updates one cost line on the voucher. The
// voucher must be in draft (re-running on an allocated voucher
// resets it back to draft to force a re-allocate).
func (s *LandedCostStore) UpsertCharge(ctx context.Context, c LandedCostCharge) (*LandedCostCharge, error) {
	if c.TenantID == uuid.Nil || c.VoucherID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	if strings.TrimSpace(c.Description) == "" {
		return nil, errors.New("landed_cost: charge description required")
	}
	if !c.Amount.IsPositive() {
		return nil, errors.New("landed_cost: charge amount must be positive")
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	now := s.now()
	c.UpdatedAt = now

	err := dbutil.WithTenantTx(ctx, s.pool, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Reset voucher status so a re-allocate is required.
		if err := resetVoucherToDraft(ctx, tx, c.TenantID, c.VoucherID, now); err != nil {
			return err
		}
		// RETURNING created_at gives us the DB's authoritative
		// value: now() on the INSERT path, the preserved
		// original on the UPDATE path. Stamping c.CreatedAt =
		// now() unconditionally lied on the update path — the
		// returned API shape used to claim the row was created
		// at the current request time when in fact it had been
		// created earlier.
		return tx.QueryRow(ctx,
			`INSERT INTO landed_cost_charges
				(tenant_id, id, voucher_id, description, amount, account_code,
				 created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
			 ON CONFLICT (tenant_id, id) DO UPDATE
			   SET description = EXCLUDED.description,
			       amount = EXCLUDED.amount,
			       account_code = EXCLUDED.account_code,
			       updated_at = EXCLUDED.updated_at
			 RETURNING created_at`,
			c.TenantID, c.ID, c.VoucherID, c.Description, c.Amount,
			c.AccountCode, now,
		).Scan(&c.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: upsert charge: %w", err)
	}
	return &c, nil
}

// DeleteCharge removes a single charge line.
func (s *LandedCostStore) DeleteCharge(ctx context.Context, tenantID, voucherID, chargeID uuid.UUID) error {
	if tenantID == uuid.Nil || chargeID == uuid.Nil {
		return errors.New("landed_cost: tenant id + charge id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := resetVoucherToDraft(ctx, tx, tenantID, voucherID, s.now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM landed_cost_charges
			  WHERE tenant_id = $1 AND voucher_id = $2 AND id = $3`,
			tenantID, voucherID, chargeID)
		return err
	})
}

// ListCharges returns all charges on a voucher.
func (s *LandedCostStore) ListCharges(ctx context.Context, tenantID, voucherID uuid.UUID) ([]LandedCostCharge, error) {
	if tenantID == uuid.Nil || voucherID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	out := []LandedCostCharge{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, voucher_id, description, amount, account_code,
			        created_at, updated_at
			   FROM landed_cost_charges
			  WHERE tenant_id = $1 AND voucher_id = $2
			  ORDER BY created_at`,
			tenantID, voucherID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c LandedCostCharge
			if err := rows.Scan(
				&c.TenantID, &c.ID, &c.VoucherID, &c.Description, &c.Amount,
				&c.AccountCode, &c.CreatedAt, &c.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: list charges: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Targets
// ---------------------------------------------------------------------------

// UpsertTarget inserts or updates one receipt-line target on the
// voucher. The (qty, unit_cost, amount) snapshot is the caller's
// responsibility — typically copied from an inventory.Move record
// for the receipt being re-based.
func (s *LandedCostStore) UpsertTarget(ctx context.Context, t LandedCostTarget) (*LandedCostTarget, error) {
	if t.TenantID == uuid.Nil || t.VoucherID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	if t.ItemID == uuid.Nil || t.WarehouseID == uuid.Nil {
		return nil, errors.New("landed_cost: item + warehouse required")
	}
	if !t.Qty.IsPositive() {
		return nil, errors.New("landed_cost: target qty must be positive")
	}
	if t.SourceKType == "" {
		t.SourceKType = "finance.ap_bill"
	}
	if t.SourceID == uuid.Nil {
		return nil, errors.New("landed_cost: source receipt id required")
	}
	if t.Weight.IsNegative() {
		return nil, errors.New("landed_cost: target weight must be non-negative")
	}
	// Weight is persisted as-given. A zero value is meaningful on a
	// by_weight voucher (the target is excluded from the share split)
	// and irrelevant on by_qty / by_amount (the allocator keys off
	// Qty / Amount instead). Silently rewriting Weight=0 → 1 would
	// override operator intent on by_weight surfaces; if every target
	// ends up with Weight=0 on a by_weight voucher, Allocate fails
	// with ErrLandedCostZeroWeightTotal — a clearer signal than a
	// silent even-split via accidental default-1.
	t.Amount = t.Qty.Mul(t.UnitCost)
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	now := s.now()
	t.UpdatedAt = now
	// New rows reset Applied; updating an applied target back to
	// false would re-trigger posting work, so callers should
	// add a fresh target instead.
	t.Applied = false

	err := dbutil.WithTenantTx(ctx, s.pool, t.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := resetVoucherToDraft(ctx, tx, t.TenantID, t.VoucherID, now); err != nil {
			return err
		}
		// RETURNING created_at — same reasoning as UpsertCharge.
		return tx.QueryRow(ctx,
			`INSERT INTO landed_cost_targets
				(tenant_id, id, voucher_id, source_ktype, source_id,
				 item_id, warehouse_id, qty, unit_cost, amount, weight,
				 allocated_amount, applied, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0, FALSE, $12, $12)
			 ON CONFLICT (tenant_id, id) DO UPDATE
			   SET source_ktype = EXCLUDED.source_ktype,
			       source_id = EXCLUDED.source_id,
			       item_id = EXCLUDED.item_id,
			       warehouse_id = EXCLUDED.warehouse_id,
			       qty = EXCLUDED.qty,
			       unit_cost = EXCLUDED.unit_cost,
			       amount = EXCLUDED.amount,
			       weight = EXCLUDED.weight,
			       allocated_amount = 0,
			       applied = FALSE,
			       updated_at = EXCLUDED.updated_at
			 RETURNING created_at`,
			t.TenantID, t.ID, t.VoucherID, t.SourceKType, t.SourceID,
			t.ItemID, t.WarehouseID, t.Qty, t.UnitCost, t.Amount, t.Weight,
			now,
		).Scan(&t.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: upsert target: %w", err)
	}
	return &t, nil
}

// DeleteTarget removes a single target line.
func (s *LandedCostStore) DeleteTarget(ctx context.Context, tenantID, voucherID, targetID uuid.UUID) error {
	if tenantID == uuid.Nil || targetID == uuid.Nil {
		return errors.New("landed_cost: tenant id + target id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := resetVoucherToDraft(ctx, tx, tenantID, voucherID, s.now()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM landed_cost_targets
			  WHERE tenant_id = $1 AND voucher_id = $2 AND id = $3`,
			tenantID, voucherID, targetID)
		return err
	})
}

// ListTargets returns all targets on a voucher in stable creation
// order.
func (s *LandedCostStore) ListTargets(ctx context.Context, tenantID, voucherID uuid.UUID) ([]LandedCostTarget, error) {
	if tenantID == uuid.Nil || voucherID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	out := []LandedCostTarget{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, voucher_id, source_ktype, source_id,
			        item_id, warehouse_id, qty, unit_cost, amount, weight,
			        allocated_amount, applied, created_at, updated_at
			   FROM landed_cost_targets
			  WHERE tenant_id = $1 AND voucher_id = $2
			  ORDER BY created_at`,
			tenantID, voucherID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t LandedCostTarget
			if err := rows.Scan(
				&t.TenantID, &t.ID, &t.VoucherID, &t.SourceKType, &t.SourceID,
				&t.ItemID, &t.WarehouseID, &t.Qty, &t.UnitCost, &t.Amount, &t.Weight,
				&t.AllocatedAmount, &t.Applied, &t.CreatedAt, &t.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: list targets: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Allocation
// ---------------------------------------------------------------------------

// AllocateVoucher computes per-target allocated_amount using the
// voucher's allocation_method. Returns the updated targets in the
// same order ListTargets emits. The voucher transitions from draft
// to allocated. Re-running on an already-allocated voucher re-runs
// the math (idempotent — same inputs produce the same shares).
func (s *LandedCostStore) AllocateVoucher(ctx context.Context, tenantID, voucherID uuid.UUID) ([]LandedCostTarget, error) {
	if tenantID == uuid.Nil || voucherID == uuid.Nil {
		return nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	var updated []LandedCostTarget
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var voucher LandedCostVoucher
		if err := scanVoucher(tx.QueryRow(ctx,
			voucherSelectSQL+" WHERE tenant_id = $1 AND id = $2 FOR UPDATE",
			tenantID, voucherID), &voucher); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLandedCostNotFound
			}
			return err
		}
		if voucher.Status == LandedCostStatusPosted {
			return ErrLandedCostAlreadyPosted
		}

		// Load charges + targets inside the tx for a consistent
		// snapshot. The voucher row is FOR UPDATE so concurrent
		// allocators serialise on this row.
		var totalCharges decimal.Decimal
		chargeRows, err := tx.Query(ctx,
			`SELECT amount FROM landed_cost_charges
			  WHERE tenant_id = $1 AND voucher_id = $2`,
			tenantID, voucherID)
		if err != nil {
			return err
		}
		for chargeRows.Next() {
			var amt decimal.Decimal
			if err := chargeRows.Scan(&amt); err != nil {
				chargeRows.Close()
				return err
			}
			totalCharges = totalCharges.Add(amt)
		}
		chargeRows.Close()
		// pgx v5 reports mid-iteration DB errors only via rows.Err()
		// — rows.Next() returns false on either EOF or error. Without
		// this check a partial charge list would silently produce a
		// too-low totalCharges, mis-distributing the allocation.
		if err := chargeRows.Err(); err != nil {
			return err
		}
		if !totalCharges.IsPositive() {
			return ErrLandedCostNoCharges
		}

		targets := []LandedCostTarget{}
		targetRows, err := tx.Query(ctx,
			`SELECT id, qty, unit_cost, amount, weight
			   FROM landed_cost_targets
			  WHERE tenant_id = $1 AND voucher_id = $2
			  ORDER BY created_at`,
			tenantID, voucherID)
		if err != nil {
			return err
		}
		for targetRows.Next() {
			var t LandedCostTarget
			if err := targetRows.Scan(&t.ID, &t.Qty, &t.UnitCost, &t.Amount, &t.Weight); err != nil {
				targetRows.Close()
				return err
			}
			targets = append(targets, t)
		}
		targetRows.Close()
		// Same defence as the charge loop above — a mid-iteration
		// DB error would otherwise drop targets silently and the
		// allocator would spread the charges across a smaller set.
		if err := targetRows.Err(); err != nil {
			return err
		}
		if len(targets) == 0 {
			return ErrLandedCostNoTargets
		}

		// Compute each target's allocated share. The "remainder
		// trick" prevents per-share rounding drift: we pre-compute
		// shares for all targets except the last, then fold the
		// residual into the last so the sum exactly equals
		// totalCharges to four decimal places.
		shares, err := computeAllocation(voucher.AllocationMethod, totalCharges, targets)
		if err != nil {
			return err
		}

		for i := range targets {
			targets[i].AllocatedAmount = shares[i]
			if _, err := tx.Exec(ctx,
				`UPDATE landed_cost_targets
				    SET allocated_amount = $1, updated_at = $2
				  WHERE tenant_id = $3 AND id = $4`,
				shares[i], s.now(), tenantID, targets[i].ID); err != nil {
				return err
			}
		}

		_, err = tx.Exec(ctx,
			`UPDATE landed_cost_vouchers
			    SET status = $1, updated_at = $2
			  WHERE tenant_id = $3 AND id = $4`,
			LandedCostStatusAllocated, s.now(), tenantID, voucherID)
		if err != nil {
			return err
		}

		// Refresh the full target rows so the caller receives the
		// canonical post-allocate snapshot (including applied flag
		// and timestamps) rather than the partial in-flight value.
		full, err := listTargetsTx(ctx, tx, tenantID, voucherID)
		if err != nil {
			return err
		}
		updated = full
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("landed_cost: allocate: %w", err)
	}
	return updated, nil
}

// computeAllocation is the pure-arithmetic core of AllocateVoucher.
// Exposed for unit-testing without a database. Inputs:
//
//   - method: one of LandedCostByQty / LandedCostByAmount /
//     LandedCostByWeight.
//   - totalCharges: sum of all LandedCostCharge.Amount.
//   - targets: one entry per LandedCostTarget; only Qty / Amount /
//     Weight are consulted (the caller is free to leave other fields
//     zero).
//
// Output: a slice of decimal shares parallel to `targets`. The
// shares are non-negative, four-decimal-rounded, and sum exactly to
// totalCharges (residual rounding goes into the last share).
func computeAllocation(method string, totalCharges decimal.Decimal, targets []LandedCostTarget) ([]decimal.Decimal, error) {
	if len(targets) == 0 {
		return nil, ErrLandedCostNoTargets
	}
	if !isKnownAllocationMethod(method) {
		return nil, ErrLandedCostBadMethod
	}
	weights := make([]decimal.Decimal, len(targets))
	var sum decimal.Decimal
	for i := range targets {
		t := &targets[i]
		var w decimal.Decimal
		switch method {
		case LandedCostByQty:
			w = t.Qty
		case LandedCostByAmount:
			w = t.Amount
		case LandedCostByWeight:
			w = t.Weight
		}
		if w.IsNegative() {
			w = decimal.Zero
		}
		weights[i] = w
		sum = sum.Add(w)
	}
	if !sum.IsPositive() {
		if method == LandedCostByWeight {
			return nil, ErrLandedCostZeroWeightTotal
		}
		// Fallback: even split. This only triggers when every
		// target has zero qty/amount, which a CHECK constraint
		// already rejects, so it's a defence-in-depth path.
		even := totalCharges.Div(decimal.NewFromInt(int64(len(targets)))).Round(4)
		shares := make([]decimal.Decimal, len(targets))
		var allocated decimal.Decimal
		for i := range shares {
			if i == len(shares)-1 {
				// Residual on the last share; clamped — see
				// the weighted path below for the same
				// reasoning.
				shares[i] = clampNonNegative(totalCharges.Sub(allocated))
			} else {
				share := even
				if allocated.Add(share).GreaterThan(totalCharges) {
					share = clampNonNegative(totalCharges.Sub(allocated))
				}
				shares[i] = share
				allocated = allocated.Add(share)
			}
		}
		return shares, nil
	}

	shares := make([]decimal.Decimal, len(targets))
	var allocated decimal.Decimal
	for i := range targets {
		if i == len(targets)-1 {
			// Last target absorbs the residual so the sum
			// across shares exactly equals totalCharges. The
			// clamp protects against the degenerate case where
			// the pre-rounded shares already sum to > totalCharges
			// (possible when shopspring/decimal's round-half-away-
			// from-zero pushes many tiny per-share results up):
			// without the clamp the last share goes negative,
			// violating the CHECK (allocated_amount >= 0) on
			// landed_cost_targets.
			shares[i] = clampNonNegative(totalCharges.Sub(allocated))
		} else {
			share := totalCharges.Mul(weights[i]).Div(sum).Round(4)
			// Cap the per-share allocation so the cumulative
			// total never exceeds totalCharges. The residual
			// trick on the last target now never goes negative.
			// Under-allocation by < $0.0001 × (targets-1) is
			// the trade-off and is invisible at the cent
			// granularity the JE is written at.
			if allocated.Add(share).GreaterThan(totalCharges) {
				share = clampNonNegative(totalCharges.Sub(allocated))
			}
			shares[i] = share
			allocated = allocated.Add(share)
		}
	}
	return shares, nil
}

// clampNonNegative returns v if v >= 0, else decimal.Zero. Used by
// computeAllocation to keep per-share shares within the CHECK
// constraint on landed_cost_targets.allocated_amount.
func clampNonNegative(v decimal.Decimal) decimal.Decimal {
	if v.IsNegative() {
		return decimal.Zero
	}
	return v
}

// ---------------------------------------------------------------------------
// Posting
// ---------------------------------------------------------------------------

// PostVoucher applies the allocated voucher: walks each target,
// emits reversal + forward inventory_moves so the moving-average
// cost is re-based, then writes the booking JE and pins
// voucher.je_id. Idempotent — re-posting a posted voucher returns
// the existing JE rather than booking a second one.
//
// The inventory cost-rebase is per-target, sourced via the original
// inventory_moves row identified by (source_ktype, source_id,
// item_id, warehouse_id). Targets that no longer have a matching
// receipt move (e.g. the source AP bill was voided after the
// voucher was created) surface an explanatory error to the caller.
func (s *LandedCostStore) PostVoucher(ctx context.Context, tenantID, voucherID, actorID uuid.UUID) (*LandedCostVoucher, *LandedCostJournalEntry, error) {
	if tenantID == uuid.Nil || voucherID == uuid.Nil {
		return nil, nil, errors.New("landed_cost: tenant id + voucher id required")
	}
	if actorID == uuid.Nil {
		return nil, nil, errors.New("landed_cost: actor required")
	}

	// Phase 1: claim the voucher with a FOR UPDATE lock, then
	// snapshot status / charges / targets inside the same tx.
	// AllocateVoucher uses the same pattern (line ~717) so two
	// concurrent Allocate/Post pairs serialise on the voucher row
	// while the snapshot is read. The lock auto-releases when the
	// tx commits; the subsequent multi-tx posting work (per-target
	// inventory moves + JE write) is idempotent via the
	// (source_ktype, source_id) unique indexes on inventory_moves
	// and journal_entries, so we deliberately do NOT hold a
	// session-scoped advisory lock across Phase 2/3.
	var (
		voucher    *LandedCostVoucher
		charges    []LandedCostCharge
		targets    []LandedCostTarget
		existingJE *LandedCostJournalEntry
	)
	phase1Err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var v LandedCostVoucher
		if err := scanVoucher(tx.QueryRow(ctx,
			voucherSelectSQL+" WHERE tenant_id = $1 AND id = $2 FOR UPDATE",
			tenantID, voucherID), &v); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrLandedCostNotFound
			}
			return err
		}
		voucher = &v
		if v.Status == LandedCostStatusPosted && v.JEID != nil {
			// Idempotent retry: the caller will short-circuit
			// below once Phase 1 returns.
			je, gerr := s.ledger.GetJournalEntry(ctx, tenantID, *v.JEID)
			if gerr != nil {
				return fmt.Errorf("landed_cost: load existing JE: %w", gerr)
			}
			if je == nil {
				// The voucher claims to be posted with a
				// concrete je_id but the JE row is gone
				// (LedgerBackend.GetJournalEntry translates
				// ledger.ErrEntryNotFound into (nil, nil) so
				// the import cycle stays broken — see
				// internal/financeadapters/landed_cost.go).
				// Surface a typed sentinel rather than
				// silently falling through into Phase 2/3,
				// where the empty snapshot would collapse
				// into ErrLandedCostNoCharges and give the
				// operator a misleading recovery hint.
				return ErrLandedCostPostedJEMissing
			}
			existingJE = je
			return nil
		}
		if v.Status != LandedCostStatusAllocated {
			return ErrLandedCostNotAllocated
		}

		cs, err := listChargesTx(ctx, tx, tenantID, voucherID)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			return ErrLandedCostNoCharges
		}
		charges = cs

		ts, err := listTargetsTx(ctx, tx, tenantID, voucherID)
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			return ErrLandedCostNoTargets
		}
		targets = ts
		return nil
	})
	if phase1Err != nil {
		return nil, nil, phase1Err
	}
	if existingJE != nil {
		return voucher, existingJE, nil
	}

	// Phase 2: per-target reversal + forward inventory_moves.
	// Each ReverseMove / RecordMove call opens its own
	// dbutil.WithTenantTx; idempotency is guaranteed by:
	//
	//   * inventory_moves_reversal_of_uniq — at most one reversal
	//     per original move.
	//   * inventory_moves_source_uniq — at most one
	//     (source_ktype, source_id, item_id, warehouse_id) tuple.
	//     The poster keys the forward move on (MoveSourceLandedCost,
	//     target.id, item_id, warehouse_id) so re-posting the same
	//     voucher is a no-op rather than a double-book. Keying on
	//     target.id (not voucher.id) keeps the tuple unique even
	//     when a single voucher has multiple targets on the same
	//     item+warehouse pair (e.g. two receipts of the same SKU).
	for i := range targets {
		t := &targets[i]
		if t.Applied {
			continue
		}
		// Locate the original receipt move so we can reverse it.
		orig, err := s.inv.GetMoveBySource(ctx, tenantID, t.SourceKType, t.SourceID, t.ItemID, t.WarehouseID)
		if err != nil {
			return nil, nil, fmt.Errorf("landed_cost: locate source move for target %d: %w", i, err)
		}
		if err := s.inv.ReverseMove(ctx, tenantID, orig.ID, actorID, fmt.Sprintf("landed cost voucher %s", voucher.VoucherNumber)); err != nil {
			return nil, nil, fmt.Errorf("landed_cost: reverse move for target %d: %w", i, err)
		}

		newUnitCost := t.UnitCost
		if t.Qty.IsPositive() {
			newUnitCost = t.UnitCost.Add(t.AllocatedAmount.Div(t.Qty)).Round(4)
		}
		fwd := LandedCostMoveInput{
			TenantID:    tenantID,
			ItemID:      t.ItemID,
			WarehouseID: t.WarehouseID,
			Qty:         t.Qty,
			UnitCost:    newUnitCost,
			VoucherID:   voucherID,
			TargetID:    t.ID,
			MovedAt:     s.now(),
			ActorID:     actorID,
		}
		if err := s.inv.RecordLandedCostMove(ctx, fwd); err != nil {
			return nil, nil, fmt.Errorf("landed_cost: record forward move for target %d: %w", i, err)
		}

		if err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE landed_cost_targets
				    SET applied = TRUE, updated_at = $1
				  WHERE tenant_id = $2 AND id = $3`,
				s.now(), tenantID, t.ID)
			return err
		}); err != nil {
			return nil, nil, fmt.Errorf("landed_cost: mark target applied: %w", err)
		}
	}

	// Phase 3: write the booking JE. Dr Inventory (sum of
	// allocated_amount across all targets), Cr each charge's
	// account_code. The JE is keyed on (MoveSourceLandedCost,
	// voucher.id) so PostJournalEntry → GetJournalEntryBySource
	// is idempotent: retries return the same entry rather than
	// double-booking.
	//
	// Surface real DB errors from the lookup — the adapter returns
	// (nil, nil) on "no existing entry" and a non-nil error only for
	// real failures (connection drop, RLS deny, query timeout). The
	// previous code discarded the error with `_`, which masked
	// transient failures: if a JE actually existed from a prior
	// partial post but the lookup failed transiently, control fell
	// through to PostLandedCostJournalEntry and the ledger errored
	// with ErrDuplicateSourceEntry, giving callers a misleading
	// "duplicate source" message instead of the genuine lookup
	// failure. The defence-in-depth catch on the Post path below
	// still handles the legitimate race where a concurrent caller
	// beat us between this lookup and the Post.
	existing, err := s.ledger.GetJournalEntryBySource(ctx, tenantID, MoveSourceLandedCost, voucherID)
	if err != nil {
		return nil, nil, fmt.Errorf("landed_cost: lookup existing JE: %w", err)
	}
	if existing != nil {
		// JE already exists from a prior partial retry — promote
		// the voucher and return.
		if err := s.markPosted(ctx, tenantID, voucherID, existing); err != nil {
			return nil, nil, err
		}
		voucher.Status = LandedCostStatusPosted
		voucher.JEID = &existing.ID
		voucher.PostedAt = &existing.PostedAt
		return voucher, existing, nil
	}

	totalAllocated := decimal.Zero
	for i := range targets {
		totalAllocated = totalAllocated.Add(targets[i].AllocatedAmount)
	}
	if !totalAllocated.IsPositive() {
		return nil, nil, ErrLandedCostNoCharges
	}

	// Currency: take it from the first charge's account currency
	// proxy — but the ledger validates currency consistency, so we
	// keep it implicit and rely on the platform default ("USD" if
	// the charge accounts are USD-only). Real multi-currency
	// landed cost is a follow-up phase; this MVP requires all
	// charge accounts to be in the tenant's base currency.
	currency := "USD"
	postedAt := s.now()
	lines := []LandedCostJELine{
		{
			AccountCode: DefaultInventoryAccountCode,
			Debit:       totalAllocated,
			Credit:      decimal.Zero,
			Currency:    currency,
			Memo:        fmt.Sprintf("Landed cost voucher %s — inventory adjust", voucher.VoucherNumber),
		},
	}

	// Sort charges by account_code so the JE's credit lines are in
	// stable order across re-runs. Then group same-account charges
	// into one line each (a voucher with two "freight - DHL" lines
	// against the same GL account collapses into one credit row).
	groups := groupChargesByAccount(charges)
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, LandedCostJELine{
			AccountCode: k,
			Debit:       decimal.Zero,
			Credit:      groups[k],
			Currency:    currency,
			Memo:        fmt.Sprintf("Landed cost voucher %s — %s", voucher.VoucherNumber, k),
		})
	}

	posted, err := s.ledger.PostLandedCostJournalEntry(ctx, LandedCostJEInput{
		TenantID:  tenantID,
		VoucherID: voucherID,
		ActorID:   actorID,
		PostedAt:  postedAt,
		Memo:      fmt.Sprintf("Landed cost voucher %s", voucher.VoucherNumber),
		Lines:     lines,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("landed_cost: post JE: %w", err)
	}
	// LedgerBackend.PostLandedCostJournalEntry is responsible for
	// catching the (source_ktype, source_id) unique-index race — a
	// concurrent poster that landed the JE between the Phase 3
	// existing-entry lookup above and this Post would otherwise
	// surface as ErrDuplicateSourceEntry from the ledger. The
	// adapter (internal/financeadapters/landed_cost.go) absorbs
	// that sentinel and reloads via GetJournalEntryBySource so the
	// store never has to import the ledger sentinel directly
	// (the LedgerBackend abstraction precisely exists to keep that
	// dependency one-way). This mirrors the invoice poster pattern
	// at internal/ledger/invoice.go:243-251.

	if err := s.markPosted(ctx, tenantID, voucherID, posted); err != nil {
		return nil, nil, err
	}
	voucher.Status = LandedCostStatusPosted
	voucher.JEID = &posted.ID
	voucher.PostedAt = &posted.PostedAt
	return voucher, posted, nil
}

// markPosted flips voucher.status to posted and pins je_id /
// posted_at.
func (s *LandedCostStore) markPosted(ctx context.Context, tenantID, voucherID uuid.UUID, je *LandedCostJournalEntry) error {
	if je == nil {
		return errors.New("landed_cost: nil je on markPosted")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE landed_cost_vouchers
			    SET status = $1, je_id = $2, posted_at = $3, updated_at = $4
			  WHERE tenant_id = $5 AND id = $6`,
			LandedCostStatusPosted, je.ID, je.PostedAt, s.now(), tenantID, voucherID)
		return err
	})
}

// groupChargesByAccount collapses charges sharing the same
// account_code into a single credit total per account so the JE's
// credit side stays compact. Empty account_code maps to the
// platform fallback DefaultStockAdjustmentAccountCode.
func groupChargesByAccount(charges []LandedCostCharge) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	for i := range charges {
		c := &charges[i]
		k := c.AccountCode
		if strings.TrimSpace(k) == "" {
			k = DefaultStockAdjustmentAccountCode
		}
		out[k] = out[k].Add(c.Amount)
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

const voucherSelectSQL = `SELECT tenant_id, id, voucher_number, description, status,
       allocation_method, posted_at, je_id, created_by, created_at, updated_at
  FROM landed_cost_vouchers`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanVoucher(row rowScanner, out *LandedCostVoucher) error {
	var (
		postedAt *time.Time
		jeID     *uuid.UUID
	)
	if err := row.Scan(
		&out.TenantID, &out.ID, &out.VoucherNumber, &out.Description, &out.Status,
		&out.AllocationMethod, &postedAt, &jeID, &out.CreatedBy, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return err
	}
	out.PostedAt = postedAt
	out.JEID = jeID
	return nil
}

func listTargetsTx(ctx context.Context, tx pgx.Tx, tenantID, voucherID uuid.UUID) ([]LandedCostTarget, error) {
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, voucher_id, source_ktype, source_id,
		        item_id, warehouse_id, qty, unit_cost, amount, weight,
		        allocated_amount, applied, created_at, updated_at
		   FROM landed_cost_targets
		  WHERE tenant_id = $1 AND voucher_id = $2
		  ORDER BY created_at`,
		tenantID, voucherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LandedCostTarget{}
	for rows.Next() {
		var t LandedCostTarget
		if err := rows.Scan(
			&t.TenantID, &t.ID, &t.VoucherID, &t.SourceKType, &t.SourceID,
			&t.ItemID, &t.WarehouseID, &t.Qty, &t.UnitCost, &t.Amount, &t.Weight,
			&t.AllocatedAmount, &t.Applied, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// listChargesTx is the in-tx twin of ListCharges. Used by
// PostVoucher's Phase 1 snapshot so the voucher FOR UPDATE lock
// and the charge read see a consistent snapshot.
func listChargesTx(ctx context.Context, tx pgx.Tx, tenantID, voucherID uuid.UUID) ([]LandedCostCharge, error) {
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, voucher_id, description, amount, account_code,
		        created_at, updated_at
		   FROM landed_cost_charges
		  WHERE tenant_id = $1 AND voucher_id = $2
		  ORDER BY created_at`,
		tenantID, voucherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LandedCostCharge{}
	for rows.Next() {
		var c LandedCostCharge
		if err := rows.Scan(
			&c.TenantID, &c.ID, &c.VoucherID, &c.Description, &c.Amount,
			&c.AccountCode, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// resetVoucherToDraft demotes an allocated voucher back to draft.
// Called from any UpsertCharge / UpsertTarget / DeleteCharge /
// DeleteTarget path so a mutation invalidates the prior allocation
// and forces the operator to re-allocate before posting. No-op on
// already-draft vouchers; rejects posted vouchers (an explicit
// status check rather than a silent UPDATE) so callers learn of
// the constraint via ErrLandedCostAlreadyPosted instead of a
// confusing "voucher already posted" downstream.
func resetVoucherToDraft(ctx context.Context, tx pgx.Tx, tenantID, voucherID uuid.UUID, now time.Time) error {
	if tenantID == uuid.Nil || voucherID == uuid.Nil {
		return errors.New("landed_cost: tenant id + voucher id required")
	}
	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM landed_cost_vouchers
		  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, voucherID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLandedCostNotFound
		}
		return err
	}
	if status == LandedCostStatusPosted {
		return ErrLandedCostAlreadyPosted
	}
	if status == LandedCostStatusDraft {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE landed_cost_vouchers
		    SET status = $1, updated_at = $2
		  WHERE tenant_id = $3 AND id = $4`,
		LandedCostStatusDraft, now, tenantID, voucherID)
	return err
}

func isKnownAllocationMethod(m string) bool {
	switch m {
	case LandedCostByQty, LandedCostByAmount, LandedCostByWeight:
		return true
	}
	return false
}
