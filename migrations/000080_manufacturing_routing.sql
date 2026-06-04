-- Stream 2 — Manufacturing Depth: Routing, Capacity Planning, Shop Floor.
--
-- Phase N6 shipped "light" manufacturing (boms + work_orders, see
-- 000063_manufacturing.sql). This migration adds the routing /
-- capacity / shop-floor layer that closes the remaining gap vs
-- ERPNext's Manufacturing module:
--
--   * work_centers      — A machine or workstation with a finite
--                         hourly capacity. The capacity-planning
--                         engine (internal/manufacturing/capacity.go)
--                         sums the operation load scheduled against
--                         each work center per day and flags the days
--                         where demand exceeds available minutes.
--
--   * routings          — A versioned, ordered sequence of operations
--                         for producing an item. Mirrors the BOM
--                         lifecycle (draft → active → obsolete); only
--                         one routing per item may be active at a time
--                         (routings_active_per_item_uniq). A work
--                         order snapshots the active routing at release
--                         time (work_orders.routing_id) so the shop-
--                         floor job cards stay reproducible even after
--                         a new routing version is activated.
--
--   * routing_operations — One step on a routing. setup_time_minutes is
--                         a fixed per-run cost; cycle_time_minutes is
--                         per produced unit. The capacity engine
--                         computes load as setup + cycle * planned_qty.
--
--   * job_cards         — Shop-floor execution records, one per routing
--                         operation per work order. Created
--                         automatically when a work order is released
--                         (see internal/manufacturing/job_card.go).
--                         Workers start / complete each card; completing
--                         the last open card triggers the existing
--                         CompleteWorkOrder inventory-move flow.
--
-- Every tenant-scoped table follows the canonical pattern: composite
-- (tenant_id, ...) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to
-- kapp_app. The migration-rls-check CI gate enforces the RLS
-- requirement.

-- ---------------------------------------------------------------------------
-- Work centers
-- ---------------------------------------------------------------------------
-- A machine or workstation. capacity_per_hour is the throughput in
-- output units per hour at 100% efficiency; operating_hours_per_day is
-- the shift length; efficiency_percent derates the nominal capacity for
-- real-world losses (changeover, micro-stops, operator pace). Available
-- minutes/day = operating_hours_per_day * 60 * efficiency_percent / 100.
CREATE TABLE IF NOT EXISTS work_centers (
    tenant_id               UUID    NOT NULL REFERENCES tenants(id),
    id                      UUID    NOT NULL,
    name                    TEXT    NOT NULL,
    capacity_per_hour       NUMERIC(20, 6) NOT NULL DEFAULT 0
                            CHECK (capacity_per_hour >= 0),
    operating_hours_per_day NUMERIC(6, 2)  NOT NULL DEFAULT 8
                            CHECK (operating_hours_per_day > 0 AND operating_hours_per_day <= 24),
    efficiency_percent      NUMERIC(6, 2)  NOT NULL DEFAULT 100
                            CHECK (efficiency_percent > 0 AND efficiency_percent <= 1000),
    status                  TEXT    NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'maintenance', 'retired')),
    notes                   TEXT,
    created_by              UUID,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    -- Names are the human handle the capacity grid and job cards
    -- render; a duplicate within a tenant would make the schedule
    -- ambiguous, so the name is unique per tenant.
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS work_centers_status_idx
    ON work_centers (tenant_id, status);

ALTER TABLE work_centers ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON work_centers;
CREATE POLICY tenant_isolation ON work_centers
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON work_centers TO kapp_app;

-- ---------------------------------------------------------------------------
-- Routings
-- ---------------------------------------------------------------------------
-- A versioned ordered sequence of operations for producing an item.
-- status drives a draft/active/obsolete lifecycle identical to boms;
-- only the active routing for a given item_id is snapshotted onto a
-- work order at release time. The partial unique index enforces the
-- single-active-row invariant the same way boms_active_per_item_uniq
-- does for BOMs.
CREATE TABLE IF NOT EXISTS routings (
    tenant_id     UUID    NOT NULL REFERENCES tenants(id),
    id            UUID    NOT NULL,
    item_id       UUID    NOT NULL,
    version       TEXT    NOT NULL,
    status        TEXT    NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'active', 'obsolete')),
    notes         TEXT,
    created_by    UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, item_id, version),
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id)
);

CREATE UNIQUE INDEX IF NOT EXISTS routings_active_per_item_uniq
    ON routings (tenant_id, item_id)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS routings_status_idx
    ON routings (tenant_id, status);

