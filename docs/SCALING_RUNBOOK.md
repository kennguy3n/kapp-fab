# Scaling Runbook

Operator procedures for adding capacity to a running Kapp cell. Every
section follows the same shape: **trigger → action → verify → rollback**.
Decisions are routed by the thresholds in
[CAPACITY_PLANNING.md §1.3](./CAPACITY_PLANNING.md#13-cell-boundary-guidelines)
and the alerts in [ONCALL_PLAYBOOK.md](./ONCALL_PLAYBOOK.md).

---

## 2.1 Horizontal Scaling — API

**Trigger.** `histogram_quantile(0.99, rate(kapp_request_duration_seconds_bucket[5m])) > 0.3`
for 10 minutes **and** PostgreSQL CPU < 60 % (i.e. compute-bound, not
DB-bound).

**Action.**

```bash
# Direct scale (no autoscaler):
kubectl -n kapp scale deploy/api --replicas=<N>

# HPA-managed (preferred — see deploy/helm/templates/api-hpa.yaml):
kubectl -n kapp patch hpa/api -p '{"spec":{"maxReplicas":<N>}}'
```

**Verify.**

```bash
# New pods pass readiness:
kubectl -n kapp get pods -l app=api -o wide

# /healthz on each:
for p in $(kubectl -n kapp get pods -l app=api -o name); do
  kubectl -n kapp exec "$p" -- wget -qO- http://localhost:8080/healthz
done

# Latency dropping:
curl -s 'http://prometheus:9090/api/v1/query?query=histogram_quantile(0.99,rate(kapp_request_duration_seconds_bucket[5m]))' | jq
```

**Rollback.** If p99 latency has not dropped within 10 minutes after
the new pods are Ready, scale back down — the bottleneck is downstream
(DB / NATS / Redis), not compute. Move to §2.2 / §2.3.

```bash
kubectl -n kapp scale deploy/api --replicas=<previous N>
```

---

## 2.2 Vertical Scaling — PostgreSQL

**Trigger.** PostgreSQL CPU > 70 % sustained 30 min, **AND** mean query
latency low (CPU-bound, not IO-bound). Confirm with:

```sql
-- Wait events should be mostly null/CPU, not IO:
SELECT wait_event_type, count(*) FROM pg_stat_activity
WHERE state = 'active' GROUP BY wait_event_type;

-- Buffer cache hit rate should be > 99 %:
SELECT sum(blks_hit)::float / nullif(sum(blks_hit + blks_read), 0)
FROM pg_stat_database WHERE datname = current_database();
```

**Action — managed Postgres (RDS / Cloud SQL / Azure DB).**

```
1. Take a fresh snapshot (verifies snapshot policy is healthy):
     aws rds create-db-snapshot --db-instance-identifier kapp-cell-a-primary \
                                --db-snapshot-identifier  pre-resize-$(date +%s)
2. Trigger instance-class upgrade with ApplyImmediately=false to land in
   the next maintenance window, or true if SEV-2:
     aws rds modify-db-instance --db-instance-identifier kapp-cell-a-primary \
                                --db-instance-class db.r6g.4xlarge \
                                --apply-immediately
3. RDS performs an in-place restart (~5 min downtime for the primary;
   replicas continue to serve reads).
4. Verify the new size and re-tune shared_buffers / effective_cache_size:
     SHOW shared_buffers; SHOW effective_cache_size; SHOW work_mem;
```

**Action — self-managed Postgres.** Promote a larger replica:

```
1. Provision new host (larger class) with streaming replica role.
2. Wait until replication lag < 100 ms (`pg_last_wal_replay_lsn` keeps up).
3. Pause the API: `kubectl -n kapp scale deploy/api --replicas=0`
4. On old primary: `SELECT pg_promote_safe();` to detach, then promote new:
     pg_ctl promote -D /var/lib/postgresql/<version>/main
5. Update PgBouncer config to point at new primary; reload:
     kill -HUP $(pidof pgbouncer)
6. Re-scale API; confirm /healthz on every pod.
```

**Verify.**

```sql
SELECT count(*) FROM pg_stat_activity WHERE state = 'active';
SHOW shared_buffers; SHOW effective_cache_size; SHOW work_mem;

-- Sanity check the kapp_app role is still present and RLS still works:
SELECT current_user;  -- kapp_admin in admin shell
SET ROLE kapp_app;
SET LOCAL app.tenant_id = '00000000-0000-0000-0000-000000000000';
SELECT count(*) FROM krecords LIMIT 1;
```

**Rollback.** Revert the instance class (RDS) or promote the old
primary back. Migrations are forward-only — schema stays compatible.

---

## 2.3 Adding Read Replicas

Read-heavy workloads (reporting, insights, list queries) push primary
CPU upward. Once primary CPU > 60 % AND `read:write` ratio > 5:1, add
a read replica.

**Steps.**

1. Provision a streaming replica on the same PG version:
   ```
   # Managed: aws rds create-db-instance-read-replica ...
   # Self-managed: pg_basebackup -h primary -D /var/lib/postgresql/...
   ```
2. Wait for replication lag < 1 s:
   ```sql
   SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS lag_bytes,
          extract(epoch FROM now() - pg_last_xact_replay_timestamp()) AS lag_seconds
   FROM pg_stat_replication;
   ```
3. Point each Kapp service at the replica via the A1 env vars (set
   per-service on the api / worker / kchat-bridge / importer /
   agent-tools deployments):

   | Variable | Default | Purpose |
   |---|---|---|
   | `KAPP_READ_REPLICA_URL` | _(unset)_ | libpq DSN for the read-only pool. Leave unset to keep every read on the primary. |
   | `KAPP_READ_REPLICA_LAG_TOLERANCE` | `1s` | Max observed lag before the router falls back to the primary. Tighten when the workload is read-after-write sensitive. |
   | `KAPP_READ_REPLICA_LAG_SAMPLE_INTERVAL` | `5s` | How often the in-process sampler refreshes its lag observation. |

   Wiring lives in `internal/dbutil/pool_router.go`. Reads route to the
   replica only when the most recent lag sample is fresh AND within
   tolerance; on any uncertainty (sampler stalled, replica unreachable,
   no replica configured) the router falls back to the primary
   transparently. All writes always go to the primary regardless.
4. Rolling restart of API pods:
   ```bash
   kubectl -n kapp rollout restart deploy/api
   kubectl -n kapp rollout status  deploy/api --timeout=300s
   ```
5. Verify the rollout via the A1 metrics:
   - `kapp_replica_configured == 1` on every pod with the replica wired
   - `kapp_replica_lag_seconds` stays under tolerance during steady state
   - `kapp_request_total{path=~"/api/v1/reports.*"}` and
     `pg_stat_activity.numbackends` on the primary trend down
6. Monitor replication lag continuously. Alert when
   `kapp_replica_lag_seconds > 5` for > 1m (router will fall back to
   primary, but degraded read fan-out is worth a page); remove from
   pool when lag exceeds `KAPP_READ_REPLICA_LAG_TOLERANCE * 30` for an
   extended window (router will already have stopped using it).

**Rollback.** Unset `KAPP_READ_REPLICA_URL`, rollout-restart pods,
confirm `kapp_replica_configured == 0` and reads route back to primary.
Tear down the replica once drained.

---

## 2.4 Provisioning a New Cell

Use the §1.3 cell-boundary trigger from [CAPACITY_PLANNING.md](./CAPACITY_PLANNING.md).
This is a 30–45 minute operation; do not run it during peak hours.

**Pre-flight.**

```bash
# Confirm the existing cell is healthy (don't migrate from a sick cell):
for svc in api worker kchat-bridge; do
  kubectl -n kapp get deploy/$svc -o jsonpath='{.status.readyReplicas}/{.spec.replicas}'; echo
done

# Verify migration tip on the source DB:
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate version
```

**Steps.**

```bash
# 1. Provision DB cluster (primary + 1 replica minimum)
#    Use a Terraform / Pulumi module from MULTI_CELL_OPERATIONS.md §6.2.

# 2. Apply schema to the new primary
DB_URL=postgres://kapp:$KAPP_NEW_DB_PASSWORD@new-primary:5432/kapp?sslmode=verify-full \
  make migrate
DB_URL=postgres://kapp:$KAPP_NEW_DB_PASSWORD@new-primary:5432/kapp?sslmode=verify-full \
  make migrate-version  # should print version=60 (or current tip)

# 3. Provision compute pool
kubectl create namespace kapp-cell-b
kubectl -n kapp-cell-b apply -f deploy/k8s/api.yaml
kubectl -n kapp-cell-b apply -f deploy/k8s/worker.yaml
kubectl -n kapp-cell-b apply -f deploy/k8s/kchat-bridge.yaml

# 4. Provision NATS (3 nodes < 5k tenants, 5 nodes >=5k)
helm install nats-cell-b nats/nats -n kapp-cell-b --values deploy/helm/nats-cell-b.yaml

# 5. Provision object store (ZK Fabric cell or MinIO cluster)
kubectl -n kapp-cell-b apply -f deploy/k8s/zk-fabric.yaml

# 6. Register cell in control plane:
psql "$KAPP_ADMIN_DB_URL" <<SQL
INSERT INTO cells (id, region, max_tenants)
VALUES ('cell-b', 'us-east-1', 5000)
ON CONFLICT (id) DO NOTHING;
SQL

# 7. Verify /healthz on every service in the new cell:
for svc in api worker kchat-bridge; do
  kubectl -n kapp-cell-b exec deploy/$svc -- wget -qO- http://localhost:8080/healthz
done

# 8. Assign tenants to the new cell (10 at a time to bound risk):
psql "$KAPP_ADMIN_DB_URL" <<SQL
UPDATE tenants
SET cell_id = 'cell-b', updated_at = now()
WHERE id IN ( /* curated 10-tenant batch */ );
SQL

# 9. Verify tenant access from the new cell — see §2.5 verify steps.
```

**Rollback.** If verification fails, revert step 8 (`cell_id = 'default'`
or original), tear down the new namespace, drop the new DB.

---

## 2.5 Tenant Migration Between Cells

Used for rebalancing load (§2.4 step 8) or decommissioning a cell
(see [MULTI_CELL_OPERATIONS.md](./MULTI_CELL_OPERATIONS.md) §6.5).

**Steps.**

```bash
TENANT_ID=<uuid>
SRC_DB="$KAPP_ADMIN_DB_URL"
DST_DB="postgres://kapp_admin:...@new-primary:5432/kapp?sslmode=verify-full"
DST_CELL=cell-b

# 1. Suspend the tenant. Sessions revoke immediately; the worker
#    drops scheduled jobs for this tenant on the next tick.
curl -s -XPOST -H "Authorization: Bearer $ADMIN_TOKEN" \
  "https://api.kapp.example.com/api/v1/admin/tenants/$TENANT_ID/suspend"

# 2. Extract (one-pass JSONL dump):
go run ./services/kapp-backup extract \
   --db "$SRC_DB" \
   --tenant "$TENANT_ID" \
   --out  /tmp/$TENANT_ID.jsonl

# 3. Restore into target cell:
go run ./services/kapp-backup restore \
   --db "$DST_DB" \
   --in /tmp/$TENANT_ID.jsonl
# Restore is atomic per table; partial failure rolls back the table
# (see services/kapp-backup/main.go::restoreTable).

# 4. Update cell assignment:
psql "$KAPP_ADMIN_DB_URL" -c "UPDATE tenants SET cell_id='$DST_CELL' WHERE id='$TENANT_ID';"

# 5. Migrate ZK Fabric objects (see DR_RUNBOOK.md §5 — dual-write +
#    repair sweep). For tenants on the global MinIO fallback this
#    step is a no-op.

# 6. Reactivate:
curl -s -XPOST -H "Authorization: Bearer $ADMIN_TOKEN" \
  "https://api.kapp.example.com/api/v1/admin/tenants/$TENANT_ID/activate"

# 7. Verify:
curl -s -H "Authorization: Bearer $ADMIN_TENANT_TOKEN" \
  "https://api.cell-b.kapp.example.com/api/v1/krecords?ktype=crm.deal&limit=5" \
  | jq '.items | length'
```

**Verify after restore.**

```sql
SET LOCAL app.tenant_id = '<TENANT_ID>';
SELECT count(*) FROM krecords;
SELECT count(*) FROM journal_entries;
SELECT count(*) FROM audit_log;
-- Each count should match the source-side equivalent.
```

**Rollback.** Re-activate on the original cell:

```bash
psql "$SRC_DB" -c "UPDATE tenants SET cell_id='default' WHERE id='$TENANT_ID';"
# Then delete the restored data from the target cell so it can't be
# re-routed there by mistake:
psql "$DST_DB" -c "BEGIN; SELECT delete_tenant_data('$TENANT_ID'); COMMIT;"
```

`delete_tenant_data` is a SECURITY DEFINER helper that walks
`TenantScopedTables` (see `services/kapp-backup/main.go`) and deletes
under `BYPASSRLS`. Installed via `migrations/000042_tier_admin_role.sql`.

---

## 2.6 PgBouncer Pool Scaling

**Trigger.** `cl_active` approaching `max_client_conn` (visible via
`SHOW POOLS;` from the pgbouncer admin console), **or** API logs show
`server login failed: timeout`.

**Action.** Increase pool sizes in `pgbouncer.ini`:

```ini
max_client_conn       = 5000   # was 2500
default_pool_size     = 100    # cap; do not raise
reserve_pool_size     = 40     # was 20
reserve_pool_timeout  = 5
```

Reload without dropping connections:

```bash
# K8s-managed:
kubectl -n kapp rollout restart deploy/pgbouncer
kubectl -n kapp rollout status  deploy/pgbouncer --timeout=120s

# Bare host:
kill -HUP $(pidof pgbouncer)
```

**Verify.**

```bash
psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW POOLS;'
psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW CONFIG;'
```

**Rollback.** Revert `pgbouncer.ini` to the previous values; reload.
If pool saturation persists at the cap, the bottleneck is downstream
— scale API pods (§2.1) or tune DB connections via `default_pool_size`
only if Postgres has headroom for more backends. Never push
`default_pool_size` past 100; the right escalation path is "more API
pods" or "split into a new cell".
