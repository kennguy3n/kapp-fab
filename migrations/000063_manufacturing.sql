-- Phase N6 — Manufacturing Light.
--
-- Three tables form the minimum manufacturing surface that closes the
-- biggest module gap vs ERPNext per the audit (Recommendation #3):
--
--   * boms             — Bill of Materials master. One row per
--                        (tenant_id, item_id, version). status drives
--                        a draft/active/obsolete lifecycle; only the
--                        active row for a given item_id can be used
--                        by a work order. The partial unique index
--                        boms_active_per_item_uniq enforces the
--                        single-active-row invariant.
--
--   * bom_components   — Components consumed by a BOM. One row per
--                        (tenant_id, bom_id, component_item_id).
--                        qty is the per-unit quantity needed to
--                        produce one finished good; scrap_percent
--                        (NULL or 0-100) reserves additional material
--                        for spoilage on work-order completion.
--                        Two-phase consumption math lives in the Go
--                        layer (internal/manufacturing/work_order.go)
--                        so the SQL stays declarative.
--
--   * work_orders      — A single production run against an active
--                        BOM. status walks draft → released →
--                        in_progress → completed → closed, with a
--                        cancelled terminal state from any of the
--                        first three.  actual_qty captures yield
--                        (>= 0 and <= planned_qty + reasonable
--                        tolerance enforced in the Go layer).
--                        warehouse_id is the receipt destination for
--                        the finished-goods stock move emitted on
--                        completion; the component-consumption moves
--                        debit the same warehouse so a single-
--                        warehouse SME doesn't have to model
--                        multiple stocking locations to use the
--                        feature. Multi-warehouse routing is deferred.
--
-- All three tables follow the canonical tenant-scoped pattern:
-- composite (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY,
-- a tenant_isolation policy keyed off app.tenant_id, and GRANT to
-- kapp_app. The migration-rls-check CI gate (services/api/
-- migrations_rls_test.go) enforces the RLS requirement.
--
-- Stock moves emitted on work-order completion are recorded against
-- inventory_moves with source_ktype = 'manufacturing.work_order' and
-- source_id = work_order.id, so the existing
-- inventory_moves_source_uniq partial unique index makes completion
-- idempotent on retry — a half-completed work order can be re-completed
-- without double-issuing the components or double-receipting the
-- finished good.
--
-- Reference: frappe/erpnext Bill of Materials + Work Order pattern,
-- simplified to skip routings, capacity planning, and shop-floor
-- control (deferred per PROPOSAL.md).

-- ---------------------------------------------------------------------------
-- BOMs
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS boms (
    tenant_id     UUID    NOT NULL REFERENCES tenants(id),
    id            UUID    NOT NULL,
    item_id       UUID    NOT NULL,
    version       TEXT    NOT NULL,
    status        TEXT    NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'active', 'obsolete')),
    output_qty    NUMERIC(20, 6) NOT NULL DEFAULT 1
                  CHECK (output_qty > 0),
    uom           TEXT    NOT NULL DEFAULT 'ea',
    notes         TEXT,
    created_by    UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, item_id, version),
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id)
);

-- The single-active-row invariant per item is the load-bearing
-- constraint of the whole module: a work order references an item
-- and the Go layer resolves it to "the unique active BOM for this
-- item". If two BOMs were active simultaneously we'd silently pick
-- whichever sorted first and SME users would get confusing yields.
-- A partial unique index makes the rule a 23505 instead of a logic
-- check.
CREATE UNIQUE INDEX IF NOT EXISTS boms_active_per_item_uniq
    ON boms (tenant_id, item_id)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS boms_status_idx
    ON boms (tenant_id, status);

ALTER TABLE boms ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON boms;
CREATE POLICY tenant_isolation ON boms
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON boms TO kapp_app;

