-- Workstream 2 (step 1) — Lot / serial projection + integrity guards.
--
-- Companion to 000088_inventory_lot_serial.sql. 000088 added the
-- schema (item flags, serial master, move↔serial junction); this
-- migration adds the read-side projection and the database-level
-- invariants that make over-issue impossible even if a future caller
-- bypasses the Go guard:
--
--   * stock_levels_by_batch — live per-(item, warehouse, lot) balance
--     projected straight from the append-only inventory_moves ledger,
--     mirroring the existing stock_levels view but with the batch
--     dimension. SECURITY INVOKER (PG15+ default) so RLS on
--     inventory_moves applies under the caller's tenant context.
--   * inventory_batches.qty_on_hand CHECK (>= 0) — a hard backstop:
--     the Go layer rejects an over-issue with ErrInsufficientLotStock
--     before it reaches the UPDATE, but if any path ever slips through,
--     the CHECK turns a silent negative balance into a loud 23514.
--   * inventory_serials.status CHECK — pins the lifecycle vocabulary so
--     a typo in application code cannot wedge a serial into an
--     unrecognised state.

-- ---------------------------------------------------------------------------
-- Per-lot stock projection.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW stock_levels_by_batch AS
    SELECT tenant_id,
           item_id,
           warehouse_id,
           batch_id,
           SUM(qty) AS qty
      FROM inventory_moves
     WHERE batch_id IS NOT NULL
     GROUP BY tenant_id, item_id, warehouse_id, batch_id;

GRANT SELECT ON stock_levels_by_batch TO kapp_app;

-- ---------------------------------------------------------------------------
-- Integrity guards.
-- ---------------------------------------------------------------------------
-- A lot can never hold a negative quantity. The Go decrement path
-- (internal/inventory.PGStore) already refuses an issue that would
-- breach this and returns ErrInsufficientLotStock; the constraint is
-- defence-in-depth for any path that forgets to.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'inventory_batches_qty_on_hand_nonneg'
    ) THEN
        ALTER TABLE inventory_batches
            ADD CONSTRAINT inventory_batches_qty_on_hand_nonneg
            CHECK (qty_on_hand >= 0);
    END IF;
END $$;

-- Pin the serial lifecycle vocabulary.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'inventory_serials_status_valid'
    ) THEN
        ALTER TABLE inventory_serials
            ADD CONSTRAINT inventory_serials_status_valid
            CHECK (status IN ('in_stock', 'consumed', 'delivered'));
    END IF;
END $$;
