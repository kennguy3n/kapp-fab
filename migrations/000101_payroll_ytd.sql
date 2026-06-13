-- Payroll depth — P1 (foundational). The engine-owned year-to-date
-- accumulator. This is the D3 deliverable: nothing in the codebase owned or
-- persisted YTD before — the tax packs' EmployeeInfo.YTDGross was fed from
-- a static employee field. payroll_ytd makes the engine the system of
-- record for cumulative gross/tax so cumulative-withholding packs and
-- mid-year joiners compute correctly across consecutive runs.
--
--   * payroll_ytd — cumulative gross/tax per (employee, tax_year). The
--                   pipeline READS this row to feed a real, persisted
--                   YTDGross into pack.ComputeWithholding, and WRITES it
--                   transactionally in the same DB transaction as the slip
--                   and its lines (see FinalizePayslip). Re-running a draft
--                   reverses the slip's prior contribution before adding
--                   the new one, so YTD is never double-counted.
--
-- cumulative_gross tracks cumulative *taxable* gross (the withholding base
-- after pre-tax deductions) because that is exactly what a cumulative
-- withholding computation consumes as prior-period income; cumulative_tax
-- tracks tax withheld to date. per_code_base is a JSONB map of
-- cumulative per-code bases (for future per-component YTD reporting and
-- cap-based statutory ceilings in later batches).
--
-- This table uses a NATURAL / COMPOSITE primary key
-- (tenant_id, employee_id, tax_year) rather than the usual
-- (tenant_id, id) — there is exactly one accumulator row per employee per
-- tax year. Because the PK is not the default (tenant_id, id), the backup
-- service MUST register a tableConflictKeys entry for payroll_ytd
-- (services/kapp-backup/main.go) so the upsert-on-restore uses the natural
-- key. Follows the canonical tenant-scoped pattern otherwise: ENABLE ROW
-- LEVEL SECURITY, a tenant_isolation policy keyed off app.tenant_id, and
-- GRANT to kapp_app. Reserved migration number for this work is 000101.

-- ---------------------------------------------------------------------------
-- Payroll year-to-date accumulator
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS payroll_ytd (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    employee_id      UUID NOT NULL,
    tax_year         INTEGER NOT NULL CHECK (tax_year >= 1900 AND tax_year <= 9999),
    cumulative_gross NUMERIC(20, 6) NOT NULL DEFAULT 0,
    cumulative_tax   NUMERIC(20, 6) NOT NULL DEFAULT 0,
    per_code_base    JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, employee_id, tax_year)
);

ALTER TABLE payroll_ytd ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payroll_ytd;
CREATE POLICY tenant_isolation ON payroll_ytd
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_ytd TO kapp_app;
