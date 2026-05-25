-- Phase N5 — Budget module.
--
-- Adds the typed `budgets` (header) and `budget_lines` (per-account,
-- per-cost-centre × monthly grid) tables that back the new finance
-- budget surface. Mirrors the same conventions every other tenant-
-- scoped table in the platform uses:
--
--   * Composite PK (tenant_id, id) so RLS predicates fall on the
--     tenant_id column index and partition-pruning works once we
--     pivot to LIST-partition by tenant_id.
--   * RLS enabled with the tenant_isolation policy that reads the
--     `app.tenant_id` GUC — identical to bank_accounts /
--     cost_centers (migrations/000011) and the rest of the platform.
--   * GRANT SELECT/INSERT/UPDATE/DELETE to the kapp_app role; no
--     direct access from kapp_admin needed at the data plane.
--
-- The KRecord mirror (finance.budget KType in internal/finance/budget.go)
-- exposes the same shape over the metadata-driven UI / KChat / agent
-- surface; the typed tables here are the source of truth for the
-- variance reports that JOIN budget_lines against journal_lines.

-- ---------------------------------------------------------------------------
-- Budgets — header table.
-- ---------------------------------------------------------------------------
-- One row per (tenant, budget). A tenant can run several budgets
-- side-by-side (e.g. one consolidated FY2025 budget plus one
-- per-cost-centre budget for a specific BU). The `cost_center`
-- column on the header is the **default scope** for unfilled
-- budget_lines.cost_center values; lines may still carry their own
-- non-NULL cost_center to override the default for a specific
-- account.

CREATE TABLE IF NOT EXISTS budgets (
    tenant_id     UUID NOT NULL,
    id            UUID NOT NULL,
    name          TEXT NOT NULL,
    fiscal_year   INT  NOT NULL,
    -- draft → active → closed is the lifecycle. Only `active`
    -- budgets are considered by the variance alerter; `closed`
    -- budgets are retained for historical reporting but never
    -- raise alerts.
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'active', 'closed')),
    -- Optional default scope (NULL = enterprise-wide).
    cost_center   TEXT,
    notes         TEXT,
    -- Variance threshold for alerts, expressed as a fraction
    -- (0.10 = 10%). NULL → fall back to the platform default
    -- in the variance-alert handler.
    variance_threshold NUMERIC(6,4),
    created_by    UUID,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- Lookup by status (the alerter only walks `active`).
CREATE INDEX IF NOT EXISTS budgets_tenant_status_idx
    ON budgets (tenant_id, status);

-- Lookup by fiscal_year (the variance dashboard pivots on it).
CREATE INDEX IF NOT EXISTS budgets_tenant_fiscal_year_idx
    ON budgets (tenant_id, fiscal_year);

ALTER TABLE budgets ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON budgets;
CREATE POLICY tenant_isolation ON budgets
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON budgets TO kapp_app;

-- ---------------------------------------------------------------------------
-- Budget lines — per-account × per-cost-centre × monthly grid.
-- ---------------------------------------------------------------------------
-- One row per (budget, account_code, cost_center) tuple. The 12
-- amount_<month> columns hold the planned figures for January
-- through December of the budget's fiscal_year; annual_total is a
-- STORED generated column so it never drifts from the monthly sum
-- and the variance report can pull it without a CASE expression.
--
-- Rationale for monthly columns vs. tall-skinny (one row per month):
-- the canonical UI is a wide spreadsheet that edits all 12 months
-- in a single PUT and the variance report frequently asks "give me
-- this account's plan for Q2" — both shapes are cheaper against
-- 12 columns than against 12 rows. The cost is paid once: rare
-- non-calendar fiscal years still fill columns 1..12 in fiscal-
-- month order (i.e. amount_jan corresponds to the FIRST fiscal
-- month, even if that's April for an Indian FY).

CREATE TABLE IF NOT EXISTS budget_lines (
    tenant_id    UUID NOT NULL,
    id           UUID NOT NULL,
    budget_id    UUID NOT NULL,
    account_code TEXT NOT NULL,
    -- Per-line cost_center may override the header default. NULL
    -- here means "use the budget header's cost_center" at report
    -- time (resolved in the variance computation, not stored).
    cost_center  TEXT,
    -- Monthly planned amounts in the tenant's base currency. The
    -- variance computation compares these against the sum of
    -- journal_lines.base_amount falling in the same fiscal month.
    amount_jan NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_feb NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_mar NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_apr NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_may NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_jun NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_jul NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_aug NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_sep NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_oct NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_nov NUMERIC(20,4) NOT NULL DEFAULT 0,
    amount_dec NUMERIC(20,4) NOT NULL DEFAULT 0,
    annual_total NUMERIC(20,4) GENERATED ALWAYS AS (
        amount_jan + amount_feb + amount_mar + amount_apr +
        amount_may + amount_jun + amount_jul + amount_aug +
        amount_sep + amount_oct + amount_nov + amount_dec
    ) STORED,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, budget_id) REFERENCES budgets (tenant_id, id) ON DELETE CASCADE
);

-- Enforce one line per (budget, account_code, cost_center). NULL
-- cost_center compares equal to NULL under UNIQUE — Postgres
-- treats NULLs as distinct by default, but we want exactly one
-- "default" line per (budget, account), so the unique key uses
-- COALESCE to normalise the NULL.
CREATE UNIQUE INDEX IF NOT EXISTS budget_lines_account_cc_uniq
    ON budget_lines (tenant_id, budget_id, account_code, COALESCE(cost_center, ''));

-- Lookup by (budget) for the line editor and the variance report.
CREATE INDEX IF NOT EXISTS budget_lines_tenant_budget_idx
    ON budget_lines (tenant_id, budget_id);

-- Lookup by (account_code) for cross-budget account drill-downs
-- (e.g. "show me every budget that allocates to account 5000").
CREATE INDEX IF NOT EXISTS budget_lines_tenant_account_idx
    ON budget_lines (tenant_id, account_code);

ALTER TABLE budget_lines ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON budget_lines;
CREATE POLICY tenant_isolation ON budget_lines
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON budget_lines TO kapp_app;

-- Keep updated_at fresh on every UPDATE so the API can return
-- monotonically increasing timestamps for optimistic-concurrency
-- clients without the handler having to remember to bump it.
CREATE OR REPLACE FUNCTION budgets_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS budgets_set_updated_at ON budgets;
CREATE TRIGGER budgets_set_updated_at
    BEFORE UPDATE ON budgets
    FOR EACH ROW EXECUTE FUNCTION budgets_set_updated_at();

DROP TRIGGER IF EXISTS budget_lines_set_updated_at ON budget_lines;
CREATE TRIGGER budget_lines_set_updated_at
    BEFORE UPDATE ON budget_lines
    FOR EACH ROW EXECUTE FUNCTION budgets_set_updated_at();
