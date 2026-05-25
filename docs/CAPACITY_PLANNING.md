# Capacity Planning

This guide turns the multi-tenant invariants in [ARCHITECTURE.md](../ARCHITECTURE.md)
into concrete numbers operators can size their cells against. Every
formula in this document is derived from the actual access patterns
in `internal/record/store.go`, `internal/insights/store.go`,
`internal/platform/db.go`, and `services/worker/main.go`.

Cross-references:

- Scaling actions when a threshold is crossed: [SCALING_RUNBOOK.md](./SCALING_RUNBOOK.md)
- Cell architecture & provisioning: [MULTI_CELL_OPERATIONS.md](./MULTI_CELL_OPERATIONS.md)
- Network topology & bandwidth: [INFRASTRUCTURE.md](./INFRASTRUCTURE.md)
- Per-query tuning: [PERFORMANCE_TUNING.md](./PERFORMANCE_TUNING.md)

---

## 1. Resource Sizing Matrix

Baseline assumptions (per active tenant, average):

| Resource           | Per-tenant baseline                |
| ------------------ | ---------------------------------- |
| `krecords` rows    | 50,000                             |
| `events` rows      | 20,000 (90-day retention)          |
| `audit_log` rows   | 200,000 (7-year retention)         |
| `journal_lines`    | 10,000                             |
| Files (sum)        | 500 MB                             |
| Concurrent users   | 5 (p95 across the SME baseline)    |
| Daily API requests | 15,000                             |

Sizing per cell, by tenant count (numbers are minimums — round up to the
next standard instance class your cloud offers). Storage figures
include indexes (≈ 1.6× raw rowsize), WAL retention (24 h), and 30%
free headroom.

| Tenants | PostgreSQL (vCPU / RAM / IOPS / storage) | API pods (replicas / CPU / mem) | Worker pods (primary + standby) | NATS (nodes / mem / JetStream storage) | Redis (mem) | Object store (throughput / storage)  | PgBouncer (`max_client_conn` / `default_pool_size`) |
| ------: | ---------------------------------------- | ------------------------------- | ------------------------------- | -------------------------------------- | ----------- | ------------------------------------ | --------------------------------------------------- |
|   100   | 2 / 8 GB / 1,500 / 80 GB                 | 2 / 250m / 256 Mi               | 1 + 1                            | 1 / 256 Mi / 2 GB                      | 256 MB      | 50 MB/s / 75 GB                      | 200 / 25                                            |
|   500   | 4 / 16 GB / 3,000 / 350 GB               | 3 / 500m / 512 Mi               | 1 + 1                            | 3 / 512 Mi / 8 GB                      | 1 GB        | 200 MB/s / 375 GB                    | 500 / 50                                            |
|  1,000  | 8 / 32 GB / 6,000 / 700 GB               | 4 / 1 / 1 Gi                    | 1 + 1                            | 3 / 1 GB / 16 GB                       | 2 GB        | 500 MB/s / 750 GB                    | 1,000 / 75                                          |
|  5,000  | 16 / 64 GB / 15,000 / 3.5 TB             | 8 / 1 / 1.5 Gi                  | 2 + 1 (sharded by namespace)    | 5 / 2 GB / 64 GB                       | 8 GB        | 2 GB/s / 3.75 TB                     | 2,500 / 100                                         |
| 10,000  | 32 / 128 GB / 30,000 / 7 TB **— split cells at this point** | 16 / 1 / 2 Gi  | 2 + 1                            | 5 / 4 GB / 128 GB                      | 16 GB       | 4 GB/s / 7.5 TB                      | 5,000 / 100                                         |

Notes:

- API memory baseline of **128 Mi idle + ~50 Mi per 100 concurrent
  connections** comes from the Go runtime overhead measured under the
  k6 100-VU load profile in `internal/integrationtest/loadtest`.
- Worker memory ≈ API memory ÷ 2 (no HTTP request goroutines).
- Redis lines are sized at **1 KB per active tenant rate-limiter
  bucket** (sliding window + token meta) + 256 MB shared cache.
- PgBouncer `default_pool_size` is per-database; the application
  uses a single database. Raise gradually — see §1.6.

### 1.1 Resource Sizing Worksheet

