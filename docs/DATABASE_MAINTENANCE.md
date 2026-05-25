# Database Maintenance Runbook

PostgreSQL operator procedures for a Kapp cell. Every query is safe to
run on a healthy primary or replica unless explicitly flagged as
destructive (`pg_terminate_backend`, `REINDEX` without
`CONCURRENTLY`, etc.).

Cross-references:

- DR + backups: [DR_RUNBOOK.md](./DR_RUNBOOK.md)
- Per-query tuning: [PERFORMANCE_TUNING.md](./PERFORMANCE_TUNING.md)
- Capacity sizing: [CAPACITY_PLANNING.md §1.2](./CAPACITY_PLANNING.md#12-database-sizing)
- Schema definitions:
  [migrations/000001_initial_schema.sql](../migrations/000001_initial_schema.sql)
  and later migrations.

---

## 5.1 Vacuum Strategy

Multi-tenant workloads with high turnover (`krecords` updates,
`events` delivery flag flips) accumulate dead tuples fast. Aggressive
autovacuum settings shipping in the recommended PG config:

```ini
# postgresql.conf  (cell-level overrides)
autovacuum_vacuum_scale_factor   = 0.01    # default 0.2 — too lazy for our turnover
autovacuum_analyze_scale_factor  = 0.005
autovacuum_vacuum_cost_delay     = 2ms      # faster sweeps
autovacuum_max_workers           = 6        # one per partitioned hot table
autovacuum_naptime               = 10s
```

Identify tables needing vacuum:

```sql
SELECT schemaname, relname, n_dead_tup, last_autovacuum, last_autoanalyze
FROM pg_stat_user_tables
WHERE n_dead_tup > 10000
ORDER BY n_dead_tup DESC LIMIT 25;
```

Manual vacuum on hot tables during a low-traffic window (≤ 02:00–06:00
local for a baseline SME cell):

```sql
-- Note: tables below are PARTITION BY RANGE (tenant_id). VACUUM on
-- the parent walks every partition; for very large cells, vacuum
-- individual partitions instead to bound runtime.
VACUUM (VERBOSE, ANALYZE) krecords;
VACUUM (VERBOSE, ANALYZE) events;
VACUUM (VERBOSE, ANALYZE) audit_log;
VACUUM (VERBOSE, ANALYZE) journal_lines;
VACUUM (VERBOSE, ANALYZE) inventory_moves;
```

Per-partition vacuum (for cells > 1 TB):

```sql
SELECT child.relname
FROM   pg_inherits
JOIN   pg_class child  ON pg_inherits.inhrelid = child.oid
JOIN   pg_class parent ON pg_inherits.inhparent = parent.oid
WHERE  parent.relname = 'krecords'
ORDER  BY child.relname;

-- Vacuum each partition:
-- VACUUM (ANALYZE) krecords_default;
-- VACUUM (ANALYZE) krecords_p001;
-- VACUUM (ANALYZE) krecords_p002;
```

---

## 5.2 Index Maintenance

Find bloated / unused indexes (zero scans, > 10 MB):

```sql
SELECT relname, indexrelname,
       pg_size_pretty(pg_relation_size(indexrelid)) AS size,
       idx_scan, idx_tup_read
FROM pg_stat_user_indexes
WHERE idx_scan = 0 AND pg_relation_size(indexrelid) > 10485760
ORDER BY pg_relation_size(indexrelid) DESC;
```

Reindex non-blockingly:

```sql
-- Always use CONCURRENTLY in production. Locks are AccessShareLock only.
REINDEX INDEX CONCURRENTLY krecords_tenant_ktype_updated_idx;
REINDEX TABLE CONCURRENTLY events;
-- REINDEX TABLE CONCURRENTLY does NOT walk partitions; reindex each:
-- REINDEX TABLE CONCURRENTLY events_default;
```

Schedule:

- **Monthly** REINDEX on high-write tables (`events`, `audit_log`,
  `krecords`, `journal_lines`).
- **30-day dormancy threshold** before dropping a zero-scan index —
  guard against weekly-only batch reports that exercise it.

---

## 5.3 Partition Management

Current partition layout (cells > 1 TB should walk this monthly):

```sql
SELECT parent.relname AS parent,
       child.relname  AS partition,
       pg_size_pretty(pg_relation_size(child.oid))  AS size,
       pg_get_expr(child.relpartbound, child.oid)   AS bound
FROM pg_inherits
JOIN pg_class parent ON pg_inherits.inhparent = parent.oid
JOIN pg_class child  ON pg_inherits.inhrelid = child.oid
WHERE parent.relname IN
   ('krecords','events','audit_log','journal_lines','inventory_moves','leave_ledger')
ORDER BY parent.relname, child.relname;
```

Add a new partition before the existing one fills (target ≤ 100
tenants per partition for hot tables):

```sql
BEGIN;
CREATE TABLE krecords_p003 PARTITION OF krecords
  FOR VALUES FROM ('30000000-0000-0000-0000-000000000000')
              TO ('39999999-ffff-ffff-ffff-ffffffffffff');
CREATE INDEX ON krecords_p003 (tenant_id, ktype, updated_at DESC);
COMMIT;
```

Detach a partition for archival (tenants in this UUID range must
already be migrated to another cell):

```sql
ALTER TABLE krecords DETACH PARTITION krecords_p001;
-- Then dump and drop:
pg_dump --table=krecords_p001 -Fc "$KAPP_ADMIN_DB_URL" > krecords_p001.dump
DROP TABLE krecords_p001;
```

---

## 5.4 Connection Health

```bash
psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW POOLS;'
psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW CLIENTS;'
psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW SERVERS;'
```

Terminate runaway connections (always inspect first — never
`pg_terminate_backend` an unknown query):

```sql
-- Inspect:
SELECT pid, usename, state, now() - xact_start AS xact_age, query
FROM pg_stat_activity
WHERE state = 'idle in transaction'
  AND xact_start < now() - interval '5 minutes';

-- Terminate (only after inspection):
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE state = 'idle in transaction'
  AND xact_start < now() - interval '5 minutes';
```

**Weekly connection-leak audit.**

```sql
-- Long-running queries (NOT idle, but active and old):
SELECT pid, usename, now() - query_start AS age, query
FROM pg_stat_activity
WHERE state = 'active' AND query_start < now() - interval '1 minute'
ORDER BY age DESC;
```

PgBouncer reload procedure after `pgbouncer.ini` changes:

```bash
kill -HUP $(pidof pgbouncer)
psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW CONFIG;' | grep -E 'pool|conn'
```

---

## 5.5 Backup Verification

Backups are taken by `services/kapp-backup` (per-tenant JSONL) and
`pg_dump` on the replica (full-cell). See
[DR_RUNBOOK.md §1](./DR_RUNBOOK.md#1-backup--restore) for the
authoritative procedures.

**Daily.** Verify backup existence and monotonic growth:

```bash
aws s3 ls s3://kapp-backups/cell-a/$(date -u +%Y-%m-%d)/ \
  --recursive --summarize
# Expected: full.tar.gz size >= yesterday * 0.95 (allow 5% shrink).
```

**Weekly.** Restore the latest backup to a scratch instance and run
row-count parity:

```bash
# Restore full-cell backup to scratch:
psql "$SCRATCH_DB_URL" <<SQL
DROP SCHEMA public CASCADE; CREATE SCHEMA public;
SQL
pg_restore -d "$SCRATCH_DB_URL" -Fc s3://kapp-backups/cell-a/$(date -u +%Y-%m-%d)/full.dump

# Row-count parity:
psql "$KAPP_ADMIN_DB_URL" -At <<SQL > /tmp/prod_counts.csv
COPY (SELECT 'krecords' AS t, count(*) FROM krecords UNION ALL
      SELECT 'events',         count(*) FROM events  UNION ALL
      SELECT 'audit_log',      count(*) FROM audit_log UNION ALL
      SELECT 'journal_lines',  count(*) FROM journal_lines)
TO STDOUT WITH CSV
SQL
psql "$SCRATCH_DB_URL" -At <<SQL > /tmp/restore_counts.csv
COPY (SELECT 'krecords' AS t, count(*) FROM krecords UNION ALL
      SELECT 'events',         count(*) FROM events  UNION ALL
      SELECT 'audit_log',      count(*) FROM audit_log UNION ALL
      SELECT 'journal_lines',  count(*) FROM journal_lines)
TO STDOUT WITH CSV
SQL
diff /tmp/prod_counts.csv /tmp/restore_counts.csv  # must be empty
```

**Monthly.** Full DR restore drill against an isolated cell — follow
[DR_RUNBOOK.md §6 chaos drill](./DR_RUNBOOK.md). Document the time-to-restore;
the SLO is < 60 minutes for an entire cell.

---

## 5.6 Query Performance Monitoring

`pg_stat_statements` is enabled in
`migrations/000010_phase_g.sql`. Sample the top-10 hot queries:

```sql
SELECT query, calls, mean_exec_time, total_exec_time, rows
FROM pg_stat_statements
ORDER BY mean_exec_time DESC LIMIT 10;
```

Common findings & fixes:

| Symptom                                              | Likely cause                                                  | Fix                                                                                                                       |
| ---------------------------------------------------- | ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Sequential scan on `krecords`                        | Missing `tenant_id` predicate (partition pruning skipped)     | Audit the call site; every query MUST include `WHERE tenant_id = $1`. See `internal/record/store.go`.                     |
| Sequential scan on `audit_log`                       | Missing `tenant_id` predicate or scan over time range > 90 d  | Add `tenant_id` filter, narrow time range, paginate.                                                                       |
| `JSONB` parse on every row                           | `data->>'field'` in WHERE clause without a GIN/jsonb_path_ops index | Add expression index, or pre-promote the field via a generated column.                                                |
| Mean exec time > 1 s                                 | Investigate immediately — no production query should sit here | `EXPLAIN (ANALYZE, BUFFERS)` and tune.                                                                                     |
| `total_exec_time` dominated by 1 query, 1 caller     | Probably a report path with cardinality blow-up                | Add LIMIT, materialise a saved view, or move to insights cache.                                                            |

Reset after tuning to make new measurements obvious:

```sql
SELECT pg_stat_statements_reset();
```

---

## 5.7 WAL Management

WAL generation rate:

```sql
SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')
     / extract(epoch FROM now() - pg_postmaster_start_time())
     AS bytes_per_second;
```

Replication slots retaining WAL:

```sql
SELECT slot_name, plugin, slot_type, active,
       pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS retained_bytes
FROM pg_replication_slots
ORDER BY retained_bytes DESC;
```

A growing `retained_bytes` with `active=false` means a consumer is
down and pinning WAL. Drop the slot after 24 hours if it is
unrecoverable:

```sql
SELECT pg_drop_replication_slot('<slot_name>');
```

WAL archiving status:

```sql
SELECT archived_count, last_archived_wal, last_archived_time,
       failed_count, last_failed_wal, last_failed_time
FROM pg_stat_archiver;
```

A non-zero `failed_count` with a recent `last_failed_time` means
`archive_command` is failing — WAL will accumulate on disk and
eventually fill the volume. Investigate immediately (storage
endpoint? credentials? IAM?).

`archive_command` reference (S3-compatible target):

```ini
archive_mode    = on
archive_command = 'aws s3 cp %p s3://kapp-wal/cell-a/%f --no-progress --quiet'
```

---

## 5.8 Schema Migration Operations

The migration CLI (`cmd/migrate`) wraps `golang-migrate` with the
repo's custom source driver — see `cmd/migrate/main.go`.

```bash
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate version
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate up
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate down 1     # roll back N
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate force <V>  # recover from dirty state
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate bootstrap  # prime schema_migrations
```

Safety guarantees (see CI: `.github/workflows/migration-numbering-check.yml`,
`migration-rls-check.yml`):

- Migrations are numbered contiguously; gaps fail CI.
- Tenant-scoped tables MUST enable RLS; missing policies fail CI.
- The migration CLI refuses to start on a partially migrated DB —
  use `migrate-bootstrap` then `migrate up`.

For breaking schema changes, see
[UPGRADE_RUNBOOK.md §7.5](./UPGRADE_RUNBOOK.md#75-database-migration-safety-checklist).