ALTER TABLE routings ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON routings;
CREATE POLICY tenant_isolation ON routings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON routings TO kapp_app;

-- ---------------------------------------------------------------------------
-- Routing operations
-- ---------------------------------------------------------------------------
-- One step on a routing, ordered by `sequence` (1-based, assigned from
-- the array position by the Go layer). setup_time_minutes is the fixed
-- per-run cost (incurred once regardless of batch size); cycle_time_minutes
-- is per produced unit. The capacity engine computes the load a work
-- order places on a work center as setup + cycle * planned_qty.
--
-- Composite FK (tenant_id, routing_id) cascades a routing delete to its
-- operations; (tenant_id, work_center_id) requires the work center to
-- live in the same tenant.
CREATE TABLE IF NOT EXISTS routing_operations (
    tenant_id          UUID    NOT NULL REFERENCES tenants(id),
    routing_id         UUID    NOT NULL,
    sequence           INTEGER NOT NULL CHECK (sequence > 0),
    operation_name     TEXT    NOT NULL,
    work_center_id     UUID    NOT NULL,
    setup_time_minutes NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (setup_time_minutes >= 0),
    cycle_time_minutes NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (cycle_time_minutes >= 0),
    description        TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, routing_id, sequence),
    FOREIGN KEY (tenant_id, routing_id) REFERENCES routings (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, work_center_id) REFERENCES work_centers (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS routing_operations_work_center_idx
    ON routing_operations (tenant_id, work_center_id);

ALTER TABLE routing_operations ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON routing_operations;
CREATE POLICY tenant_isolation ON routing_operations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON routing_operations TO kapp_app;

-- ---------------------------------------------------------------------------
-- Work order routing snapshot
-- ---------------------------------------------------------------------------
-- A work order snapshots the active routing at release time, mirroring
-- the bom_id snapshot (000063). routing_id is NULL for work orders
-- against items that have no routing — the light manufacturing path
-- (BOM-only, no shop-floor control) keeps working unchanged.
ALTER TABLE work_orders
    ADD COLUMN IF NOT EXISTS routing_id UUID;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'work_orders_routing_fk'
    ) THEN
        ALTER TABLE work_orders
            ADD CONSTRAINT work_orders_routing_fk
            FOREIGN KEY (tenant_id, routing_id)
            REFERENCES routings (tenant_id, id);
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Job cards
-- ---------------------------------------------------------------------------
-- Shop-floor execution records, one per routing operation per work
-- order. status walks pending → in_progress → completed; the Go state
-- machine (internal/manufacturing/job_card.go) enforces the legal
-- transitions so the error surface is a typed sentinel rather than a
-- CHECK violation. routing_operation_seq references the snapshotted
-- routing's operation sequence (not an FK — the routing row may later
-- be obsoleted, but the job card must stay queryable).
--
-- qty_produced / qty_rejected capture the per-operation yield reported
-- by the operator. The UNIQUE (tenant_id, work_order_id,
-- routing_operation_seq) constraint makes job-card creation idempotent:
-- a retried release re-inserts the same (work_order, seq) as a no-op.
CREATE TABLE IF NOT EXISTS job_cards (
    tenant_id             UUID    NOT NULL REFERENCES tenants(id),
    id                    UUID    NOT NULL,
    work_order_id         UUID    NOT NULL,
    routing_operation_seq INTEGER NOT NULL CHECK (routing_operation_seq > 0),
    work_center_id        UUID    NOT NULL,
    status                TEXT    NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'in_progress', 'completed')),
    planned_start         TIMESTAMPTZ,
    planned_end           TIMESTAMPTZ,
    actual_start          TIMESTAMPTZ,
    actual_end            TIMESTAMPTZ,
    operator_id           UUID,
    qty_produced          NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (qty_produced >= 0),
    qty_rejected          NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (qty_rejected >= 0),
    notes                 TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, work_order_id, routing_operation_seq),
    FOREIGN KEY (tenant_id, work_order_id) REFERENCES work_orders (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, work_center_id) REFERENCES work_centers (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS job_cards_work_order_idx
    ON job_cards (tenant_id, work_order_id);

CREATE INDEX IF NOT EXISTS job_cards_status_idx
    ON job_cards (tenant_id, status);

CREATE INDEX IF NOT EXISTS job_cards_work_center_idx
    ON job_cards (tenant_id, work_center_id);

ALTER TABLE job_cards ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON job_cards;
CREATE POLICY tenant_isolation ON job_cards
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON job_cards TO kapp_app;
