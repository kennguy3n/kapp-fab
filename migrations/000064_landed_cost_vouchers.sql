-- Phase N9c — Landed Cost Vouchers.
--
-- A "landed cost" is the total cost of bringing purchased goods to
-- the receiving warehouse beyond the supplier's invoice amount —
-- freight, insurance, customs duty, import tax, handling, port
-- fees. Without a landed-cost flow, the moving-average cost basis
-- of inventory under-states the true acquisition cost: COGS at
-- subsequent sale is too low and gross margin is overstated. Every
-- mature ERP (ERPNext, Odoo, NetSuite, SAP, Dynamics) ships a
-- "landed cost voucher" surface that lets finance allocate one or
-- more additional cost lines across one or more received receipts
-- (AP bills), proportionally by qty / amount / weight, and re-bases
-- the affected inventory_moves so the moving-average cost reflects
-- the true landed cost.
--
-- Storage shape:
--
--   - `landed_cost_vouchers` — header. One row per voucher.
--     `status` follows draft → allocated → posted, where:
--       * draft     — voucher header + charges + targets exist
--                     but allocated_amount on each target is 0
--                     (or stale). The /allocate endpoint runs the
--                     math and persists shares.
--       * allocated — allocation math has been applied to targets
--                     and the user can review per-target shares.
--       * posted    — reversal + forward inventory_moves have been
--                     written + a JE has been booked. Posting is
--                     idempotent via the je_id pointer back to the
--                     posting JE; a retry returns the same JE.
--     `allocation_method` is one of `by_qty` / `by_amount` /
--     `by_weight`. `by_qty` is the conservative default (every
--     target line picks up the same per-unit landed share); the
--     other two surface in cases where freight is dominated by
--     bulk volume (`by_weight`) or invoice value
--     (`by_amount`, e.g. duty calculated as a % of declared
--     value).
--
--   - `landed_cost_charges` — N rows per voucher. Each row is one
--     incoming charge line (e.g. "Freight - DHL, $1,200" + GL
--     account 6210). The voucher's grand_total is the sum of all
--     charge.amount columns. account_code is the GL account that
--     gets credited at posting (because the cost is reclassified
--     from "Cost of Freight / Duty / Insurance" expense into
--     "Inventory" asset). If `account_code` is empty the platform
--     default (Stock Adjustment) is used.
--
--   - `landed_cost_targets` — N rows per voucher. Each row points
--     at exactly one already-existing receipt line (typically an
--     inventory_moves row sourced from `finance.ap_bill`) and
--     carries:
--       * the (item_id, warehouse_id, qty, unit_cost) snapshot
--         from the receipt so the voucher is self-contained even
--         if the source AP bill is later voided,
--       * the `amount` (qty × unit_cost) and a per-row `weight`
--         field — the three columns the allocator needs in order
--         to assign a proportional share of the charges,
--       * the `allocated_amount` column set by the allocation
--         step — this is what the posting step uses to compute
--         the per-unit cost adjustment for the reversal+repost
--         inventory_moves.
--
-- The migration is forward-only. The `finance.landed_cost_voucher`
-- KType + the HTTP CRUD surface + the agent tools / KChat command
-- / frontend page mirror the same conventions used by the Phase N5
-- budget surface (KType for the metadata-driven builder, typed
-- tables for the math, posting flow gated by the `posted` status).
--
-- Sibling concern: this PR intentionally does NOT add a `weight`
-- column to `inventory_items`. Most tenants don't track weight per
-- SKU; pushing weight onto the master table would force a non-NULL
-- default and a backfill cost across every existing tenant. Carrying
-- weight on the per-voucher target row lets early adopters use
-- by_weight allocation without imposing schema cost on tenants who
-- never use it; if weight becomes a first-class master-data concern
-- a follow-up phase can lift it onto `inventory_items` and the
-- voucher target can default from there.

CREATE TABLE IF NOT EXISTS landed_cost_vouchers (
    tenant_id          UUID NOT NULL,
    id                 UUID NOT NULL,
    voucher_number     TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'draft',
    allocation_method  TEXT NOT NULL DEFAULT 'by_qty',
    posted_at          TIMESTAMPTZ,
    je_id              UUID,
    created_by         UUID,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, voucher_number),
    CONSTRAINT landed_cost_vouchers_status_chk
        CHECK (status IN ('draft', 'allocated', 'posted')),
    CONSTRAINT landed_cost_vouchers_method_chk
        CHECK (allocation_method IN ('by_qty', 'by_amount', 'by_weight'))
);

CREATE INDEX IF NOT EXISTS landed_cost_vouchers_tenant_status_idx
    ON landed_cost_vouchers (tenant_id, status);

CREATE TABLE IF NOT EXISTS landed_cost_charges (
    tenant_id     UUID NOT NULL,
    id            UUID NOT NULL,
    voucher_id    UUID NOT NULL,
    description   TEXT NOT NULL,
    amount        NUMERIC(20,4) NOT NULL,
    account_code  TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, voucher_id)
        REFERENCES landed_cost_vouchers (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT landed_cost_charges_amount_chk CHECK (amount > 0)
);

