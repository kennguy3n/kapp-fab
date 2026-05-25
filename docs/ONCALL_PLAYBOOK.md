# On-Call Playbook

For every alert that pages an on-call engineer, this document gives
the **trigger expression**, the **first-response commands**, the
**branching logic**, and the **escalation path**. Severity routing
lives in [INCIDENT_RESPONSE.md §3.1](./INCIDENT_RESPONSE.md#31-severity-definitions).

Metric names follow the implementation in
`internal/platform/metrics.go` (`kapp_request_*`),
`services/worker/main.go` (`kapp_outbox_*`),
`internal/platform/leader.go` (`kapp_leader_active`).
Metrics suffixed with **(planned)** are recommended for v0.2.x — the
expressions still describe the intended PromQL once they ship.

---

## 4.1 KappHighErrorRate

- **Trigger.** `sum(rate(kapp_request_total{status=~"5.."}[5m]))
              / sum(rate(kapp_request_total[5m]))
              > 0.01` sustained 5 minutes.
- **Severity.** SEV-2 (single cell) → SEV-1 if multiple cells.

**First response.**

```bash
# Which paths are erroring?
kubectl -n kapp logs -l app=api --since=5m \
  | grep '"status":5' \
  | jq -r '.path' | sort | uniq -c | sort -rn | head

# Single tenant or many?
kubectl -n kapp logs -l app=api --since=5m \
  | grep '"status":5' \
  | jq -r '.tenant_id' | sort | uniq -c | sort -rn | head
```

Branching:

- **Single tenant**: check `tenants.status`, `tenant_usage` for the
  current period, and recent admin actions
  (`SELECT * FROM audit_log WHERE tenant_id = $1
   AND created_at > now() - interval '1 hour' ORDER BY created_at DESC`).
- **All tenants**: walk the
  [INCIDENT_RESPONSE.md §3.3.1](./INCIDENT_RESPONSE.md#331-api-5xx-spike)
  decision tree (DB → NATS → OOM → rollout → tracing).

**Escalation.** If error rate stays > 1 % for 15 minutes despite a
rollback, escalate to SEV-1 and engage the on-call platform lead.

---

## 4.2 KappHighLatency

- **Trigger.** `histogram_quantile(0.99,
    sum(rate(kapp_request_duration_seconds_bucket[5m])) by (le)) > 2`
  sustained 5 minutes.
- **Severity.** SEV-3 (single endpoint) → SEV-2 (platform-wide).

**First response.**

```sql
-- Long-running active queries (Postgres):
SELECT pid, now() - query_start AS duration, state, query
FROM pg_stat_activity
WHERE state = 'active' AND query_start < now() - interval '2 seconds'
ORDER BY duration DESC LIMIT 20;

-- Lock contention:
SELECT
  blocked_locks.pid     AS blocked_pid,
  blocking_locks.pid    AS blocking_pid,
  blocked_activity.query AS blocked_query,
  blocking_activity.query AS blocking_query
FROM pg_locks blocked_locks
JOIN pg_stat_activity blocked_activity
  ON blocked_activity.pid = blocked_locks.pid
JOIN pg_locks blocking_locks
  ON blocking_locks.locktype = blocked_locks.locktype
 AND blocking_locks.granted = true
 AND blocked_locks.pid != blocking_locks.pid
JOIN pg_stat_activity blocking_activity
  ON blocking_activity.pid = blocking_locks.pid
WHERE NOT blocked_locks.granted;
```

Escalation:

- **Locks** → identify blocker, capture evidence, terminate with
  `SELECT pg_terminate_backend(<pid>)`.
- **CPU bound** (no locks, all queries fast) → scale API pods
  ([SCALING_RUNBOOK.md §2.1](./SCALING_RUNBOOK.md#21-horizontal-scaling-api)).
- **IO bound** (DB CPU OK, disk queue depth high) → check vacuum and
  WAL pressure
  ([DATABASE_MAINTENANCE.md §5.1](./DATABASE_MAINTENANCE.md#51-vacuum-strategy)).
- **Slow span in trace** → open Tempo/Jaeger, filter `service.name=kapp-api
  duration>1s`, identify span; if external integration, escalate to
  the integration owner.

---

## 4.3 KappOutboxLag

- **Trigger.** `kapp_outbox_drain_duration_seconds > 30` sustained 2
  minutes. Emitted from `services/worker/main.go`.
- **Severity.** SEV-3 unless workflow guarantees break — promote to
  SEV-2 if outbox depth > 100,000.

**First response.**

```sql
-- Outbox depth:
SELECT count(*) FROM events WHERE delivered_at IS NULL;

-- Oldest undelivered (a stuck-since timestamp here is the lag):
SELECT id, tenant_id, type, created_at
FROM events
WHERE delivered_at IS NULL
ORDER BY created_at LIMIT 10;

-- Distribution by event type:
SELECT type, count(*)
FROM events
WHERE delivered_at IS NULL
GROUP BY type
ORDER BY count(*) DESC;
```

```bash
# Worker health:
kubectl -n kapp get pods -l app=worker
kubectl -n kapp logs -l app=worker --since=5m \
  | grep -iE 'error|panic|timeout'

# NATS health:
kubectl -n kapp exec deploy/nats-0 -- nats server ping
kubectl -n kapp exec deploy/nats-0 -- nats stream info kapp.events
```

Branching:

- **Worker crashed** (`Restarts > 0`) → check OOM, raise limit, restart.
- **NATS unreachable** → see
  [INCIDENT_RESPONSE.md §3.3.3a](./INCIDENT_RESPONSE.md#333a-nats-cluster-recovery).
- **Specific event type stuck** → consumer logic broken; tail worker
  logs for the type, fix, redeploy.
- **All types stuck, DB hot** → leader election issue, see §4.4.

---

## 4.4 KappLeaderLost

- **Trigger.** `kapp_leader_active == 0` for 30 seconds.
  Emitted from `internal/platform/leader.go`. The worker uses
  PostgreSQL advisory locks (see ADR-010 once merged) for leader
  election so the lock-holder always serializes outbox drains.
- **Severity.** SEV-3. Promote to SEV-2 if no leader within 2 minutes.

**First response.**

```bash
kubectl -n kapp get pods -l app=worker -o wide
kubectl -n kapp describe pod <worker-pod>
kubectl -n kapp logs <worker-pod> --since=10m | grep -i 'leader'
```

```sql
-- Advisory locks held in this database
SELECT locktype, objid, mode, granted, pid
FROM pg_locks WHERE locktype = 'advisory';

-- The lock is keyed on hash of "kapp-worker" by default; if you see
-- granted=false rows piling up, a backend died holding the lock.
```

Branching:

- **Worker pod running but not leader**: check DB connectivity from
  worker pod (`kubectl exec ... -- psql "$KAPP_ADMIN_DB_URL" -c 'SELECT 1'`).
- **Pod crashed**: K8s restarts it; expect re-acquisition within 60 s.
- **Stale advisory lock** (granted=false rows from dead PIDs):
  ```sql
  SELECT pg_terminate_backend(pid) FROM pg_locks
  WHERE locktype = 'advisory'
    AND pid IN (SELECT pid FROM pg_stat_activity WHERE state IS NULL);
  ```

---

## 4.5 KappDBConnectionPoolExhausted

- **Trigger.** `kapp_pgpool_available_connections == 0` for 1 minute. **(planned)**
  Until the gauge ships, derive the same signal from pgbouncer's
  `SHOW POOLS;` (`cl_waiting > 0` sustained 1 minute).
- **Severity.** SEV-2.

**First response.**

```sql
-- Connection breakdown by state and user:
SELECT count(*), state FROM pg_stat_activity GROUP BY state;
SELECT count(*), usename FROM pg_stat_activity GROUP BY usename;

-- Idle in transaction = likely leak:
SELECT pid, now() - xact_start AS age, query
FROM pg_stat_activity
WHERE state = 'idle in transaction' AND xact_start < now() - interval '30 seconds';
```

Branching:

- **Idle-in-transaction > 60 s**:
  ```sql
  SELECT pg_terminate_backend(pid)
  FROM pg_stat_activity
  WHERE state = 'idle in transaction'
    AND xact_start < now() - interval '60 seconds';
  ```
- **No leaks, legitimate load**: bump `default_pool_size` (capped at
  100) and/or scale API pods — see
  [SCALING_RUNBOOK.md §2.6](./SCALING_RUNBOOK.md#26-pgbouncer-pool-scaling).

---

## 4.6 Additional alerts to document

Each alert below ships in `deploy/prometheus/alerts/kapp.yaml` (planned).
Metric names tagged **(planned)** match metrics not yet emitted by the
v0.1.0 code but referenced in the alerts catalogue.

### KappTenantQuotaExceeded
- **Trigger.** `kapp_tenant_quota_exceeded_total` increasing for any
  tenant. **(planned)**
- **Action.** Look up the tenant's plan:
  ```sql
  SELECT t.id, t.plan, p.api_calls_per_hour, p.storage_bytes,
         u.api_calls_period, u.storage_bytes_period
  FROM tenants t
  JOIN plan_definitions p ON p.plan = t.plan
  JOIN tenant_usage u ON u.tenant_id = t.id
   AND u.period = date_trunc('month', now());
  ```
  Notify the tenant admin via the platform notification path; if
  fraudulent burst, set `tenants.status='suspended'`.

### KappReplicaLag
- **Trigger.** `kapp_replica_lag_seconds > 5` for 2 minutes. **(planned)**
- **Action.**
  ```sql
  SELECT * FROM pg_stat_replication;
  -- Calculate lag on the replica:
  SELECT extract(epoch FROM now() - pg_last_xact_replay_timestamp()) AS lag_seconds;
  ```
  At lag > 30 s, remove the replica from `KAPP_READ_REPLICAS` and
  rolling-restart API pods (see
  [SCALING_RUNBOOK.md §2.3](./SCALING_RUNBOOK.md#23-adding-read-replicas)).

### KappCertExpiringSoon
- **Trigger.** TLS certificate expiry < 7 days for any external host.
  cert-manager exposes `certmanager_certificate_expiration_timestamp_seconds`
  — alert when `expiration - time() < 7 * 24 * 3600`.
- **Action.** Force renewal:
  ```bash
  kubectl -n kapp annotate certificate kapp-api-tls \
    cert-manager.io/issue-temporary-certificate="true" --overwrite
  kubectl -n kapp delete secret kapp-api-tls
  ```
  cert-manager re-issues within seconds. Verify with
  `openssl s_client -servername api.kapp.example.com -connect api.kapp.example.com:443 < /dev/null 2>/dev/null | openssl x509 -noout -dates`.

### KappMigrationFailed
- **Trigger.** Deployment migration step exits non-zero. Emitted by
  the CI/CD pipeline.
- **Action.**
  ```bash
  DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate version
  # Identify the problematic migration; if non-idempotent, roll back:
  DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate down 1
  # Investigate, fix, redeploy.
  ```
  Block the deploy until the migration is fixed or reverted — see
  [UPGRADE_RUNBOOK.md §7.5](./UPGRADE_RUNBOOK.md#75-database-migration-safety-checklist).

### KappDiskSpaceLow
- **Trigger.** `node_filesystem_avail_bytes / node_filesystem_size_bytes < 0.20`
  on any volume.
- **Action.**
  ```bash
  # Identify the largest growers:
  kubectl -n kapp exec deploy/postgres-0 -- du -sh /var/lib/postgresql/data/* | sort -h | tail
  # If WAL: check replication slots that may be retaining WAL.
  ```
  See
  [DATABASE_MAINTENANCE.md §5.7](./DATABASE_MAINTENANCE.md#57-wal-management).

### KappNATSSlowConsumer
- **Trigger.** NATS consumer's `num_pending` or `num_redelivered` > 0
  consistently. Emitted by NATS surveyor.
- **Action.**
  ```bash
  kubectl -n kapp exec deploy/nats-0 -- nats consumer info kapp.events kapp-worker
  # If num_pending growing, the worker can't keep up — scale workers,
  # then investigate batching window in services/worker/main.go.
  ```

### KappAuditChainBroken
- **Trigger.** Verification job
  (`kapp-cli audit verify --tenant <id>`) reports a broken hash chain.
- **Severity.** SEV-1 (security).
- **Action.** Do *not* remediate the row. Engage the security IC
  immediately. See
  [INCIDENT_RESPONSE.md §3.3.4](./INCIDENT_RESPONSE.md#334-cross-tenant-data-exposure-sev-1)
  and §3.5 evidence preservation. The hash columns added by
  `migrations/000016_audit_hash_chain.sql` are append-only; any
  contradiction = tampering.

```sql
-- Walk and verify in production:
SELECT id, encode(prev_hash, 'hex'),
       encode(row_hash, 'hex'),
       encode(
         digest(
           coalesce(prev_hash, '\x'::bytea) ||
           id::text::bytea ||
           created_at::text::bytea ||
           coalesce(payload::text, '')::bytea,
           'sha256'
         ), 'hex'
       ) AS recomputed
FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at;
-- Any row where row_hash != recomputed is tampered with.
```
