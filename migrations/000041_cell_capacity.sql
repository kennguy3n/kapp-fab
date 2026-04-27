-- Phase G — cell autoscaling.
--
-- The platform places tenants onto "cells" (independent control-plane
-- shards). Each cell has a fixed capacity and an observed live load
-- vector (CPU%, memory%, connection-pool saturation%). The autoscaler
-- in internal/platform/autoscaler.go walks these rows on a cron tick,
-- compares them to the configured policy, and writes a decision row
-- into platform_scale_events plus a structured slog line so the
-- cell-router (or a human operator) can act on it.
--
-- cells and platform_scale_events are control-plane tables (no
-- tenant_id column, no RLS). They live in `public` so the worker's
-- regular pool can read/write them under kapp_app without needing
-- the admin BYPASSRLS pool. The kapp-backup tool excludes them from
-- per-tenant exports because they have no tenant_id; that excludes
-- them from tier upgrades for the same reason, which is correct —
-- a cell row belongs to the platform, not the tenant.

CREATE TABLE IF NOT EXISTS cells (
    id                   TEXT PRIMARY KEY,
    region               TEXT NOT NULL DEFAULT '',
    max_tenants          INTEGER NOT NULL DEFAULT 1000 CHECK (max_tenants > 0),
    cpu_pct              REAL NOT NULL DEFAULT 0 CHECK (cpu_pct >= 0 AND cpu_pct <= 100),
    mem_pct              REAL NOT NULL DEFAULT 0 CHECK (mem_pct >= 0 AND mem_pct <= 100),
    conn_saturation_pct  REAL NOT NULL DEFAULT 0 CHECK (conn_saturation_pct >= 0 AND conn_saturation_pct <= 100),
    observed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE ON cells TO kapp_app;

-- tenants.cell_id binds a tenant to its cell. Optional: NULL means
-- the tenant is on the implicit "default" cell, which the autoscaler
-- treats as cell_id = 'default'. Adding a real cell registry will
-- backfill this column with the chosen placement.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS cell_id TEXT REFERENCES cells(id);

CREATE INDEX IF NOT EXISTS tenants_cell_id_idx ON tenants (cell_id);

-- platform_scale_events records every autoscaler decision (including
-- "hold") so operators can audit policy behaviour over time without
-- relying on log retention.
CREATE TABLE IF NOT EXISTS platform_scale_events (
    id          BIGSERIAL PRIMARY KEY,
    cell_id     TEXT NOT NULL,
    event_type  TEXT NOT NULL CHECK (event_type IN ('scale_up', 'scale_down', 'hold')),
    reason      TEXT NOT NULL,
    snapshot    JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS platform_scale_events_cell_idx
    ON platform_scale_events (cell_id, created_at DESC);

GRANT SELECT, INSERT ON platform_scale_events TO kapp_app;
GRANT USAGE, SELECT ON SEQUENCE platform_scale_events_id_seq TO kapp_app;

-- Seed a single 'default' cell so existing tenants implicitly belong
-- somewhere. Idempotent so re-running the migration is safe.
INSERT INTO cells (id, region, max_tenants)
VALUES ('default', 'local', 1000)
ON CONFLICT (id) DO NOTHING;