CREATE INDEX IF NOT EXISTS landed_cost_charges_voucher_idx
    ON landed_cost_charges (tenant_id, voucher_id);

CREATE TABLE IF NOT EXISTS landed_cost_targets (
    tenant_id          UUID NOT NULL,
    id                 UUID NOT NULL,
    voucher_id         UUID NOT NULL,
    source_ktype       TEXT NOT NULL DEFAULT 'finance.ap_bill',
    source_id          UUID NOT NULL,
    item_id            UUID NOT NULL,
    warehouse_id       UUID NOT NULL,
    qty                NUMERIC(20,4) NOT NULL,
    unit_cost          NUMERIC(20,4) NOT NULL,
    amount             NUMERIC(20,4) NOT NULL,
    -- weight defaults to 0 to match the Go zero value. The
    -- allocator treats Weight=0 as "exclude this target from the
    -- by_weight share split"; with a DEFAULT of 1.0 a row created
    -- by ad-hoc SQL would silently participate in the share split
    -- with weight 1, while the Go-created row for the same shape
    -- would be excluded. Keeping the column NOT NULL preserves the
    -- "always set explicitly" contract on the Go path; the new
    -- default only fires for direct-SQL paths where the operator
    -- omitted the column on purpose.
    weight             NUMERIC(20,4) NOT NULL DEFAULT 0,
    allocated_amount   NUMERIC(20,4) NOT NULL DEFAULT 0,
    applied            BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, voucher_id)
        REFERENCES landed_cost_vouchers (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT landed_cost_targets_qty_chk CHECK (qty > 0),
    CONSTRAINT landed_cost_targets_unit_cost_chk CHECK (unit_cost >= 0),
    CONSTRAINT landed_cost_targets_weight_chk CHECK (weight >= 0),
    -- allocated_amount is the per-target share of the voucher's
    -- charges, written by the allocator (computeAllocation in
    -- internal/finance/landed_cost.go). The Go-side clampNonNegative
    -- guard already keeps every share >= 0, but the DB enforces the
    -- same invariant as defense-in-depth: a future bug that lets a
    -- negative share leak through the rounding logic would be caught
    -- here rather than silently writing a negative inventory re-base.
    CONSTRAINT landed_cost_targets_allocated_amount_chk CHECK (allocated_amount >= 0)
);

CREATE INDEX IF NOT EXISTS landed_cost_targets_voucher_idx
    ON landed_cost_targets (tenant_id, voucher_id);

CREATE INDEX IF NOT EXISTS landed_cost_targets_source_idx
    ON landed_cost_targets (tenant_id, source_ktype, source_id);

ALTER TABLE landed_cost_vouchers ENABLE ROW LEVEL SECURITY;
ALTER TABLE landed_cost_charges  ENABLE ROW LEVEL SECURITY;
ALTER TABLE landed_cost_targets  ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS landed_cost_vouchers_isolation ON landed_cost_vouchers;
CREATE POLICY landed_cost_vouchers_isolation ON landed_cost_vouchers
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS landed_cost_charges_isolation ON landed_cost_charges;
CREATE POLICY landed_cost_charges_isolation ON landed_cost_charges
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS landed_cost_targets_isolation ON landed_cost_targets;
CREATE POLICY landed_cost_targets_isolation ON landed_cost_targets
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON landed_cost_vouchers TO kapp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON landed_cost_charges  TO kapp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON landed_cost_targets  TO kapp_app;

COMMENT ON TABLE landed_cost_vouchers IS
    'Phase N9c — landed cost voucher header. One per allocation event; status walks draft → allocated → posted, and posting writes reversal + forward inventory_moves so the moving-average cost basis of the receipt picks up the allocated freight / duty / insurance.';
COMMENT ON TABLE landed_cost_charges IS
    'One row per incoming charge line (freight, duty, insurance). amount is positive; sum across all charges is the voucher grand total that is then divided across targets by the allocation method.';
COMMENT ON TABLE landed_cost_targets IS
    'One row per receipt line being adjusted. (qty, unit_cost) snapshot the original inventory_moves row so the voucher is self-contained even after the source AP bill is voided; allocated_amount is filled in by the allocator and consumed by the poster.';

COMMENT ON COLUMN landed_cost_vouchers.status IS
    'draft = header + charges + targets exist, allocation not yet computed. allocated = per-target allocated_amount populated. posted = reversal + forward inventory_moves emitted, je_id points at the booking JE.';
COMMENT ON COLUMN landed_cost_vouchers.allocation_method IS
    'by_qty (default) = even per-unit share. by_amount = proportional to (qty × unit_cost). by_weight = proportional to per-target weight column.';
COMMENT ON COLUMN landed_cost_vouchers.je_id IS
    'When the voucher posts, the platform emits one JE crediting the charge clearing accounts and debiting Inventory. je_id pins that JE so a retry is idempotent (the same JE is returned rather than a second one written).';
COMMENT ON COLUMN landed_cost_targets.applied IS
    'True once the poster has written the reversal + forward inventory_moves for this target. The composite (voucher.status="posted" AND target.applied=true) is the steady-state invariant; intermediate states are visible to the operator if a posting attempt failed partway through.';
