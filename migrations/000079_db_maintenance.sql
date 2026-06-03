-- Workstream 4 (NoOps Infrastructure) — automated database maintenance.
--
-- internal/platform/db_maintenance.go runs a platform-level loop in the
-- worker (mirroring the cell autoscaler in autoscaler.go): on a daily
-- tick it manages range partitions, refreshes planner statistics,
-- reindexes high-churn indexes, and triggers VACUUM on tables that have
-- accumulated dead tuples. Every action the loop takes is recorded in
-- platform_maintenance_log so an operator can audit what the
-- self-maintaining database did without tailing the worker's logs.
--
-- platform_maintenance_log is a CONTROL-PLANE table: it has no
-- per-customer column and therefore no row-level security, exactly like
-- platform_scale_events (see migrations/000041_cell_capacity.sql). The
-- worker reads and writes it through the regular kapp_app pool — no
-- BYPASSRLS admin pool is required because there is nothing to isolate.
-- The migration-rls-check workflow only requires ENABLE ROW LEVEL
-- SECURITY for tables whose body declares a customer-scoping column,
-- which this table deliberately does not.
--
-- NOTE ON NUMBERING: prefix 000078 is reserved by a parallel workstream
-- merging concurrently, so this file uses 000079 as coordinated. The
-- gap-free monotonic sequence is restored once 000078 lands on main.

CREATE TABLE IF NOT EXISTS platform_maintenance_log (
    id           BIGSERIAL PRIMARY KEY,
    -- task is one of the platform.MaintenanceTask* constants:
    -- 'partition_create', 'analyze', 'reindex', 'vacuum', 'bloat_check'.
    task         TEXT NOT NULL,
    -- target names the relation the task acted on (table, partition, or
    -- index). Empty string for runs that are not relation-specific.
    target       TEXT NOT NULL DEFAULT '',
    -- status records the outcome so a sweep over recent rows answers
    -- "did the nightly maintenance succeed?" without log access.
    status       TEXT NOT NULL CHECK (status IN ('ok', 'skipped', 'warning', 'error')),
    -- detail carries a short human-readable note (skip reason, error
    -- text, bloat ratio, estimated row count, ...).
    detail       TEXT NOT NULL DEFAULT '',
    -- duration_ms is the wall-clock cost of the action; surfaced so an
    -- operator can spot a REINDEX that is trending slower over weeks.
    duration_ms  BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Recent-runs-by-task lookup powers the weekly REINDEX cadence gate:
-- the worker reads the latest successful 'reindex' row to decide whether
-- seven days have elapsed since the last run.
CREATE INDEX IF NOT EXISTS platform_maintenance_log_task_idx
    ON platform_maintenance_log (task, created_at DESC);

GRANT SELECT, INSERT ON platform_maintenance_log TO kapp_app;
GRANT USAGE, SELECT ON SEQUENCE platform_maintenance_log_id_seq TO kapp_app;

COMMENT ON TABLE platform_maintenance_log IS
    'Control-plane audit trail of automated DB maintenance actions (partition creation, ANALYZE, REINDEX, VACUUM, bloat checks) performed by internal/platform.DBMaintenanceLoop. No tenant scoping by design; not row-level-security protected.';
