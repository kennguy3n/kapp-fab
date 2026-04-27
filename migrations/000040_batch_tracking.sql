-- Phase G/L deferred — Batch / lot tracking on top of inventory_moves.
--
-- `inventory_batches` stores per-tenant lot identifiers (one row per
-- batch). `inventory_moves` gains an optional `batch_id` foreign key
-- so a stock receipt or delivery can be tied to a specific batch.
-- The internal/inventory.PGStore.RecordMove path validates the batch
-- belongs to the same tenant and the same item before allowing the
-- INSERT, so a tenant cannot accidentally (or maliciously) post a
-- move against another item's batch.
--
-- RLS follows the canonical multi-tenancy pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, tenant
-- isolation policy keyed off the app.tenant_id GUC, and GRANT to
-- kapp_app. The migration-rls-check CI gate enforces the RLS
-- requirement.
--
-- Reference: frappe/erpnext Batch Master + Stock Ledger Entry batch
-- linkage.

CREATE TABLE IF NOT EXISTS inventory_batches (
    tenant_id      UUID    NOT NULL REFERENCES tenants(id),
    id             UUID    NOT NULL,
    item_id        UUID    NOT NULL,
    batch_no       TEXT    NOT NULL,
    manufactured_at DATE,
    expires_at     DATE,
    qty_on_hand    NUMERIC(20, 6) NOT NULL DEFAULT 0,
    metadata       JSONB   NOT NULL DEFAULT '{}'::jsonb,
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, item_id, batch_no),
    FOREIGN KEY (tenant_id, item_id) REFERENCES inventory_items (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS inventory_batches_item_idx
    ON inventory_batches (tenant_id, item_id);

CREATE INDEX IF NOT EXISTS inventory_batches_expiry_idx
    ON inventory_batches (tenant_id, expires_at)
    WHERE expires_at IS NOT NULL;

ALTER TABLE inventory_batches ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON inventory_batches;
CREATE POLICY tenant_isolation ON inventory_batches
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON inventory_batches TO kapp_app;

-- inventory_moves picks up an optional batch_id column. The composite
-- foreign key ensures (tenant_id, batch_id) addresses one of *this
-- tenant's* batches; cross-tenant linkage is impossible by construction.
-- The column is NULLable because non-batch-tracked items continue to
-- post moves with batch_id IS NULL.
ALTER TABLE inventory_moves
    ADD COLUMN IF NOT EXISTS batch_id UUID;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_name = 'inventory_moves'
          AND constraint_name = 'inventory_moves_batch_fk'
    ) THEN
        ALTER TABLE inventory_moves
            ADD CONSTRAINT inventory_moves_batch_fk
            FOREIGN KEY (tenant_id, batch_id)
            REFERENCES inventory_batches (tenant_id, id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS inventory_moves_batch_idx
    ON inventory_moves (tenant_id, batch_id)
    WHERE batch_id IS NOT NULL;
