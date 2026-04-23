-- Phase G follow-up: sales/procurement, bank reconciliation, cost centers,
-- and delta-import bookkeeping. Everything here is tenant-scoped with RLS
-- and uses the same (tenant_id, id) PK convention as the existing tables.

-- ---------------------------------------------------------------------------
-- Bank accounts + transactions
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS bank_accounts (
    tenant_id      UUID NOT NULL,
    id             UUID NOT NULL,
    name           TEXT NOT NULL,
    account_code   TEXT NOT NULL,
    bank_name      TEXT,
    account_number TEXT,
    iban           TEXT,
    currency       TEXT NOT NULL,
    active         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS bank_accounts_tenant_active_idx
    ON bank_accounts (tenant_id, active);

ALTER TABLE bank_accounts ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_accounts;
CREATE POLICY tenant_isolation ON bank_accounts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_accounts TO kapp_app;

CREATE TABLE IF NOT EXISTS bank_transactions (
    tenant_id        UUID NOT NULL,
    id               UUID NOT NULL,
    bank_account_id  UUID NOT NULL,
    value_date       DATE NOT NULL,
    description      TEXT,
    amount           NUMERIC(20,4) NOT NULL,
    currency         TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'unreconciled'
        CHECK (status IN ('unreconciled', 'matched', 'ignored')),
    matched_entry_id UUID,
    external_ref     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, bank_account_id) REFERENCES bank_accounts (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS bank_transactions_tenant_value_date_idx
    ON bank_transactions (tenant_id, bank_account_id, value_date);
CREATE INDEX IF NOT EXISTS bank_transactions_tenant_status_idx
    ON bank_transactions (tenant_id, status) WHERE status = 'unreconciled';

ALTER TABLE bank_transactions ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_transactions;
CREATE POLICY tenant_isolation ON bank_transactions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_transactions TO kapp_app;

-- ---------------------------------------------------------------------------
-- Cost centers + journal-line dimension
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS cost_centers (
    tenant_id   UUID NOT NULL,
    code        TEXT NOT NULL,
    name        TEXT NOT NULL,
    parent_code TEXT,
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, code)
);

ALTER TABLE cost_centers ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON cost_centers;
CREATE POLICY tenant_isolation ON cost_centers
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON cost_centers TO kapp_app;

-- Optional per-line dimension. Trial-balance + income-statement
-- reports filter on this column when the caller passes a cost_center
-- filter; postings that don't care about CC leave it NULL.
ALTER TABLE journal_lines
    ADD COLUMN IF NOT EXISTS cost_center TEXT;
CREATE INDEX IF NOT EXISTS journal_lines_tenant_cost_center_idx
    ON journal_lines (tenant_id, cost_center)
    WHERE cost_center IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Importer: delta sync bookkeeping
-- ---------------------------------------------------------------------------

ALTER TABLE import_jobs
    ADD COLUMN IF NOT EXISTS last_sync_at TIMESTAMPTZ;
