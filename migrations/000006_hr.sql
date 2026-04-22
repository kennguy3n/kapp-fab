-- Phase E — HR leave-balance ledger.
--
-- Employees, leave requests, attendance, and expense claims are stored
-- as KRecords (schemas in internal/hr/hr.go; tables: `krecords`), so no
-- dedicated tables are needed for those. The one exception is leave
-- balances: to make balance projections cheap, auditable, and immune
-- to KRecord partial updates we keep them in a dedicated append-only
-- ledger mirroring the finance journal pattern.
--
-- `leave_ledger` is append-only. Accruals are positive deltas, taken
-- leave is negative. Current balance for an employee/leave_type pair
-- is SUM(delta) filtered by tenant. The `source_ktype`/`source_id`
-- columns link entries back to the driving KRecord (e.g. an approved
-- `hr.leave_request`) and a partial unique index prevents the same
-- leave_request being posted twice when the approval worker retries.

CREATE TABLE IF NOT EXISTS leave_ledger (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    employee_id     UUID NOT NULL,
    leave_type      TEXT NOT NULL,
    delta_days      NUMERIC(10,4) NOT NULL,
    effective_on    DATE NOT NULL,
    source_ktype    TEXT,
    source_id       UUID,
    memo            TEXT,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS leave_ledger_default PARTITION OF leave_ledger DEFAULT;

CREATE INDEX IF NOT EXISTS leave_ledger_tenant_employee_idx
    ON leave_ledger (tenant_id, employee_id, leave_type, effective_on);

-- Source-row idempotency: one entry per (source_ktype, source_id)
-- per tenant. Matches the finance `journal_entries_source_uniq` and
-- inventory `inventory_moves_source_uniq` patterns.
CREATE UNIQUE INDEX IF NOT EXISTS leave_ledger_source_uniq
    ON leave_ledger (tenant_id, source_ktype, source_id)
    WHERE source_id IS NOT NULL;

ALTER TABLE leave_ledger ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON leave_ledger;
CREATE POLICY tenant_isolation ON leave_ledger
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT ON leave_ledger TO kapp_app;
GRANT USAGE, SELECT ON SEQUENCE leave_ledger_id_seq TO kapp_app;

-- Balance projection view: SUM(delta_days) per employee/leave_type.
-- SECURITY INVOKER (default on Postgres 15+) means RLS on the base
-- table applies to the caller's tenant context.
CREATE OR REPLACE VIEW leave_balances AS
    SELECT tenant_id,
           employee_id,
           leave_type,
           SUM(delta_days) AS balance_days
      FROM leave_ledger
     GROUP BY tenant_id, employee_id, leave_type;

GRANT SELECT ON leave_balances TO kapp_app;