```
api_pods           = ceil((peak_rps * p99_latency_s) / target_in_flight_per_pod)
                     # target_in_flight_per_pod = 50 for 1 vCPU, 100 for 2 vCPU
worker_pods        = max(2, ceil(tenants / 2500))
                     # 1 primary + 1 standby = 2 minimum
pgbouncer_pool     = min(100, max(25, ceil(api_pods * 10)))
nats_nodes         = 3 if tenants < 5000 else 5
redis_memory_mb    = 256 + (active_tenants_per_minute * 1 KB)
db_storage_gb      = num_tenants * (
                       avg_krecords      * 2 KB +
                       avg_events        * 0.5 KB +
                       avg_audit_entries * 1 KB +
                       avg_journal_lines * 0.3 KB
                     ) * 1.6 + wal_24h_gb + free_headroom_gb
```

### 1.2 Database Sizing

**Storage formula** (per cell, in GB):

```
storage_gb = num_tenants * (
                avg_krecords      * 2 KB    +   # JSONB + indexes
                avg_events        * 0.5 KB  +   # outbox + delivery state
                avg_audit_entries * 1 KB    +   # hash chain + JSON payload
                avg_journal_lines * 0.3 KB
             ) / 1_000_000

# Example: 1000 tenants, baseline averages
storage_gb = 1000 * (50_000*2 + 20_000*0.5 + 200_000*1 + 10_000*0.3) /1_000_000
           = 1000 * (100 + 10 + 200 + 3) /1000
           = ~313 GB raw → ~500 GB provisioned (× 1.6 indexes/WAL headroom)
```

**Sustained IOPS** (read + write, both directions):

```
sustained_iops = concurrent_users * 3 * 1.2

# Example: 5000 tenants × 5 concurrent users × 3 queries/request × 1.2 write amp
             = 5000 * 5 * 3 * 1.2 = 90_000 sustained IOPS at peak
# Provision burst credits for ~5× sustained (450k) for backups + vacuum windows.
```

**WAL generation rate** (for replication bandwidth):

```sql
-- Sample over 60 seconds on a representative cell
SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0')
     / extract(epoch FROM now() - pg_postmaster_start_time())
     AS bytes_per_second;

-- Rule of thumb: provision streaming-replication bandwidth at
-- 3 × measured WAL byte rate, sustained, with a 10× burst for
-- bulk imports.
```

**Partition count guidance** — partitioned tables `krecords`, `events`,
`audit_log`, `journal_lines`, `inventory_moves` use `PARTITION BY
RANGE (tenant_id)`. Create one range partition per ~100 tenants for
hot tables:

```sql
-- After adding tenants 1..100 (lexicographic by UUID), pre-create the next:
CREATE TABLE krecords_p001 PARTITION OF krecords
  FOR VALUES FROM ('00000000-0000-0000-0000-000000000000')
              TO ('19999999-ffff-ffff-ffff-ffffffffffff');
-- See migrations/000001_initial_schema.sql for the parent definitions.
```

### 1.3 Cell Boundary Guidelines

Provision a new cell — *do not* keep scaling an existing one — when any
**single** condition holds for the listed window:

| Signal                                              | Threshold                  | Sustained window |
| --------------------------------------------------- | -------------------------- | ---------------- |
| DB CPU                                              | > 70%                      | 30 minutes        |
| Connection-pool utilization (`SHOW POOLS`)          | > 80% (`cl_active / max`)  | 15 minutes        |
| p99 API latency (`kapp_request_duration_seconds`)   | > 500 ms                   | 30 minutes        |
| Provisioned-disk usage                              | > 80%                      | n/a (immediate)   |
| Tenant count                                        | > 5,000                    | n/a               |

When two or more signals fire simultaneously, treat it as a SEV-2 and
fast-track cell provisioning — see [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) §3.2.

**Cell provisioning checklist** (each step verified before the next):

1. Provision PostgreSQL primary + 2 replicas, same major/minor as the
   existing cell (see [MULTI_CELL_OPERATIONS.md](./MULTI_CELL_OPERATIONS.md) §6.2).
2. Run `make migrate` against the new primary.
3. Provision compute namespace (`kubectl create namespace kapp-cell-<id>`)
   and deploy `api`, `worker`, `kchat-bridge`.
4. Provision object-storage bucket (per-tenant ZK Fabric buckets are
   created lazily by the wizard — only the cell-level fallback bucket
   needs pre-provisioning).
5. Provision NATS cluster (3 nodes < 5,000 tenants, 5 nodes ≥ 5,000).
6. Register cell in the control plane:
   ```sql
   INSERT INTO cells (id, region, max_tenants)
   VALUES ('cell-b', 'us-east-1', 5000);
   ```
7. Configure DNS:  `api.cell-b.kapp.example.com`,
   `portal.cell-b.kapp.example.com` → load balancer for the new
   namespace.
8. Verify `/healthz` returns `200` from every service in the cell.

