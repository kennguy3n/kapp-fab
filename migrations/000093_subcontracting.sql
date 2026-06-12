-- Batch-2 manufacturing depth (Workstream — subcontracting). Models a
-- subcontracted operation: components are issued to an external
-- supplier, the supplier performs an operation (assembly, plating,
-- machining, …), and the finished or sub-assembled item is received
-- back. This closes the remaining make-vs-buy gap vs ERPNext's
-- subcontracting flow on top of the make-to-stock loop (000063) and the
-- routing / shop-floor layer (000080).
--
--   * subcontract_orders     — the header. Optionally ties to an
--                              existing work order + the routing
--                              operation that is performed off-site, so
--                              subcontracting plugs into the routing
--                              model rather than forking a parallel one.
--                              status walks draft → issued → received →
--                              closed, with a cancelled terminal state
--                              reachable only from draft (once issued the
--                              components have moved, so the order must be
--                              received rather than cancelled — enforced by
--                              the Go state machine, not this CHECK).
--                              item_id is the
--                              finished item received back; supplier_id
--                              is an opaque ref to the crm.organization
--                              KRecord (no FK — suppliers are records,
--                              not a typed table).
--
--   * subcontract_components — the components issued to the supplier for
--                              one order. One row per component item.
--                              issued_qty tracks how much has actually
--                              been issued so a re-issue is a no-op.
--
-- The issue and receipt both emit inventory_moves through the existing
-- inventory.RecordMove path (source_ktype
-- 'manufacturing.subcontract.issue' / '.receipt', source_id = the
-- order id) so they participate in the same outbox + audit pipeline and
-- inherit idempotency from inventory_moves_source_uniq — a retried
-- issue / receipt replays as a no-op.
--
-- Every tenant-scoped table follows the canonical pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to
-- kapp_app. The migration-rls-check CI gate enforces the RLS
-- requirement. Reserved migration number for this work is 000093.

-- ---------------------------------------------------------------------------
-- Subcontract orders
-- ---------------------------------------------------------------------------
-- One subcontracting job. work_order_id / routing_operation_seq are
-- nullable: a subcontract order may stand alone (a one-off out-sourced
-- job) or tie into an existing work order's routing operation. item_id
-- is the item received back from the supplier; qty is the expected
-- quantity, received_qty the running actual. charge_amount is the
-- supplier's service fee for the job, folded into the receipt move's
-- unit cost so the finished stock carries the subcontracting cost.
CREATE TABLE IF NOT EXISTS subcontract_orders (
    tenant_id             UUID    NOT NULL REFERENCES tenants(id),
    id                    UUID    NOT NULL,
    work_order_id         UUID,
    routing_operation_seq INTEGER CHECK (routing_operation_seq IS NULL OR routing_operation_seq > 0),
    supplier_id           UUID,
    item_id               UUID    NOT NULL,
    warehouse_id          UUID    NOT NULL,
    qty                   NUMERIC(20, 6) NOT NULL CHECK (qty > 0),
    received_qty          NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (received_qty >= 0),
    status                TEXT    NOT NULL DEFAULT 'draft'
                          CHECK (status IN ('draft', 'issued', 'received', 'closed', 'cancelled')),
    charge_amount         NUMERIC(20, 4) NOT NULL DEFAULT 0 CHECK (charge_amount >= 0),
    charge_currency       TEXT,
    issued_at             TIMESTAMPTZ,
    received_at           TIMESTAMPTZ,
    notes                 TEXT,
    created_by            UUID,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id),
    FOREIGN KEY (tenant_id, warehouse_id) REFERENCES inventory_warehouses (tenant_id, id),
    FOREIGN KEY (tenant_id, work_order_id) REFERENCES work_orders (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS subcontract_orders_status_idx
    ON subcontract_orders (tenant_id, status);

CREATE INDEX IF NOT EXISTS subcontract_orders_work_order_idx
    ON subcontract_orders (tenant_id, work_order_id)
    WHERE work_order_id IS NOT NULL;

ALTER TABLE subcontract_orders ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON subcontract_orders;
CREATE POLICY tenant_isolation ON subcontract_orders
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON subcontract_orders TO kapp_app;

-- ---------------------------------------------------------------------------
-- Subcontract components
-- ---------------------------------------------------------------------------
-- The components issued to the supplier for one order. qty is the
-- planned issue quantity; issued_qty is the running actual so a retried
-- issue is idempotent at the application layer (the inventory move is
-- additionally idempotent at the DB layer). The natural uniqueness of
-- one row per (order, component) is enforced by a unique index rather
-- than a composite PK so the table keeps the canonical (tenant_id, id)
-- shape and needs no tableConflictKeys entry in kapp-backup.
CREATE TABLE IF NOT EXISTS subcontract_components (
    tenant_id            UUID    NOT NULL REFERENCES tenants(id),
    id                   UUID    NOT NULL,
    subcontract_order_id UUID    NOT NULL,
    item_id              UUID    NOT NULL,
    qty                  NUMERIC(20, 6) NOT NULL CHECK (qty > 0),
    issued_qty           NUMERIC(20, 6) NOT NULL DEFAULT 0 CHECK (issued_qty >= 0),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, subcontract_order_id) REFERENCES subcontract_orders (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id)
);

CREATE UNIQUE INDEX IF NOT EXISTS subcontract_components_order_item_uniq
    ON subcontract_components (tenant_id, subcontract_order_id, item_id);

ALTER TABLE subcontract_components ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON subcontract_components;
CREATE POLICY tenant_isolation ON subcontract_components
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON subcontract_components TO kapp_app;
