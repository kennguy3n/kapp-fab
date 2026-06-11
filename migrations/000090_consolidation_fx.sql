-- Workstream 3 (step 1) — multi-entity consolidation + multi-currency FX.
--
-- Two changes, both additive and idempotent:
--
--   1. consolidation_groups.cta_account_code — the equity account the
--      consolidation parks Cumulative Translation Adjustment (CTA)
--      differences in. Under the IAS 21 / ASC 830 current-rate method
--      balance-sheet accounts translate at the closing rate and P&L
--      accounts at the average rate; the residual difference (plus any
--      intercompany-elimination FX mismatch) is booked to this equity
--      line so the consolidated balance sheet still balances. NULL
--      falls back to the package default (ledger.AccountCodeCTA, 3900).
--      consolidation_groups is operator-scoped (no tenant_id / no RLS),
--      so the new column inherits that posture.
--
--   2. fx_revaluation_runs — an audit trail for on-demand FX
--      revaluation runs triggered via the finance API. The scheduled
--      UnrealizedGainLossJob posts the same revaluation journal
--      entries but does not write here (its journal entries are the
--      durable record); the API runner persists a per-run envelope so
--      operators can review what each period-end revaluation did. The
--      table is tenant-scoped with RLS + the kapp_app grant, matching
--      exchange_rates (000017) and the rest of the finance schema.

-- ---------------------------------------------------------------------------
-- 1. Consolidation CTA account configuration
-- ---------------------------------------------------------------------------
ALTER TABLE consolidation_groups
    ADD COLUMN IF NOT EXISTS cta_account_code TEXT;

-- ---------------------------------------------------------------------------
-- 2. FX revaluation run audit trail (tenant-scoped, RLS)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS fx_revaluation_runs (
    tenant_id    UUID NOT NULL,
    id           UUID NOT NULL,
    -- Period-end the open foreign-currency balances were revalued at.
    as_of        TIMESTAMPTZ NOT NULL,
    -- Magnitudes (both non-negative); net = total_gain - total_loss.
    total_gain   NUMERIC(20,4) NOT NULL DEFAULT 0,
    total_loss   NUMERIC(20,4) NOT NULL DEFAULT 0,
    net          NUMERIC(20,4) NOT NULL DEFAULT 0,
    -- Full RevaluationResult envelope (per-account deltas + the journal
    -- entry ids the deltas were posted through) for drill-down.
    result       JSONB NOT NULL,
    created_by   UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- Newest-first per-tenant listing is the only read pattern.
CREATE INDEX IF NOT EXISTS fx_revaluation_runs_tenant_asof_idx
    ON fx_revaluation_runs (tenant_id, as_of DESC);

ALTER TABLE fx_revaluation_runs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON fx_revaluation_runs;
CREATE POLICY tenant_isolation ON fx_revaluation_runs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON fx_revaluation_runs TO kapp_app;