### 1.4 Cost Modeling

Express cost cloud-agnostically in three units: **vCPU-hours**,
**GB-months** (RAM and disk), and **IOPS-hours** (provisioned I/O).
Per-tenant cost at three scales (excluding network egress):

| Scale         | vCPU-h / tenant / mo | GB-mo (RAM) / tenant | GB-mo (disk) / tenant | IOPS-h / tenant / mo |
| ------------- | -------------------: | -------------------: | --------------------: | -------------------: |
| 100 tenants   | 31.2                 | 0.51                 | 0.80                  | 1,080                |
| 1,000 tenants | 7.2                  | 0.18                 | 0.50                  | 432                  |
| 5,000 tenants | 3.7                  | 0.07                 | 0.35                  | 216                  |
| 10,000 tenants| 2.9                  | 0.06                 | 0.30                  | 216                  |

**Break-even: shared vs. dedicated tier.** The dedicated tier
(`scripts/upgrade_tier.sh`) carves out a per-tenant schema with
dedicated worker queues. The marginal cost of running a tenant in a
shared cell at the 5,000-tenant scale (~$2.40/mo at $0.05/vCPU-h) is
crossed when a tenant exceeds **roughly 20× the average row counts
and 5× the average API traffic** of a baseline tenant. Below that, a
dedicated tier costs more and delivers no isolation benefit beyond
what RLS + per-tenant rate-limiting already provides.

**Zero-idle-cost verification.** Idle tenants must contribute:

- Disk (rows at rest, no autovacuum sweep without dead rows): yes
- Storage tier object cost (files at rest): yes
- Compute (process scheduling, request handling): **zero**
- RAM (cached state): **zero** beyond the LRU-cached metadata lookup
  (`internal/tenant/cache.go`, < 4 KB per tenant)

Confirm by sampling on a populated cell:

```sql
-- Inactive tenants in the last 7 days
SELECT tenant_id,
       (SELECT count(*) FROM krecords k WHERE k.tenant_id = t.id) AS records,
       (SELECT count(*) FROM events e   WHERE e.tenant_id = t.id
          AND e.created_at > now() - interval '7 days') AS recent_events
FROM tenants t
WHERE NOT EXISTS (
  SELECT 1 FROM audit_log a
   WHERE a.tenant_id = t.id AND a.created_at > now() - interval '7 days'
);
-- recent_events MUST be 0 for inactive tenants
```

### 1.5 Growth Projection Template

Operators can paste the table below into a spreadsheet and extend the
month rows for 6/12/24-month projections. `tenant_growth_rate` is a
month-over-month decimal (0.10 = 10 % / mo).

| Month | Tenants                              | DB storage (GB)                                                 | API pods                                   | Worker pods                          |
| ----: | ------------------------------------ | --------------------------------------------------------------- | ------------------------------------------ | ------------------------------------ |
| 0     | T₀                                   | =T₀ * 313 / 1000                                                | =CEILING(T₀/250, 1) + 1                    | 2                                    |
| 1     | =T₀ * (1 + g)                        | =T₁ * 313 / 1000                                                | =CEILING(T₁/250, 1) + 1                    | =MAX(2, CEILING(T₁/2500, 1) + 1)     |
| n     | =T_(n-1) * (1 + g)                   | =T_n * 313 / 1000                                               | =CEILING(T_n/250, 1) + 1                    | =MAX(2, CEILING(T_n/2500, 1) + 1)    |

When the storage column crosses 5 TB or the API-pods column crosses
16, plan to provision a new cell (§1.3) instead of growing the
existing one.

### 1.6 PgBouncer Sizing

Pool sizing must satisfy:

```
max_client_conn  >= api_pods * 200 + worker_pods * 50 + bridge_pods * 50
default_pool_size  <= min(100, postgres_max_connections / num_pools)
                      # one pool = one (db, user) pair; Kapp uses two
                      # (kapp_app, kapp_admin), so divide by 2
```

Refuse to raise `default_pool_size` above 100 — Postgres's per-connection
RAM (~10 MB shared_buffers + backend overhead) makes 200+ backends per
database the wrong knob. Add API pods instead.

```ini
# pgbouncer.ini, 5,000-tenant cell example
[pgbouncer]
pool_mode             = transaction
max_client_conn       = 2500
default_pool_size     = 100
reserve_pool_size     = 20
reserve_pool_timeout  = 5
server_idle_timeout   = 60
query_wait_timeout    = 30
auth_type             = scram-sha-256
```

Apply changes with `kill -HUP $(pidof pgbouncer)`; verify with
`psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW POOLS;'`.
