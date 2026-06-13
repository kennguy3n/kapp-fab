-- Payroll depth — P1 (foundational). Variable per-employee pay inputs for
-- a run: the figures that vary period to period and cannot be derived from
-- the standing salary structure alone. Builds on payroll_runs (000098).
--
--   * payroll_pay_inputs — one row per variable input for an (run,
--                          employee). type classifies the input:
--                          'hours' / 'overtime' / 'bonus' /
--                          'reimbursement' (additive earnings),
--                          'lop_days' (loss-of-pay unpaid days that reduce
--                          gross), and 'adjustment' (a signed catch-all).
--                          The ordered pipeline reads these in
--                          gatherPayInputs and folds them into the slip:
--                          earnings on top of the prorated structure
--                          earnings, lop_days docking gross by the daily
--                          rate. taxable records whether an additive input
--                          contributes to taxable_gross (reimbursements
--                          default to taxable=false at the call site).
--
-- Follows the canonical tenant-scoped pattern: composite (tenant_id, id)
-- primary key, ENABLE ROW LEVEL SECURITY, a tenant_isolation policy keyed
-- off app.tenant_id, and GRANT to kapp_app. Reserved migration number for
-- this work is 000100.

-- ---------------------------------------------------------------------------
-- Payroll pay inputs
-- ---------------------------------------------------------------------------
-- qty/rate/amount are kept independently so the slip can show the working
-- (e.g. overtime: qty=hours, rate=hourly, amount=qty*rate) while still
-- carrying a resolved amount the engine can sum directly; an input whose
-- amount is the source of truth (e.g. a flat bonus) sets amount and leaves
-- qty/rate zero. lop_days carries qty=unpaid days (amount derived from the
-- daily rate at calculation time). The composite FK to payroll_runs
-- cascades a run delete to its inputs.
CREATE TABLE IF NOT EXISTS payroll_pay_inputs (
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    id          UUID NOT NULL,
    run_id      UUID NOT NULL,
    employee_id UUID NOT NULL,
    type        TEXT NOT NULL
                CHECK (type IN ('hours', 'overtime', 'bonus', 'reimbursement', 'lop_days', 'adjustment')),
    code        TEXT NOT NULL DEFAULT '',
    label       TEXT NOT NULL DEFAULT '',
    qty         NUMERIC(20, 6) NOT NULL DEFAULT 0,
    rate        NUMERIC(20, 6) NOT NULL DEFAULT 0,
    amount      NUMERIC(20, 6) NOT NULL DEFAULT 0,
    taxable     BOOLEAN NOT NULL DEFAULT true,
    note        TEXT,
    created_by  UUID,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES payroll_runs (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS payroll_pay_inputs_run_employee_idx
    ON payroll_pay_inputs (tenant_id, run_id, employee_id);

ALTER TABLE payroll_pay_inputs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payroll_pay_inputs;
CREATE POLICY tenant_isolation ON payroll_pay_inputs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_pay_inputs TO kapp_app;
