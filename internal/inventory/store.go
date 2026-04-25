package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
)

const (
	// pgUniqueViolation is the SQLSTATE class for unique_violation.
	pgUniqueViolation = "23505"

	// inventoryMovesSourceUniqIndex is the partial unique index
	// installed in migrations/000005_inventory.sql. A 23505 on this
	// specific constraint translates into ErrDuplicateSourceMove so
	// retries of the ledger hook are idempotent.
	inventoryMovesSourceUniqIndex = "inventory_moves_source_uniq"

	// inventoryMovesReversalOfUniqIndex is the partial unique index
	// installed in migrations/000035_stock_reversal.sql that prevents
	// the same move from being reversed twice. A 23505 on this
	// constraint translates into ErrAlreadyReversed.
	inventoryMovesReversalOfUniqIndex = "inventory_moves_reversal_of_uniq"
)

// PGStore persists items, warehouses, and stock moves against
// PostgreSQL. Every mutation runs inside dbutil.WithTenantTx so the
// tenant GUC is established before any RLS-protected table is touched;
// event-outbox and audit entries participate in the same transaction
// as the business write.
type PGStore struct {
	pool      *pgxpool.Pool
	publisher events.Publisher
	auditor   audit.Logger
	now       func() time.Time
}

// NewPGStore wires a PGStore from the shared pool and its collaborators.
// A nil publisher or auditor is tolerated so unit tests can run the
// store without the outbox/audit pipeline — every production caller
// should pass non-nil values.
func NewPGStore(pool *pgxpool.Pool, publisher events.Publisher, auditor audit.Logger) *PGStore {
	return &PGStore{
		pool:      pool,
		publisher: publisher,
		auditor:   auditor,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// WithNow lets tests pin the clock to a deterministic value.
func (s *PGStore) WithNow(now func() time.Time) *PGStore {
	s.now = now
	return s
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

// UpsertItem creates or updates an item by (tenant_id, sku). Returns
// the canonical row. SKU + Name + UOM are required.
func (s *PGStore) UpsertItem(ctx context.Context, it Item) (*Item, error) {
	if it.TenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if it.SKU == "" || it.Name == "" || it.UOM == "" {
		return nil, errors.New("inventory: sku, name, and uom required")
	}
	if it.ID == uuid.Nil {
		it.ID = uuid.New()
	}
	out := it
	err := dbutil.WithTenantTx(ctx, s.pool, it.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO inventory_items (tenant_id, id, sku, name, uom, active, reorder_level)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, sku) DO UPDATE SET
			     name = EXCLUDED.name,
			     uom = EXCLUDED.uom,
			     active = EXCLUDED.active,
			     reorder_level = EXCLUDED.reorder_level
			 RETURNING id, sku, name, uom, active, reorder_level`,
			it.TenantID, it.ID, it.SKU, it.Name, it.UOM, it.Active, it.ReorderLevel,
		).Scan(&out.ID, &out.SKU, &out.Name, &out.UOM, &out.Active, &out.ReorderLevel)
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: upsert item: %w", err)
	}
	return &out, nil
}

// GetItem loads an item by id.
func (s *PGStore) GetItem(ctx context.Context, tenantID, id uuid.UUID) (*Item, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("inventory: tenant id and item id required")
	}
	var it Item
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, sku, name, uom, active, reorder_level
			 FROM inventory_items WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(&it.TenantID, &it.ID, &it.SKU, &it.Name, &it.UOM, &it.Active, &it.ReorderLevel)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// GetItemBySKU loads an item by its per-tenant SKU.
func (s *PGStore) GetItemBySKU(ctx context.Context, tenantID uuid.UUID, sku string) (*Item, error) {
	if tenantID == uuid.Nil || sku == "" {
		return nil, errors.New("inventory: tenant id and sku required")
	}
	var it Item
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, sku, name, uom, active, reorder_level
			 FROM inventory_items WHERE tenant_id = $1 AND sku = $2`,
			tenantID, sku,
		).Scan(&it.TenantID, &it.ID, &it.SKU, &it.Name, &it.UOM, &it.Active, &it.ReorderLevel)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// ListItems returns items in sku order. Active filter is applied when set.
func (s *PGStore) ListItems(ctx context.Context, tenantID uuid.UUID, filter ItemFilter) ([]Item, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	out := make([]Item, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			conds  = []string{"tenant_id = $1"}
			args   = []any{tenantID}
			nextID = 2
		)
		if filter.Active != nil {
			conds = append(conds, fmt.Sprintf("active = $%d", nextID))
			args = append(args, *filter.Active)
			nextID++
		}
		args = append(args, filter.Limit, filter.Offset)
		q := fmt.Sprintf(
			`SELECT tenant_id, id, sku, name, uom, active, reorder_level
			 FROM inventory_items
			 WHERE %s
			 ORDER BY sku
			 LIMIT $%d OFFSET $%d`,
			strings.Join(conds, " AND "), nextID, nextID+1,
		)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("inventory: list items: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it Item
			if err := rows.Scan(&it.TenantID, &it.ID, &it.SKU, &it.Name, &it.UOM, &it.Active, &it.ReorderLevel); err != nil {
				return fmt.Errorf("inventory: scan item: %w", err)
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Warehouses
// ---------------------------------------------------------------------------

// UpsertWarehouse creates or updates a warehouse by (tenant_id, code).
func (s *PGStore) UpsertWarehouse(ctx context.Context, wh Warehouse) (*Warehouse, error) {
	if wh.TenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if wh.Code == "" || wh.Name == "" {
		return nil, errors.New("inventory: code and name required")
	}
	if wh.ID == uuid.Nil {
		wh.ID = uuid.New()
	}
	out := wh
	err := dbutil.WithTenantTx(ctx, s.pool, wh.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO inventory_warehouses (tenant_id, id, code, name)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, code) DO UPDATE SET name = EXCLUDED.name
			 RETURNING id, code, name`,
			wh.TenantID, wh.ID, wh.Code, wh.Name,
		).Scan(&out.ID, &out.Code, &out.Name)
	})
	if err != nil {
		return nil, fmt.Errorf("inventory: upsert warehouse: %w", err)
	}
	return &out, nil
}

// GetWarehouseByCode loads a warehouse by its per-tenant code.
func (s *PGStore) GetWarehouseByCode(ctx context.Context, tenantID uuid.UUID, code string) (*Warehouse, error) {
	if tenantID == uuid.Nil || code == "" {
		return nil, errors.New("inventory: tenant id and code required")
	}
	var wh Warehouse
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, code, name
			 FROM inventory_warehouses WHERE tenant_id = $1 AND code = $2`,
			tenantID, code,
		).Scan(&wh.TenantID, &wh.ID, &wh.Code, &wh.Name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWarehouseNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &wh, nil
}

// ListWarehouses returns warehouses in code order.
func (s *PGStore) ListWarehouses(ctx context.Context, tenantID uuid.UUID) ([]Warehouse, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	out := make([]Warehouse, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, code, name
			 FROM inventory_warehouses
			 WHERE tenant_id = $1
			 ORDER BY code`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("inventory: list warehouses: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var wh Warehouse
			if err := rows.Scan(&wh.TenantID, &wh.ID, &wh.Code, &wh.Name); err != nil {
				return fmt.Errorf("inventory: scan warehouse: %w", err)
			}
			out = append(out, wh)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Moves
// ---------------------------------------------------------------------------

// RecordMove appends a single stock move to inventory_moves. The move
// is signed — positive quantities are receipts, negative quantities are
// deliveries — and rows are never mutated or deleted, so a correction
// is expressed as a contra-entry.
//
// When SourceKType+SourceID are set, the partial unique index
// `inventory_moves_source_uniq` guarantees at-most-one move per
// (source_ktype, source_id, item_id, warehouse_id). A retry therefore
// surfaces ErrDuplicateSourceMove rather than double-posting.
func (s *PGStore) RecordMove(ctx context.Context, m Move) (*Move, error) {
	if m.TenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if m.ItemID == uuid.Nil || m.WarehouseID == uuid.Nil {
		return nil, fmt.Errorf("%w: item_id and warehouse_id required", ErrMoveInvalid)
	}
	if m.Qty.IsZero() {
		return nil, fmt.Errorf("%w: qty must be non-zero", ErrMoveInvalid)
	}
	if m.MovedAt.IsZero() {
		m.MovedAt = s.now()
	}
	var srcKType any
	var srcID any
	if m.SourceKType != "" {
		srcKType = m.SourceKType
	}
	if m.SourceID != nil {
		srcID = *m.SourceID
	}
	var unitCost any
	if m.UnitCost.IsPositive() || m.UnitCost.IsNegative() {
		unitCost = m.UnitCost
	}

	out := m
	err := dbutil.WithTenantTx(ctx, s.pool, m.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO inventory_moves
			     (tenant_id, item_id, warehouse_id, qty, unit_cost, source_ktype, source_id, moved_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING id`,
			m.TenantID, m.ItemID, m.WarehouseID, m.Qty, unitCost, srcKType, srcID, m.MovedAt,
		).Scan(&out.ID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) &&
				pgErr.Code == pgUniqueViolation &&
				pgErr.ConstraintName == inventoryMovesSourceUniqIndex {
				return ErrDuplicateSourceMove
			}
			return fmt.Errorf("inventory: insert move: %w", err)
		}
		return s.emitMove(ctx, tx, out, "inventory.move.recorded")
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordTransfer appends two balanced moves (negative on from,
// positive on to) inside one transaction so stock levels remain
// conserved. Returns both moves in source-then-destination order.
func (s *PGStore) RecordTransfer(ctx context.Context, t Transfer) ([]Move, error) {
	if t.TenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if t.ItemID == uuid.Nil {
		return nil, fmt.Errorf("%w: item_id required", ErrMoveInvalid)
	}
	if t.FromWarehouse == uuid.Nil || t.ToWarehouse == uuid.Nil || t.FromWarehouse == t.ToWarehouse {
		return nil, ErrTransferUnbalanced
	}
	if !t.Qty.IsPositive() {
		return nil, ErrTransferUnbalanced
	}
	if t.MovedAt.IsZero() {
		t.MovedAt = s.now()
	}
	var unitCost any
	if t.UnitCost.IsPositive() {
		unitCost = t.UnitCost
	}

	out := make([]Move, 0, 2)
	err := dbutil.WithTenantTx(ctx, s.pool, t.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		neg := Move{
			TenantID:    t.TenantID,
			ItemID:      t.ItemID,
			WarehouseID: t.FromWarehouse,
			Qty:         t.Qty.Neg(),
			UnitCost:    t.UnitCost,
			SourceKType: MoveSourceTransfer,
			MovedAt:     t.MovedAt,
			CreatedBy:   t.CreatedBy,
		}
		pos := Move{
			TenantID:    t.TenantID,
			ItemID:      t.ItemID,
			WarehouseID: t.ToWarehouse,
			Qty:         t.Qty,
			UnitCost:    t.UnitCost,
			SourceKType: MoveSourceTransfer,
			MovedAt:     t.MovedAt,
			CreatedBy:   t.CreatedBy,
		}
		for _, m := range []Move{neg, pos} {
			row := m
			err := tx.QueryRow(ctx,
				`INSERT INTO inventory_moves
				     (tenant_id, item_id, warehouse_id, qty, unit_cost, source_ktype, source_id, moved_at)
				 VALUES ($1, $2, $3, $4, $5, $6, NULL, $7)
				 RETURNING id`,
				row.TenantID, row.ItemID, row.WarehouseID, row.Qty, unitCost, row.SourceKType, row.MovedAt,
			).Scan(&row.ID)
			if err != nil {
				return fmt.Errorf("inventory: insert transfer move: %w", err)
			}
			if err := s.emitMove(ctx, tx, row, "inventory.move.recorded"); err != nil {
				return err
			}
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReverseMove posts a contra-entry that exactly cancels the move
// identified by moveID. The contra row is signed-opposite (negative
// of the original Qty) and points back via reversal_of so the audit
// trail is explicit; the original is left untouched (inventory_moves
// is append-only). Stock levels are conserved automatically because
// the stock_levels view sums the ledger and the contra row's
// negative qty offsets the original.
//
// Idempotency: the partial unique index inventory_moves_reversal_of_uniq
// prevents the same move from being reversed twice — a duplicate
// surfaces ErrAlreadyReversed. Reversing a contra-entry directly
// is rejected with ErrCannotReverseContra so callers do not
// accidentally re-issue the original; reverse the original move
// again instead.
//
// actor is recorded on the new move's audit entry; pass uuid.Nil
// for system-driven reversals.
//
// Reference: frappe/erpnext Stock Entry cancellation (which posts
// reverse Stock Ledger Entries with is_cancelled=1).
func (s *PGStore) ReverseMove(ctx context.Context, tenantID uuid.UUID, moveID int64, actor uuid.UUID, memo string) (*Move, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if moveID <= 0 {
		return nil, fmt.Errorf("%w: move id required", ErrMoveInvalid)
	}
	var out Move
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			origQty      decimal.Decimal
			origItem     uuid.UUID
			origWh       uuid.UUID
			origUnitCost decimal.NullDecimal
			origReversal *int64
		)
		// source_ktype / source_id are intentionally NOT copied to
		// the contra-entry: the contra row is its own artifact, not
		// a second move from the same source. Copying them would
		// collide with inventory_moves_source_uniq (which only
		// allows one move per tenant+source tuple) for every move
		// that has a non-NULL source_id — e.g. moves created by the
		// invoice poster via PosterHook. The contra row leaves both
		// columns NULL so it sits outside the partial unique index.
		err := tx.QueryRow(ctx,
			`SELECT item_id, warehouse_id, qty, unit_cost, reversal_of
			   FROM inventory_moves WHERE tenant_id = $1 AND id = $2`,
			tenantID, moveID,
		).Scan(&origItem, &origWh, &origQty, &origUnitCost, &origReversal)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMoveNotFound
			}
			return fmt.Errorf("inventory: load move %d: %w", moveID, err)
		}
		if origReversal != nil {
			return ErrCannotReverseContra
		}

		now := s.now()
		newQty := origQty.Neg()
		var unitCostArg any
		if origUnitCost.Valid {
			unitCostArg = origUnitCost.Decimal
		}
		err = tx.QueryRow(ctx,
			`INSERT INTO inventory_moves
			     (tenant_id, item_id, warehouse_id, qty, unit_cost, source_ktype, source_id, moved_at, reversal_of)
			 VALUES ($1, $2, $3, $4, $5, NULL, NULL, $6, $7)
			 RETURNING id`,
			tenantID, origItem, origWh, newQty, unitCostArg, now, moveID,
		).Scan(&out.ID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
				if pgErr.ConstraintName == inventoryMovesReversalOfUniqIndex {
					return ErrAlreadyReversed
				}
				// Any other unique-violation is a programmer bug
				// (source_uniq shouldn't fire now that we null out
				// source_ktype/source_id on the contra row), but
				// surface it clearly instead of masquerading as
				// ErrAlreadyReversed.
				return fmt.Errorf("inventory: insert reversal: unexpected unique violation on %q: %w", pgErr.ConstraintName, err)
			}
			return fmt.Errorf("inventory: insert reversal: %w", err)
		}
		out.TenantID = tenantID
		out.ItemID = origItem
		out.WarehouseID = origWh
		out.Qty = newQty
		if origUnitCost.Valid {
			out.UnitCost = origUnitCost.Decimal
		}
		// SourceKType / SourceID left zero: contra rows carry no
		// forward source pointer (reversal_of is the backward one).
		out.MovedAt = now
		out.CreatedBy = actor
		reversedID := moveID
		out.ReversalOf = &reversedID
		_ = memo // memo is currently informational; logged via audit when wired through the agent tool / API
		return s.emitMove(ctx, tx, out, "inventory.move.reversed")
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMoves returns moves ordered by moved_at DESC, filtered by
// optional item / warehouse / source / date range.
func (s *PGStore) ListMoves(ctx context.Context, tenantID uuid.UUID, filter MoveFilter) ([]Move, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}
	out := make([]Move, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			conds  = []string{"tenant_id = $1"}
			args   = []any{tenantID}
			nextID = 2
		)
		if filter.ItemID != nil {
			conds = append(conds, fmt.Sprintf("item_id = $%d", nextID))
			args = append(args, *filter.ItemID)
			nextID++
		}
		if filter.WarehouseID != nil {
			conds = append(conds, fmt.Sprintf("warehouse_id = $%d", nextID))
			args = append(args, *filter.WarehouseID)
			nextID++
		}
		if filter.SourceKType != "" {
			conds = append(conds, fmt.Sprintf("source_ktype = $%d", nextID))
			args = append(args, filter.SourceKType)
			nextID++
		}
		if filter.SourceID != nil {
			conds = append(conds, fmt.Sprintf("source_id = $%d", nextID))
			args = append(args, *filter.SourceID)
			nextID++
		}
		if filter.From != nil {
			conds = append(conds, fmt.Sprintf("moved_at >= $%d", nextID))
			args = append(args, *filter.From)
			nextID++
		}
		if filter.To != nil {
			conds = append(conds, fmt.Sprintf("moved_at <= $%d", nextID))
			args = append(args, *filter.To)
			nextID++
		}
		args = append(args, filter.Limit, filter.Offset)
		q := fmt.Sprintf(
			`SELECT id, tenant_id, item_id, warehouse_id, qty, unit_cost,
			        source_ktype, source_id, moved_at
			 FROM inventory_moves
			 WHERE %s
			 ORDER BY moved_at DESC, id DESC
			 LIMIT $%d OFFSET $%d`,
			strings.Join(conds, " AND "), nextID, nextID+1,
		)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("inventory: list moves: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				m        Move
				unitCost *decimal.Decimal
				srcKType *string
				srcID    *uuid.UUID
			)
			if err := rows.Scan(
				&m.ID, &m.TenantID, &m.ItemID, &m.WarehouseID, &m.Qty,
				&unitCost, &srcKType, &srcID, &m.MovedAt,
			); err != nil {
				return fmt.Errorf("inventory: scan move: %w", err)
			}
			if unitCost != nil {
				m.UnitCost = *unitCost
			}
			if srcKType != nil {
				m.SourceKType = *srcKType
			}
			m.SourceID = srcID
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetMoveBySource looks up an existing move by (source_ktype, source_id,
// item_id, warehouse_id). Used by the ledger hook to reuse a previously
// recorded move after a partial failure. Returns ErrItemNotFound when
// no such move exists (the same sentinel is reused so callers have a
// single "not found" branch).
func (s *PGStore) GetMoveBySource(
	ctx context.Context, tenantID uuid.UUID,
	sourceKType string, sourceID, itemID, warehouseID uuid.UUID,
) (*Move, error) {
	if tenantID == uuid.Nil || sourceKType == "" || sourceID == uuid.Nil {
		return nil, errors.New("inventory: tenant id, source_ktype, source_id required")
	}
	var m Move
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			unitCost *decimal.Decimal
			srcKType *string
			srcID    *uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, item_id, warehouse_id, qty, unit_cost,
			        source_ktype, source_id, moved_at
			 FROM inventory_moves
			 WHERE tenant_id = $1 AND source_ktype = $2 AND source_id = $3
			   AND item_id = $4 AND warehouse_id = $5
			 LIMIT 1`,
			tenantID, sourceKType, sourceID, itemID, warehouseID,
		).Scan(
			&m.ID, &m.TenantID, &m.ItemID, &m.WarehouseID, &m.Qty,
			&unitCost, &srcKType, &srcID, &m.MovedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrItemNotFound
		}
		if err != nil {
			return fmt.Errorf("inventory: load move by source: %w", err)
		}
		if unitCost != nil {
			m.UnitCost = *unitCost
		}
		if srcKType != nil {
			m.SourceKType = *srcKType
		}
		m.SourceID = srcID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ---------------------------------------------------------------------------
// Stock levels
// ---------------------------------------------------------------------------

// ListStockLevels returns every (item, warehouse) with its current
// quantity. `itemID` narrows to a single item when non-nil.
func (s *PGStore) ListStockLevels(ctx context.Context, tenantID uuid.UUID, itemID *uuid.UUID) ([]StockLevel, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	out := make([]StockLevel, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			q    string
			args []any
		)
		if itemID != nil {
			q = `SELECT tenant_id, item_id, warehouse_id, qty
			     FROM stock_levels
			     WHERE tenant_id = $1 AND item_id = $2
			     ORDER BY warehouse_id`
			args = []any{tenantID, *itemID}
		} else {
			q = `SELECT tenant_id, item_id, warehouse_id, qty
			     FROM stock_levels
			     WHERE tenant_id = $1
			     ORDER BY item_id, warehouse_id`
			args = []any{tenantID}
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("inventory: list stock levels: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var sl StockLevel
			if err := rows.Scan(&sl.TenantID, &sl.ItemID, &sl.WarehouseID, &sl.Qty); err != nil {
				return fmt.Errorf("inventory: scan stock level: %w", err)
			}
			out = append(out, sl)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ValuationRow is one line of the inventory valuation report.
type ValuationRow struct {
	ItemID    uuid.UUID       `json:"item_id"`
	SKU       string          `json:"sku"`
	Name      string          `json:"name"`
	Qty       decimal.Decimal `json:"qty"`
	ValueCost decimal.Decimal `json:"value_cost"`
}

// ValuationReport is the response shape for the inventory valuation endpoint.
type ValuationReport struct {
	AsOf       time.Time       `json:"as_of"`
	Rows       []ValuationRow  `json:"rows"`
	TotalValue decimal.Decimal `json:"total_value"`
}

// Valuation computes total qty + cost (SUM(qty*unit_cost)) per item
// across all warehouses. Moves without a unit_cost contribute 0 to the
// cost column but still contribute to qty.
func (s *PGStore) Valuation(ctx context.Context, tenantID uuid.UUID, asOf time.Time) (*ValuationReport, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if asOf.IsZero() {
		asOf = s.now()
	}
	rep := &ValuationReport{AsOf: asOf, Rows: make([]ValuationRow, 0)}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT i.id, i.sku, i.name,
			        COALESCE(SUM(m.qty), 0) AS qty,
			        COALESCE(SUM(m.qty * COALESCE(m.unit_cost, 0)), 0) AS value_cost
			   FROM inventory_items i
			   LEFT JOIN inventory_moves m
			          ON m.tenant_id = i.tenant_id
			         AND m.item_id   = i.id
			         AND m.moved_at <= $2
			  WHERE i.tenant_id = $1
			  GROUP BY i.id, i.sku, i.name
			  ORDER BY i.sku`,
			tenantID, asOf,
		)
		if err != nil {
			return fmt.Errorf("inventory: valuation query: %w", err)
		}
		defer rows.Close()
		total := decimal.Zero
		for rows.Next() {
			var row ValuationRow
			if err := rows.Scan(&row.ItemID, &row.SKU, &row.Name, &row.Qty, &row.ValueCost); err != nil {
				return fmt.Errorf("inventory: scan valuation: %w", err)
			}
			total = total.Add(row.ValueCost)
			rep.Rows = append(rep.Rows, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rep.TotalValue = total
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Emit helpers
// ---------------------------------------------------------------------------

// emitMove writes the outbox event + audit entry for a newly-recorded
// move. Participates in the caller's tenant transaction so the ledger,
// outbox, and audit log all commit together.
func (s *PGStore) emitMove(ctx context.Context, tx pgx.Tx, m Move, eventType string) error {
	payload, _ := json.Marshal(map[string]any{
		"id":           m.ID,
		"item_id":      m.ItemID,
		"warehouse_id": m.WarehouseID,
		"qty":          m.Qty.String(),
		"unit_cost":    m.UnitCost.String(),
		"source_ktype": m.SourceKType,
		"source_id":    m.SourceID,
		"reversal_of":  m.ReversalOf,
		"moved_at":     m.MovedAt,
	})
	if s.publisher != nil {
		if err := s.publisher.EmitTx(ctx, tx, events.Event{
			TenantID: m.TenantID,
			Type:     eventType,
			Payload:  payload,
		}); err != nil {
			return err
		}
	}
	if s.auditor != nil {
		actor := m.CreatedBy
		var actorPtr *uuid.UUID
		if actor != uuid.Nil {
			actorPtr = &actor
		}
		if err := s.auditor.LogTx(ctx, tx, audit.Entry{
			TenantID:    m.TenantID,
			ActorID:     actorPtr,
			ActorKind:   audit.ActorUser,
			Action:      eventType,
			TargetKType: KTypeMove,
			After:       payload,
		}); err != nil {
			return err
		}
	}
	return nil
}
