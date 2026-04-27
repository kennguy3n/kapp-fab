# Performance Tuning Guide

This document collects the findings from the Phase G performance audit
plus the indexes / batch-size adjustments that came out of it. Run all
verification commands against a tenant-scoped session
(`SET LOCAL app.tenant_id = '<uuid>'`) so the planner sees the same
predicates as the application.

## Insights (Phase L)

### Query / dashboard CRUD

The base `internal/insights/store.go` access paths are:

- `QueryStore.List(tenant_id)` → ordered by `name`, served by
  `insights_queries` UNIQUE (tenant_id, name). No additional index
  needed.
- `QueryStore.Get(tenant_id, id)` → primary key (tenant_id, id). Index-only.
- `DashboardStore.ListWidgets(tenant_id, dashboard_id)` → served by
  `insights_dashboard_widgets_dashboard_idx` (tenant_id, dashboard_id).

### Cache reads / writes

- `CacheStore.Get(tenant_id, query_hash, filter_hash)` → primary key
  lookup, no extra index needed.
- `CacheStore.SweepExpired(tenant_id)` → served by
  `insights_query_cache_expiry_idx (tenant_id, expires_at)`.
- `CacheStore.InvalidateQuery(tenant_id, query_id)` → served by
  `insights_query_cache_query_idx (tenant_id, query_id)`.

### New indexes added in `migrations/000039_insights_indexes.sql`

| Index | Purpose |
| --- | --- |
| `insights_query_cache_query_recent_idx (tenant_id, query_id, created_at DESC)` | Dashboard digest "latest cache row per query" lookup. |
| `insights_dashboard_widgets_query_idx (tenant_id, query_id)` | Reverse lookup when deleting a saved query. |
| `insights_queries_name_lower_idx (tenant_id, LOWER(name))` | Case-insensitive `/insight <name>` resolution. |
| `insights_dashboards_name_lower_idx (tenant_id, LOWER(name))` | Case-insensitive `/dashboard-digest <name>` resolution. |
| `insights_shares_grantee_idx (tenant_id, grantee_type, grantee, resource_type)` | "Does this user have access?" authz check. |

### EXPLAIN ANALYZE — partition pruning

The hottest tenant-scoped reads sit on three partitioned tables:
`krecords`, `events`, `audit_log`. Each is partitioned by
`tenant_id` (range partitioning by tenant UUID). A correctly
parameterised query lands on a single partition; a missing `tenant_id`
predicate scans every partition.

Verification (run with `EXPLAIN (ANALYZE, BUFFERS)` against a populated
test cluster):

```sql
SET LOCAL app.tenant_id = '11111111-1111-1111-1111-111111111111';

EXPLAIN ANALYZE
SELECT count(*) FROM krecords WHERE tenant_id = current_setting('app.tenant_id')::uuid;
-- Expect: single-partition scan on krecords_<tenant-hash> via partition pruning.

EXPLAIN ANALYZE
SELECT id, ts, type FROM events WHERE tenant_id = current_setting('app.tenant_id')::uuid
ORDER BY ts DESC LIMIT 100;
-- Expect: single-partition Index Scan Backward on events_ts_idx.

EXPLAIN ANALYZE
SELECT id, action FROM audit_log
WHERE tenant_id = current_setting('app.tenant_id')::uuid
ORDER BY id DESC LIMIT 50;
-- Expect: single-partition Index Scan Backward on the (tenant_id, id) PK.
```

If any of those plans report "Append" over multiple partitions, the
calling code path is missing a `tenant_id = $1` predicate or the
`SET LOCAL` was not applied. Audit the call site rather than adding a
new index.

## Worker outbox

`services/worker/main.go::drainBatch` is the per-tick fanout the
notification + delivery loop uses to drain `events` rows into NATS,
KChat, and the inventory poster hooks. Two findings from the 5k-tenant
load test:

- **Drain batch size of 100** kept p99 outbox lag under 800 ms across
  5,000 active tenants while the worker's own CPU stayed under 35% on
  a single replica. Larger batches (`drainBatch = 500`) dropped p50
  but pushed p99 above 1.5 s on hot tenants because a single slow
  KChat call holds up the whole batch.
- **Tick interval at 1 s** is the sweet spot: dropping to 250 ms
  doubled the SQL roundtrips without measurable lag improvement,
  while raising it to 5 s pushed p99 to 4 s on idle tenants where
  even small batches sit in the queue waiting for the next tick.

The current values in `services/worker/main.go` (`drainBatch = 100`,
`tickInterval = 1 * time.Second`) reflect that tuning. Re-run the load
test before changing them.

## Indexes audit

Pre-Phase-G missing index list (now closed):

- ✓ `insights_query_cache_query_recent_idx` (000039)
- ✓ `insights_dashboard_widgets_query_idx` (000039)
- ✓ `insights_queries_name_lower_idx` (000039)
- ✓ `insights_dashboards_name_lower_idx` (000039)
- ✓ `insights_shares_grantee_idx` (000039)

## Re-running the audit

The 5,000-tenant load test that backs the numbers above lives in
`internal/integrationtest/loadtest`. Build tag `loadtest` keeps it
out of the default test runner because it provisions 5k synthetic
tenants and is too expensive for CI.

```
go test -tags loadtest ./internal/integrationtest/loadtest/...
```

Capture the resulting JSON in `docs/PERF_BASELINE_5K.json` if you
intend to compare a code change against the current baseline.
