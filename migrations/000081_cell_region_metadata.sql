-- Stream 5 (Multi-Region Automation + Cell Autoscaling) — cell region
-- metadata.
--
-- The autoscaler (internal/platform/autoscaler.go) and the new cell
-- provisioner (internal/platform/cell_provisioner.go) need richer
-- placement metadata than the bare `region` column added in
-- migrations/000041_cell_capacity.sql. When auto-provisioning is
-- enabled (KAPP_AUTOSCALE_PROVISION=true) a provisioner creates and
-- tears down cells; this migration adds the columns it needs to record
-- WHERE a cell lives and WHAT lifecycle state it is in:
--
--   * provider   — infrastructure backend ('aws', 'gcp', 'docker',
--                  'baremetal', …). Free-form; empty for pre-existing
--                  cells.
--   * zone       — availability zone / sub-region within `region`.
--   * endpoint   — control endpoint the cell-router uses to reach the
--                  cell (e.g. an internal URL or host:port).
--   * status     — provisioning lifecycle: 'active' (default, serving
--                  tenants), 'provisioning', 'draining' (being emptied
--                  ahead of teardown), or 'deprovisioned'. These are the
--                  CellStatus* constants in cell_provisioner.go (the
--                  persisted lifecycle), NOT the CellProvisionState probe
--                  result returned by a provisioner's Status() call.
--   * provisioner — which provisioner created the cell ('script',
--                  'webhook', 'noop', or '' for manually-seeded cells).
--   * metadata   — provider-specific bag (VPC id, RDS arn, …) kept as
--                  JSONB so operators can attach arbitrary placement
--                  detail without further schema churn.
--
-- Like `cells` itself, every column added here is CONTROL-PLANE data:
-- there is no per-customer column and therefore no row-level security
-- (see the RLS note in migrations/000041_cell_capacity.sql). The
-- migration-rls-check workflow only requires ENABLE ROW LEVEL SECURITY
-- for tables whose body declares a customer-scoping column, which this
-- ALTER deliberately does not.
--
-- All statements are idempotent (IF NOT EXISTS / guarded constraint
-- creation) so re-applying the migration against a partially-migrated
-- database is safe.

ALTER TABLE cells
    ADD COLUMN IF NOT EXISTS provider    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS zone        TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS endpoint    TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS status      TEXT  NOT NULL DEFAULT 'active',
    ADD COLUMN IF NOT EXISTS provisioner TEXT  NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS metadata    JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Constrain status to the known lifecycle states. Added separately and
-- guarded so the migration is idempotent (ADD CONSTRAINT has no
-- IF NOT EXISTS before PostgreSQL 17). The guard is scoped to the `cells`
-- table via conrelid (not conname alone): constraint names are unique only
-- per-table in PostgreSQL, and this platform provisions per-tenant schemas
-- on tier upgrade, so a bare conname match could be satisfied by a
-- like-named constraint on a different table/schema and wrongly skip
-- creating ours here.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conname = 'cells_status_check'
           AND conrelid = 'cells'::regclass
    ) THEN
        ALTER TABLE cells
            ADD CONSTRAINT cells_status_check
            CHECK (status IN ('active', 'provisioning', 'draining', 'deprovisioned'));
    END IF;
END$$;

-- The cell-router and autoscaler filter cells by region and by status
-- (e.g. "active cells in eu-west-1 with headroom"). Index the pair so
-- those scans stay cheap as the fleet grows.
CREATE INDEX IF NOT EXISTS cells_region_status_idx
    ON cells (region, status);

-- No new GRANTs required: migrations/000041_cell_capacity.sql already
-- grants SELECT, INSERT, UPDATE on `cells` to kapp_app, which covers the
-- columns added here.
