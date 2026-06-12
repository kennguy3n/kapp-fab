-- Batch-2 manufacturing depth (Workstream — MRP run). Adds material
-- requirements planning on top of the make-to-stock loop shipped in
-- 000063 (BOMs + work orders) and 000080 (routings / capacity / shop
-- floor) and the lot/serial tracking from 000088/000089.
--
-- An MRP run takes a snapshot of independent demand (sales orders,
-- work orders, or min-stock top-ups), nets it against supply (on-hand
-- stock plus the open work orders already scheduled to receive the
-- item), explodes the active BOM of every make item to derive the
-- dependent demand for its components, and emits planned orders
-- (make vs buy) with a suggested release date computed by backward
-- scheduling from the demand due date over the item's lead time
-- (routing-derived for make items, a fixed purchasing lead time for
-- buy items). The whole run is persisted so a planner can audit which
-- inputs produced which planned orders:
--
--   * mrp_runs           — the run header: the planning horizon, the
--                          parameters the run was computed with, the
--                          run status, and a denormalized summary of
--                          how many planned orders it produced.
--
--   * mrp_demand_lines   — the independent (top-level) demand the run
--                          was computed against, snapshotted at run
--                          time so the run stays reproducible even if
--                          the originating sales/work order changes.
--
--   * mrp_planned_orders — the netted output: one planned make or buy
--                          order per item with a net requirement,
--                          carrying the suggested start / due dates and
--                          the BOM-explosion level it was derived at so
--                          a planner can release lower-level orders
--                          first.
--
-- Every tenant-scoped table follows the canonical pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to
-- kapp_app. The migration-rls-check CI gate enforces the RLS
-- requirement. Reserved migration number for this work is 000092.

-- ---------------------------------------------------------------------------
-- MRP runs
-- ---------------------------------------------------------------------------
-- One row per executed MRP run. horizon_start / horizon_end bound the
-- planning window the run considered; demand due dates outside the
-- window are ignored. include_min_stock records whether the run topped
-- up items below their inventory reorder_level in addition to the
-- explicit demand lines. The *_count columns are a denormalized summary
-- of the planned-order output so a listing of runs needs no aggregate
-- join. buy_lead_time_days is the fixed purchasing lead time the run
-- used when backward-scheduling buy planned orders.
CREATE TABLE IF NOT EXISTS mrp_runs (
    tenant_id           UUID    NOT NULL REFERENCES tenants(id),
    id                  UUID    NOT NULL,
    status              TEXT    NOT NULL DEFAULT 'completed'
                        CHECK (status IN ('completed', 'failed')),
    horizon_start       DATE    NOT NULL,
    horizon_end         DATE    NOT NULL,
    include_min_stock   BOOLEAN NOT NULL DEFAULT false,
    buy_lead_time_days  INTEGER NOT NULL DEFAULT 7
                        CHECK (buy_lead_time_days >= 0),
    demand_line_count   INTEGER NOT NULL DEFAULT 0 CHECK (demand_line_count >= 0),
    planned_order_count INTEGER NOT NULL DEFAULT 0 CHECK (planned_order_count >= 0),
    make_order_count    INTEGER NOT NULL DEFAULT 0 CHECK (make_order_count >= 0),
    buy_order_count     INTEGER NOT NULL DEFAULT 0 CHECK (buy_order_count >= 0),
    notes               TEXT,
    created_by          UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT mrp_runs_horizon_order CHECK (horizon_end >= horizon_start)
);

CREATE INDEX IF NOT EXISTS mrp_runs_created_at_idx
    ON mrp_runs (tenant_id, created_at DESC);

ALTER TABLE mrp_runs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mrp_runs;
CREATE POLICY tenant_isolation ON mrp_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON mrp_runs TO kapp_app;

-- ---------------------------------------------------------------------------
-- MRP demand lines
-- ---------------------------------------------------------------------------
-- The independent demand snapshot a run was computed against. source
-- records where the line came from ('sales_order', 'work_order',
-- 'min_stock', or 'manual'); source_ref is an opaque human-readable
-- handle (e.g. the SO number or item SKU) carried through for the audit
-- trail. due_date is when the quantity is required; the planner backward
-- schedules from it. Composite FK to mrp_runs cascades a run delete to
-- its demand lines.
CREATE TABLE IF NOT EXISTS mrp_demand_lines (
    tenant_id   UUID    NOT NULL REFERENCES tenants(id),
    id          UUID    NOT NULL,
    run_id      UUID    NOT NULL,
    item_id     UUID    NOT NULL,
    qty         NUMERIC(20, 6) NOT NULL CHECK (qty > 0),
    due_date    DATE    NOT NULL,
    source      TEXT    NOT NULL DEFAULT 'manual'
                CHECK (source IN ('sales_order', 'work_order', 'min_stock', 'manual')),
    source_ref  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES mrp_runs (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS mrp_demand_lines_run_idx
    ON mrp_demand_lines (tenant_id, run_id);

ALTER TABLE mrp_demand_lines ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mrp_demand_lines;
CREATE POLICY tenant_isolation ON mrp_demand_lines
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON mrp_demand_lines TO kapp_app;

-- ---------------------------------------------------------------------------
-- MRP planned orders
-- ---------------------------------------------------------------------------
-- The netted output of a run: one row per item with a positive net
-- requirement. order_type is 'make' when the item has an active BOM (it
-- is produced in-house and was exploded into component demand) or 'buy'
-- otherwise (it is purchased). qty is the net requirement after
-- subtracting on-hand and scheduled receipts. due_date is the date the
-- quantity is required; suggested_start_date is due_date minus the
-- item's lead time (backward scheduling). bom_id / routing_id snapshot
-- the active recipe/routing used to derive a make order (NULL for buy).
-- explosion_level is the BOM depth the order was derived at (0 = the
-- top-level finished good in the demand lines); a planner releases
-- higher levels first so lower-level components are available in time.
CREATE TABLE IF NOT EXISTS mrp_planned_orders (
    tenant_id           UUID    NOT NULL REFERENCES tenants(id),
    id                  UUID    NOT NULL,
    run_id              UUID    NOT NULL,
    item_id             UUID    NOT NULL,
    order_type          TEXT    NOT NULL
                        CHECK (order_type IN ('make', 'buy')),
    qty                 NUMERIC(20, 6) NOT NULL CHECK (qty > 0),
    due_date            DATE    NOT NULL,
    suggested_start_date DATE   NOT NULL,
    explosion_level     INTEGER NOT NULL DEFAULT 0 CHECK (explosion_level >= 0),
    bom_id              UUID,
    routing_id          UUID,
    lead_time_days      INTEGER NOT NULL DEFAULT 0 CHECK (lead_time_days >= 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES mrp_runs (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id),
    FOREIGN KEY (tenant_id, bom_id) REFERENCES boms (tenant_id, id),
    FOREIGN KEY (tenant_id, routing_id) REFERENCES routings (tenant_id, id),
    CONSTRAINT mrp_planned_orders_start_before_due CHECK (suggested_start_date <= due_date)
);

CREATE INDEX IF NOT EXISTS mrp_planned_orders_run_idx
    ON mrp_planned_orders (tenant_id, run_id, explosion_level);

ALTER TABLE mrp_planned_orders ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON mrp_planned_orders;
CREATE POLICY tenant_isolation ON mrp_planned_orders
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON mrp_planned_orders TO kapp_app;
