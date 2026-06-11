-- Workstream 2 (step 1) — Lot / serial tracking for inventory.
--
-- Regulated manufacturing (food, pharma, electronics) requires that
-- every unit of stock be traceable to the lot it came from and, for
-- high-value goods, to an individual serial number. Phase G/L already
-- shipped batch (lot) identifiers (migrations/000040_batch_tracking.sql)
-- and an optional inventory_moves.batch_id linkage, but two gaps
-- remained:
--
--   1. There was no item-level *configuration* declaring an item as
--      lot-tracked and/or serial-tracked, so the engine could not
--      enforce that a receipt of a tracked item carries a lot/serial,
--      nor that an issue decrements a *specific* lot/serial.
--   2. There was no serial-number dimension at all: the move ledger
--      could attribute a move to a lot but never to one of N discrete
--      serialised units.
--
-- This migration closes both gaps:
--
--   * inventory_items gains `lot_tracked` / `serial_tracked` flags.
--   * inventory_serials is the per-tenant serial-number master. One
--     row per (tenant_id, item_id, serial_no). A serial carries its
--     current lifecycle status (in_stock / consumed / delivered), its
--     current warehouse (NULL once it leaves stock), and an optional
--     owning lot so a serialised unit can also be lot-traced.
--   * inventory_move_serials is the append-only junction that records
--     which serials each inventory_moves row touched. This is the
--     backbone of forward/backward traceability: from a serial you can
--     walk to every move that affected it, and from each move to the
--     business document (source_ktype/source_id) that drove it.
--
-- RLS follows the canonical multi-tenancy pattern used by
-- inventory_batches (000040): composite (tenant_id, ...) primary key,
-- ENABLE ROW LEVEL SECURITY, a tenant_isolation policy keyed off the
-- app.tenant_id GUC, and GRANT to kapp_app. Composite foreign keys are
-- always (tenant_id, <id>) so cross-tenant linkage is impossible by
-- construction — the same defence inventory_moves.batch_id relies on.
--
-- Reference: frappe/erpnext Serial No master + Batch master + Stock
-- Ledger Entry serial/batch linkage.

-- ---------------------------------------------------------------------------
-- Item-level tracking configuration.
-- ---------------------------------------------------------------------------
-- Both default FALSE so every existing item keeps the pre-Workstream-2
-- behaviour (moves with no lot/serial dimension) until an operator
-- explicitly opts the item into tracking. An item may be both
-- lot-tracked and serial-tracked (e.g. serialised units grouped into a
-- manufacturing lot).
ALTER TABLE inventory_items
    ADD COLUMN IF NOT EXISTS lot_tracked    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE inventory_items
    ADD COLUMN IF NOT EXISTS serial_tracked BOOLEAN NOT NULL DEFAULT FALSE;

-- ---------------------------------------------------------------------------
-- Serial-number master.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS inventory_serials (
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    id           UUID NOT NULL,
    item_id      UUID NOT NULL,
    serial_no    TEXT NOT NULL,
    -- Lifecycle status. 'in_stock' units sit at warehouse_id; terminal
    -- states ('consumed' by manufacturing, 'delivered' to a customer)
    -- have warehouse_id = NULL. The CHECK is added in the companion
    -- 000089 migration alongside the rest of the projection layer.
    status       TEXT NOT NULL DEFAULT 'in_stock',
    -- Current location while in stock; NULL once the serial leaves
    -- stock. MATCH SIMPLE means the composite FK below is not enforced
    -- when warehouse_id IS NULL, which is exactly the terminal-state
    -- semantics we want.
    warehouse_id UUID,
    -- Optional owning lot so a serialised unit is also lot-traceable.
    batch_id     UUID,
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, item_id, serial_no),
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id),
    FOREIGN KEY (tenant_id, warehouse_id) REFERENCES inventory_warehouses (tenant_id, id),
    FOREIGN KEY (tenant_id, batch_id) REFERENCES inventory_batches (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS inventory_serials_item_idx
    ON inventory_serials (tenant_id, item_id);

-- Hot path for issuing: "is this serial currently in stock at this
-- warehouse?". Partial so it only indexes live units.
CREATE INDEX IF NOT EXISTS inventory_serials_in_stock_idx
    ON inventory_serials (tenant_id, item_id, warehouse_id)
    WHERE status = 'in_stock';

CREATE INDEX IF NOT EXISTS inventory_serials_batch_idx
    ON inventory_serials (tenant_id, batch_id)
    WHERE batch_id IS NOT NULL;

ALTER TABLE inventory_serials ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON inventory_serials;
CREATE POLICY tenant_isolation ON inventory_serials
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON inventory_serials TO kapp_app;

-- ---------------------------------------------------------------------------
-- Move ↔ serial junction (append-only).
-- ---------------------------------------------------------------------------
-- One row per (move, serial) pair. A single receipt move of N serials
-- produces N junction rows; a single-serial issue produces one. The
-- (tenant_id, move_id) FK references the partitioned inventory_moves
-- parent (supported since PG 12), so a junction row can never point at
-- another tenant's move. ON DELETE CASCADE is intentionally omitted —
-- inventory_moves is append-only and never deleted, so the junction
-- inherits the same immutability.
CREATE TABLE IF NOT EXISTS inventory_move_serials (
    tenant_id  UUID   NOT NULL REFERENCES tenants(id),
    move_id    BIGINT NOT NULL,
    serial_id  UUID   NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, move_id, serial_id),
    FOREIGN KEY (tenant_id, move_id) REFERENCES inventory_moves (tenant_id, id),
    FOREIGN KEY (tenant_id, serial_id) REFERENCES inventory_serials (tenant_id, id)
);

-- Walk a serial's history in id order: every move that touched it.
CREATE INDEX IF NOT EXISTS inventory_move_serials_serial_idx
    ON inventory_move_serials (tenant_id, serial_id);

ALTER TABLE inventory_move_serials ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON inventory_move_serials;
CREATE POLICY tenant_isolation ON inventory_move_serials
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON inventory_move_serials TO kapp_app;
