-- Payroll depth — P1 (foundational). Promotes the payroll *run* artifact
-- from the declarative hr.pay_run KType (JSONB in krecords) to a typed,
-- high-integrity SQL table. The master-data KTypes (hr.salary_component,
-- hr.salary_structure) stay declarative; only the run/slip/line artifacts
-- — the financial records an auditor must trust — become typed tables.
--
--   * payroll_runs — the run header: the pay period, frequency, currency,
--                    run lifecycle status, the posted journal-entry link,
--                    and a denormalized rollup of the slips it produced so
--                    a listing of runs needs no aggregate join.
--
-- The ordered calculation pipeline (see internal/hr/payroll_pipeline.go)
-- writes payroll_payslips / payroll_payslip_lines (000099) against this
-- header, reading variable inputs from payroll_pay_inputs (000100) and
-- the engine-owned year-to-date accumulator payroll_ytd (000101).
--
-- Every tenant-scoped table follows the canonical pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to kapp_app.
-- The migration-rls-check CI gate enforces the RLS requirement. Reserved
-- migration number for this work is 000098.

-- ---------------------------------------------------------------------------
-- Payroll runs
-- ---------------------------------------------------------------------------
-- One row per pay run. id mirrors the hr.pay_run KRecord id so the typed
-- run and its backward-compatible KType shim share one identity (the
-- engine reuses the KRecord uuid as the typed PK). period_start/period_end
-- bound the pay period; freq records the cadence. run_type defaults to
-- 'regular'; the 'correction' / 'off_cycle' / 'bonus' values are reserved
-- for later batches and constrained now so they need no migration. status
-- tracks the run lifecycle. posted_je_id links the balanced journal entry
-- PostPayRun produces (NULL until the run is posted) — a composite FK to
-- journal_entries that, under MATCH SIMPLE, is unenforced while NULL. The
-- total_* columns are a denormalized rollup of the run's payslips.
-- deduction_account_map maps a statutory deduction code to a dedicated
-- liability account so PostPayRun can split withholdings by remittance
-- authority instead of the catch-all salary_payable line.
CREATE TABLE IF NOT EXISTS payroll_runs (
    tenant_id                   UUID NOT NULL REFERENCES tenants(id),
    id                          UUID NOT NULL,
    name                        TEXT NOT NULL,
    period_start                DATE NOT NULL,
    period_end                  DATE NOT NULL,
    freq                        TEXT NOT NULL DEFAULT 'monthly'
                                CHECK (freq IN ('monthly', 'semimonthly', 'biweekly', 'weekly')),
    currency                    TEXT NOT NULL DEFAULT 'USD'
                                CHECK (currency ~ '^[A-Z]{3}$'),
    status                      TEXT NOT NULL DEFAULT 'draft'
                                CHECK (status IN ('draft', 'processing', 'approved', 'paid')),
    run_type                    TEXT NOT NULL DEFAULT 'regular'
                                CHECK (run_type IN ('regular', 'off_cycle', 'correction', 'bonus')),
    department                  TEXT,
    posted_je_id                UUID,
    payslip_count               INTEGER NOT NULL DEFAULT 0 CHECK (payslip_count >= 0),
    total_gross                 NUMERIC(20, 6) NOT NULL DEFAULT 0,
    total_taxable               NUMERIC(20, 6) NOT NULL DEFAULT 0,
    total_ee_deductions         NUMERIC(20, 6) NOT NULL DEFAULT 0,
    total_net                   NUMERIC(20, 6) NOT NULL DEFAULT 0,
    salary_expense_account_code TEXT,
    salary_payable_account_code TEXT,
    deduction_account_map       JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by                  UUID,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, posted_je_id) REFERENCES journal_entries (tenant_id, id),
    CONSTRAINT payroll_runs_period_order CHECK (period_end >= period_start)
);

CREATE INDEX IF NOT EXISTS payroll_runs_period_idx
    ON payroll_runs (tenant_id, period_start DESC);

ALTER TABLE payroll_runs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payroll_runs;
CREATE POLICY tenant_isolation ON payroll_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_runs TO kapp_app;