-- ---------------------------------------------------------------------------
-- BOM components
-- ---------------------------------------------------------------------------
-- One row per component consumed to produce one batch of the parent
-- BOM's output. qty is the per-output-batch quantity; scrap_percent
-- (NULL ≡ 0) reserves additional material for spoilage. The Go layer
-- multiplies (planned_qty / parent.output_qty) by qty *
-- (1 + scrap_percent/100) when emitting consumption moves at
-- completion time.
--
-- Composite FK (tenant_id, bom_id) on the parent ensures cross-tenant
-- linkage is impossible; (tenant_id, component_item_id) likewise
-- requires the component item to live in the same tenant as the BOM.
CREATE TABLE IF NOT EXISTS bom_components (
    tenant_id           UUID    NOT NULL REFERENCES tenants(id),
    bom_id              UUID    NOT NULL,
    component_item_id   UUID    NOT NULL,
    qty                 NUMERIC(20, 6) NOT NULL CHECK (qty > 0),
    uom                 TEXT    NOT NULL DEFAULT 'ea',
    scrap_percent       NUMERIC(6, 2)
                        CHECK (scrap_percent IS NULL OR (scrap_percent >= 0 AND scrap_percent <= 100)),
    sort_order          INTEGER NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bom_id, component_item_id),
    FOREIGN KEY (tenant_id, bom_id) REFERENCES boms (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, component_item_id) REFERENCES inventory_items (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS bom_components_component_idx
    ON bom_components (tenant_id, component_item_id);

ALTER TABLE bom_components ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bom_components;
CREATE POLICY tenant_isolation ON bom_components
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON bom_components TO kapp_app;

-- ---------------------------------------------------------------------------
-- Work orders
-- ---------------------------------------------------------------------------
-- A single production run. status is the load-bearing field; legal
-- transitions are enforced in the Go state machine, not in SQL, so
-- the error surface is a typed error instead of a CHECK violation.
-- planned_qty is required up front; actual_qty is NULL until the
-- order completes (yield can be < planned, e.g. spoilage).
--
-- bom_id is captured at release time and is immutable thereafter, so
-- the consumption math at completion is reproducible even if the BOM
-- is later marked obsolete or a new version is activated for the
-- same item.
CREATE TABLE IF NOT EXISTS work_orders (
    tenant_id        UUID    NOT NULL REFERENCES tenants(id),
    id               UUID    NOT NULL,
    item_id          UUID    NOT NULL,
    bom_id           UUID,
    warehouse_id     UUID    NOT NULL,
    planned_qty      NUMERIC(20, 6) NOT NULL CHECK (planned_qty > 0),
    actual_qty       NUMERIC(20, 6) CHECK (actual_qty IS NULL OR actual_qty >= 0),
    status           TEXT    NOT NULL DEFAULT 'draft'
                     CHECK (status IN ('draft', 'released', 'in_progress', 'completed', 'closed', 'cancelled')),
    scheduled_start  TIMESTAMPTZ,
    scheduled_end    TIMESTAMPTZ,
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    notes            TEXT,
    created_by       UUID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id),
    FOREIGN KEY (tenant_id, warehouse_id) REFERENCES inventory_warehouses (tenant_id, id),
    FOREIGN KEY (tenant_id, bom_id) REFERENCES boms (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS work_orders_status_idx
    ON work_orders (tenant_id, status);

CREATE INDEX IF NOT EXISTS work_orders_item_idx
    ON work_orders (tenant_id, item_id);

ALTER TABLE work_orders ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON work_orders;
CREATE POLICY tenant_isolation ON work_orders
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON work_orders TO kapp_app;

-- ---------------------------------------------------------------------------
-- Stabilise the auto-generated partition child index names on
-- inventory_moves_default.
--
-- inventory_moves is a partitioned parent (see migrations/
-- 000001_initial_schema.sql:272). When the parent-level partial unique
-- indexes inventory_moves_source_uniq (000005_inventory.sql) and
-- inventory_moves_reversal_of_uniq (000035_stock_reversal.sql) were
-- created, PostgreSQL also created child indexes on the default
-- partition with names auto-derived from the column tuple and
-- truncated at the 63-character identifier limit. The exact truncation
-- point depends on the partition table name AND the column list, and
-- changes if any column tuple is altered — so matching the child name
-- by suffix in Go (inventory/store.go) is fragile across PG versions
-- and refactors.
--
-- This migration renames every auto-generated child index on every
-- partition of inventory_moves to a deterministic name of the form
-- `<partition_relname>_<short_suffix>` (e.g. inventory_moves_default
-- yields `inventory_moves_default_source_uniq`). The matcher on the
-- Go side uses a prefix+suffix pattern (see
-- internal/inventory/store.go `isInventoryMovesSourceUniqViolation`),
-- so any partition added in the future — yearly partitions, tenant-
-- specific shards, archive partitions — also produces a match without
-- a follow-up migration. The loop is the load-bearing piece: an
-- earlier draft used `SELECT ... INTO` which only captures one row,
-- so if more than one partition existed only the first child got
-- renamed and the rest stayed on the auto-generated name (which the
-- Go matcher would not recognise → ErrDuplicateSourceMove silently
-- regresses to a generic insert error).
--
-- The renames are idempotent: each iteration checks the current child
-- name against the target before issuing the ALTER. Running the
-- migration twice (or against a freshly-built schema that already has
-- the canonical names) is a no-op.
DO $$
DECLARE
    rec record;
    target_name text;
BEGIN
    -- inventory_moves_source_uniq → <partition>_source_uniq for every partition
    FOR rec IN
        SELECT c.relname AS child_name, partition_tbl.relname AS partition_relname
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        JOIN pg_inherits h ON h.inhrelid = c.oid
        JOIN pg_class parent_idx ON parent_idx.oid = h.inhparent
        JOIN pg_class partition_tbl ON partition_tbl.oid = i.indrelid
        WHERE parent_idx.relname = 'inventory_moves_source_uniq'
    LOOP
        target_name := rec.partition_relname || '_source_uniq';
        IF rec.child_name <> target_name THEN
            EXECUTE format('ALTER INDEX %I RENAME TO %I', rec.child_name, target_name);
        END IF;
    END LOOP;

    -- inventory_moves_reversal_of_uniq → <partition>_reversal_of_uniq for every partition
    FOR rec IN
        SELECT c.relname AS child_name, partition_tbl.relname AS partition_relname
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        JOIN pg_inherits h ON h.inhrelid = c.oid
        JOIN pg_class parent_idx ON parent_idx.oid = h.inhparent
        JOIN pg_class partition_tbl ON partition_tbl.oid = i.indrelid
        WHERE parent_idx.relname = 'inventory_moves_reversal_of_uniq'
    LOOP
        target_name := rec.partition_relname || '_reversal_of_uniq';
        IF rec.child_name <> target_name THEN
            EXECUTE format('ALTER INDEX %I RENAME TO %I', rec.child_name, target_name);
        END IF;
    END LOOP;
END
$$;
