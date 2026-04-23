-- Phase G: Hardening, Observability, and Scale
-- Adds reorder_level to inventory_items and the saved_views table.

-- ---------------------------------------------------------------------------
-- Inventory: reorder level for low-stock alerts
-- ---------------------------------------------------------------------------

ALTER TABLE inventory_items
    ADD COLUMN IF NOT EXISTS reorder_level NUMERIC(20,4) NOT NULL DEFAULT 0;

-- ---------------------------------------------------------------------------
-- Saved views: per-user, per-KType filter/sort/column persistence
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS saved_views (
    tenant_id   UUID NOT NULL,
    id          UUID NOT NULL,
    user_id     UUID NOT NULL,
    ktype       TEXT NOT NULL,
    name        TEXT NOT NULL,
    filters     JSONB NOT NULL DEFAULT '{}',
    sort        TEXT NOT NULL DEFAULT '',
    columns     JSONB NOT NULL DEFAULT '[]',
    is_default  BOOLEAN NOT NULL DEFAULT FALSE,
    shared      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS saved_views_tenant_user_ktype_idx
    ON saved_views (tenant_id, user_id, ktype);

ALTER TABLE saved_views ENABLE ROW LEVEL SECURITY;

-- Reuse the standard tenant_isolation policy pattern.
DROP POLICY IF EXISTS tenant_isolation ON saved_views;
CREATE POLICY tenant_isolation ON saved_views
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Grant to application role.
GRANT SELECT, INSERT, UPDATE, DELETE ON saved_views TO kapp_app;
