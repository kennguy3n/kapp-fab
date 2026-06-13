-- Payroll depth — P1 (foundational). The per-employee payslip and its
-- ordered line items, promoted from the hr.payslip KType to typed tables.
-- Builds on payroll_runs (000098). See internal/hr/payroll_pipeline.go for
-- the ordered calculation pipeline that writes these rows.
--
--   * payroll_payslips      — one row per (run, employee). Carries the
--                             gross / taxable_gross / tax_total /
--                             total_ee_deductions / net rollup so a slip
--                             listing needs no aggregate join, plus the
--                             slip lifecycle status. A UNIQUE (tenant_id,
--                             run_id, employee_id) constraint makes slip
--                             generation idempotent at the database level:
--                             re-running a draft run UPSERTs the same row
--                             rather than producing a duplicate slip.
--
--   * payroll_payslip_lines — the self-explaining breakdown of a slip in
--                             pipeline order (seq). kind classifies each
--                             line: 'earning' → 'pretax_deduction' → 'tax'
--                             → 'posttax_deduction'. 'er_contribution' is
--                             reserved for P2 (employer contributions) and
--                             constrained now so P2 needs no migration; P1
--                             never emits it. taxable records whether the
--                             line contributes to taxable_gross.
--
-- Both tables follow the canonical tenant-scoped pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to kapp_app.
-- Reserved migration number for this work is 000099.

-- ---------------------------------------------------------------------------
-- Payroll payslips
-- ---------------------------------------------------------------------------
-- id mirrors the hr.payslip KRecord id so the typed slip and its
-- backward-compatible KType shim share one identity. taxable_gross is the
-- income-tax base after pre-tax deductions reduce it (computed BEFORE
-- withholding); contribution_gross is the social-security / contribution
-- base (the full gross unless a component is flagged to also reduce it),
-- persisted so a draft re-run can reverse the slip's prior contribution
-- cleanly when advancing payroll_ytd; tax_total is the sum of the 'tax'
-- lines; total_ee_deductions is pretax + tax + posttax; net = gross -
-- total_ee_deductions. The composite FK to payroll_runs cascades a run
-- delete to its slips.
CREATE TABLE IF NOT EXISTS payroll_payslips (
    tenant_id           UUID NOT NULL REFERENCES tenants(id),
    id                  UUID NOT NULL,
    run_id              UUID NOT NULL,
    employee_id         UUID NOT NULL,
    structure_id        UUID,
    currency            TEXT NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    period_start        DATE NOT NULL,
    period_end          DATE NOT NULL,
    gross               NUMERIC(20, 6) NOT NULL DEFAULT 0,
    taxable_gross       NUMERIC(20, 6) NOT NULL DEFAULT 0,
    contribution_gross  NUMERIC(20, 6) NOT NULL DEFAULT 0,
    tax_total           NUMERIC(20, 6) NOT NULL DEFAULT 0,
    total_ee_deductions NUMERIC(20, 6) NOT NULL DEFAULT 0,
    net                 NUMERIC(20, 6) NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft', 'approved', 'paid')),
    posted_je_id        UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, run_id) REFERENCES payroll_runs (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT payroll_payslips_run_employee_uniq UNIQUE (tenant_id, run_id, employee_id),
    CONSTRAINT payroll_payslips_period_order CHECK (period_end >= period_start)
);

CREATE INDEX IF NOT EXISTS payroll_payslips_run_idx
    ON payroll_payslips (tenant_id, run_id);

ALTER TABLE payroll_payslips ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payroll_payslips;
CREATE POLICY tenant_isolation ON payroll_payslips
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_payslips TO kapp_app;

-- ---------------------------------------------------------------------------
-- Payroll payslip lines
-- ---------------------------------------------------------------------------
-- One row per resolved slip line, ordered by seq in pipeline order so the
-- slip is reproducible and self-explaining. base/rate are informational
-- (e.g. base salary and proration factor for an earning, or taxable_gross
-- and the withholding rate for a tax line) and may be NULL; amount is the
-- resolved money figure. gl_account_code optionally pins the line to a
-- chart-of-accounts code for posting. The composite FK to payroll_payslips
-- cascades a slip delete (and thus a run delete) to its lines, which is how
-- re-generating a draft slip atomically replaces its lines.
CREATE TABLE IF NOT EXISTS payroll_payslip_lines (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    id              UUID NOT NULL,
    payslip_id      UUID NOT NULL,
    seq             INTEGER NOT NULL CHECK (seq >= 0),
    kind            TEXT NOT NULL
                    CHECK (kind IN ('earning', 'pretax_deduction', 'tax', 'posttax_deduction', 'er_contribution')),
    code            TEXT NOT NULL,
    label           TEXT NOT NULL DEFAULT '',
    base            NUMERIC(20, 6),
    rate            NUMERIC(20, 6),
    amount          NUMERIC(20, 6) NOT NULL,
    taxable         BOOLEAN NOT NULL DEFAULT false,
    gl_account_code TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, payslip_id) REFERENCES payroll_payslips (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT payroll_payslip_lines_seq_uniq UNIQUE (tenant_id, payslip_id, seq)
);

CREATE INDEX IF NOT EXISTS payroll_payslip_lines_payslip_idx
    ON payroll_payslip_lines (tenant_id, payslip_id, seq);

ALTER TABLE payroll_payslip_lines ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON payroll_payslip_lines;
CREATE POLICY tenant_isolation ON payroll_payslip_lines
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON payroll_payslip_lines TO kapp_app;
