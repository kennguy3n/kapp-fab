-- Phase J — tenant resource metering and plan definitions.
--
-- tenant_usage is an atomic counter table keyed by (tenant, period,
-- metric). Every request increment lands here via INSERT ... ON
-- CONFLICT DO UPDATE so no denormalised "current period" column
-- needs to be maintained in the tenants row. `period_start` is
-- truncated to month in application code (not enforced by a CHECK
-- constraint so the platform can experiment with weekly or daily
-- billing periods without a migration).
--
-- plan_definitions is a control-plane table (no RLS, no tenant_id)
-- that stores the pricing tiers the platform offers. The /plans
-- API reads it so the UI can render upgrade/downgrade prompts that
-- reflect the current tier structure without baking the pricing
-- into the web bundle.

CREATE TABLE IF NOT EXISTS tenant_usage (
    tenant_id     UUID NOT NULL REFERENCES tenants(id),
    period_start  DATE NOT NULL,
    metric        TEXT NOT NULL,
    value         BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, period_start, metric)
);

CREATE INDEX IF NOT EXISTS tenant_usage_tenant_period_idx
    ON tenant_usage (tenant_id, period_start);

ALTER TABLE tenant_usage ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON tenant_usage;
CREATE POLICY tenant_isolation ON tenant_usage
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_usage TO kapp_app;

CREATE TABLE IF NOT EXISTS plan_definitions (
    name          TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    limits        JSONB NOT NULL DEFAULT '{}'::jsonb,
    features      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT ON plan_definitions TO kapp_app;

-- Seed the four canonical plans. UPSERT so a re-run of the migration
-- refreshes limits/features without wiping out manual overrides.
INSERT INTO plan_definitions (name, display_name, limits, features)
VALUES
    ('free', 'Free', '{
        "api_calls": 10000,
        "storage_bytes": 1073741824,
        "krecord_count": 1000,
        "user_seats": 3
    }'::jsonb, '{
        "crm": true,
        "finance": false,
        "inventory": false,
        "hr": false,
        "lms": false,
        "helpdesk": false,
        "reporting": false
    }'::jsonb),
    ('starter', 'Starter', '{
        "api_calls": 100000,
        "storage_bytes": 10737418240,
        "krecord_count": 25000,
        "user_seats": 10
    }'::jsonb, '{
        "crm": true,
        "finance": true,
        "inventory": true,
        "hr": false,
        "lms": false,
        "helpdesk": false,
        "reporting": false
    }'::jsonb),
    ('business', 'Business', '{
        "api_calls": 1000000,
        "storage_bytes": 107374182400,
        "krecord_count": 250000,
        "user_seats": 50
    }'::jsonb, '{
        "crm": true,
        "finance": true,
        "inventory": true,
        "hr": true,
        "lms": true,
        "helpdesk": true,
        "reporting": true
    }'::jsonb),
    ('enterprise', 'Enterprise', '{
        "api_calls": 10000000,
        "storage_bytes": 1099511627776,
        "krecord_count": 5000000,
        "user_seats": 500
    }'::jsonb, '{
        "crm": true,
        "finance": true,
        "inventory": true,
        "hr": true,
        "lms": true,
        "helpdesk": true,
        "reporting": true
    }'::jsonb)
ON CONFLICT (name) DO UPDATE SET
    display_name = EXCLUDED.display_name,
    limits = EXCLUDED.limits,
    features = EXCLUDED.features,
    updated_at = now();
