# Incident Response

Operator procedures for triaging, investigating, and resolving
incidents. Use this document as the canonical reference for the
incident commander (IC). Pages are routed by
[ONCALL_PLAYBOOK.md](./ONCALL_PLAYBOOK.md); first-response queries for
specific alerts live there.

For evidence preservation in **security incidents** (cross-tenant
exposure, audit-chain breaks, suspicious admin actions), follow
§3.5 *before* any remediation.

---

## 3.1 Severity Definitions

| Severity | Definition                                                         | Response time      | Resolution target |
| -------- | ------------------------------------------------------------------ | ------------------ | ----------------- |
| SEV-1    | Complete platform outage, data breach, audit-chain integrity violation, or cross-tenant data exposure | 5 minutes          | 1 hour            |
| SEV-2    | Partial outage (> 10 % tenants affected), single-cell down, or one critical workflow broken                            | 15 minutes         | 4 hours           |
| SEV-3    | Degraded performance (SLO breach without functional impact) or a single noisy tenant impacting others                  | 30 minutes         | 24 hours          |
| SEV-4    | Minor issue with documented workaround, or a single tenant affected with a non-blocking error                          | Next business day  | 1 week            |

The IC always classifies on the *observed* effect, not the suspected
cause. Re-classify upward as new evidence appears; never re-classify
downward without explicit operator sign-off.

---

## 3.2 Incident Commander Checklist

1. **Acknowledge the alert in PagerDuty/Opsgenie.** The first
   acknowledger is the IC by default.
2. **Open the incident channel** — `#kapp-incident-<YYYYMMDD>-<n>` in
   Slack / KChat. Pin a one-line summary message.
3. **Classify** per §3.1.
4. **Assign roles** (single-person responsibilities, even if the same
   person holds two roles temporarily):
   - **IC** — drives the response, makes go/no-go calls.
   - **Communications lead** — owns customer status page and internal
     updates.
   - **Technical lead** — runs queries, captures evidence, executes
     remediation under IC direction.
5. **Begin investigation** with the relevant decision tree (§3.3).
6. **Communicate status** every:
   - 15 min for SEV-1/2 (status page + internal channel)
   - 1 h for SEV-3 (internal channel only unless customer-facing)
7. **Resolve and verify**:
   - Error rate < 0.1 % for ≥ 10 minutes
   - p99 latency back inside its SLO
   - No active alerts for the impacted services
   - Affected tenants confirm normal operation (sample 3)
8. **Schedule the post-mortem** within 48 h. SEV-1/2 require a
   blameless post-mortem written within 5 business days; the IC owns
   the document.

---

## 3.3 Decision Trees for Common Incidents

### 3.3.1 API 5xx spike

```
KappHighErrorRate fires
        │
        ▼
1. Check error spread: single tenant or all?
   kubectl -n kapp logs -l app=api --since=5m \
     | grep '"status":5' | jq -r '.tenant_id' | sort | uniq -c | sort -rn
        │
        ├─ Concentrated on 1 tenant ──► §3.3.1a Tenant-specific
        │
        └─ Spread across many tenants
                │
                ▼
2. DB connectivity:
   kubectl -n kapp exec deploy/api -- wget -qO- http://localhost:8080/healthz
   psql "$KAPP_ADMIN_DB_URL" -c 'SELECT 1;'
        │
        ├─ Failing ──► DB outage — see §3.3.2 + scaling §2.2
        │
        └─ OK
                │
                ▼
3. NATS:
   kubectl -n kapp exec deploy/nats-0 -- nats server ping
        │
        ├─ Failing ──► NATS cluster recovery — see §3.3.3
        │
        └─ OK
                │
                ▼
4. OOM kills on the API:
   kubectl -n kapp get pods -l app=api -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[0].lastState.terminated.reason}{"\n"}{end}'
        │
        ├─ OOMKilled ──► Bump memory limit, rollout restart
        │
        └─ No OOM
                │
                ▼
5. Recent rollout? Check Deployment revision:
   kubectl -n kapp rollout history deploy/api
        │
        ├─ Yes, regressed at rev N ──► kubectl -n kapp rollout undo deploy/api
        │
        └─ No ──► Open SEV-2, dig into pprof + traces (§3.4)
```

#### 3.3.1a Tenant-specific 5xx

- Check tenant status: `SELECT status, plan, suspended_reason FROM tenants WHERE id = $1`
- Check quota: `SELECT * FROM tenant_usage WHERE tenant_id = $1 AND period = date_trunc('month', now())`
- Check for malformed payloads:
  ```
  kubectl -n kapp logs -l app=api --since=10m \
    | grep '"tenant_id":"<UUID>"' | grep '"level":"ERROR"' | jq
  ```

### 3.3.2 High latency

```
KappHighLatency fires
        │
        ▼
1. Identify slow queries:
   SELECT pid, now() - query_start AS duration, state, query
     FROM pg_stat_activity
    WHERE state = 'active' AND query_start < now() - interval '2s'
    ORDER BY duration DESC LIMIT 20;
        │
        ├─ Lock waits (wait_event_type='Lock') ──► §3.3.2a Lock contention
        │
        ├─ Long-running specific query ──► EXPLAIN ANALYZE, missing index?
        │
        └─ No long queries — but pool saturated?
                │
                ▼
2. SHOW POOLS on pgbouncer:
   psql -h pgbouncer -p 6432 pgbouncer -c 'SHOW POOLS;'
        │
        ├─ cl_waiting > 0 ──► Connection pool exhaustion — see §3.3.5
        │
        └─ Pool healthy
                │
                ▼
3. Network: latency between API ↔ pgbouncer ↔ Postgres?
   kubectl -n kapp exec deploy/api -- sh -c 'time wget -qO- http://localhost:8080/healthz'
   kubectl -n kapp exec deploy/api -- ping -c5 postgres-primary
        │
        ▼
4. Traces:
   Open Tempo/Jaeger, filter `service.name=kapp-api` and `duration>1s`,
   identify the slow span (DB / external / compute).
```

#### 3.3.2a Lock contention

```sql
-- Identify blocker → blocked chain:
SELECT blocked.pid       AS blocked_pid,
       blocked.usename   AS blocked_user,
       blocking.pid      AS blocking_pid,
       blocking.usename  AS blocking_user,
       blocked.query     AS blocked_query,
       blocking.query    AS blocking_query
FROM   pg_stat_activity blocked
JOIN   pg_stat_activity blocking
  ON   blocking.pid = ANY(pg_blocking_pids(blocked.pid));
```

Terminate the blocker only after capturing evidence:

```sql
SELECT pg_terminate_backend(<blocking_pid>);
```

### 3.3.3 Outbox lag growing

```
KappOutboxLag fires (kapp_outbox_drain_duration_seconds > 30 for 2 min)
        │
        ▼
1. Outbox depth:
   SELECT count(*) FROM events WHERE delivered_at IS NULL;
   SELECT id, tenant_id, type, created_at FROM events
    WHERE delivered_at IS NULL ORDER BY created_at LIMIT 10;
        │
        ▼
2. Worker health:
   kubectl -n kapp get pods -l app=worker
   kubectl -n kapp logs -l app=worker --since=5m \
     | grep -iE 'error|panic|timeout'
        │
        ├─ Worker crashed ──► Check OOM (§3.3.1 step 4), restart
        │
        └─ Worker running
                │
                ▼
3. NATS reachability from worker:
   kubectl -n kapp exec deploy/worker -- sh -c 'nats server ping'
        │
        ├─ NATS down ──► Restore cluster (§3.3.3a)
        │
        └─ NATS OK
                │
                ▼
4. Slow consumer? Inspect consumer stream:
   kubectl -n kapp exec deploy/nats-0 -- nats consumer info kapp.events kapp-worker
        │
        ▼
5. DLQ (events table):
   SELECT type, count(*) FROM events
    WHERE delivered_at IS NULL AND created_at < now() - interval '1 hour'
    GROUP BY type ORDER BY count DESC;
```

#### 3.3.3a NATS cluster recovery

```bash
# Identify lagging node:
kubectl -n kapp exec deploy/nats-0 -- nats server list

# If a single node is down, K8s reschedules; if quorum is lost:
kubectl -n kapp rollout restart statefulset/nats
kubectl -n kapp rollout status statefulset/nats --timeout=180s

# After recovery, force the worker to re-drain the outbox:
kubectl -n kapp delete pod -l app=worker
```

### 3.3.4 Cross-tenant data exposure (SEV-1)

**IMMEDIATE RESPONSE** — read this list end-to-end before taking any
action:

1. **Do not delete data or audit rows.**
2. Isolate the affected services:
   ```bash
   # Block public ingress on the affected cell:
   kubectl -n kapp annotate ingress/kapp-api \
     nginx.ingress.kubernetes.io/server-snippet="return 503;"
   # Keep internal access alive for forensics.
   ```
3. Revoke all active sessions on the suspected tenant pair:
   ```sql
   UPDATE sessions SET revoked_at = now()
   WHERE tenant_id IN ('<A>', '<B>') AND revoked_at IS NULL;
   ```
4. Engage the security team (PagerDuty escalation policy
   `sec-platform-oncall`).
5. Preserve evidence (§3.5).
6. Hand control to the security IC; switch to follow-the-incident
   mode.

### 3.3.5 DB connection pool exhausted

Mirrored in [ONCALL_PLAYBOOK.md §4.5](./ONCALL_PLAYBOOK.md#45-kappdbconnectionpoolexhausted)
because the alert name is `KappDBConnectionPoolExhausted`. Use that as
the canonical first-response reference; this section's job is to
guide IC decisions on remediation.

Decision: **kill stale backends** (under 5 minutes' work) vs. **scale
the pool** (under 30 minutes including reload). Choose the latter if:

- `idle in transaction` count < 10 (no leaks)
- AND API pods are saturated under load (HPA at maxReplicas)
- AND pgbouncer reports `cl_waiting > 5` for 5+ minutes

Otherwise kill stale backends first (§3.3.2a) and re-evaluate.

---

## 3.4 Communication Templates

### Initial notification (internal — Slack/KChat)

```
:rotating_light: SEV-<N>: <one-line impact>
IC: @<name>
Comms: @<name>
Tech lead: @<name>
Started: <ISO timestamp>
Channel: #kapp-incident-<YYYYMMDD>-<n>
Status page: <link or "pending">
```

### Customer-facing status page (initial)

```
Title: <Service> degraded performance / outage
Severity: <Investigating / Identified / Monitoring / Resolved>
Body: We are investigating reports of <impact>. Updates every 15
minutes.
Posted: <timestamp>
```

### Customer-facing status page (resolution)

```
Title: <Service> incident resolved
Severity: Resolved
Body: <Service> is fully restored as of <timestamp>. A summary of the
root cause and remediation steps will be published in a post-mortem
within 5 business days at <link>.
Posted: <timestamp>
```

### Post-mortem summary template

```markdown
# Post-mortem: <one-line title> — <YYYY-MM-DD>

## Summary
- Severity: SEV-<N>
- Duration: <hh:mm> (start ISO → end ISO)
- Impact: <tenants affected, requests dropped, error rate peak>

## Timeline (UTC)
- <time> — alert fires
- <time> — IC acknowledges
- <time> — mitigation step 1
- ...
- <time> — incident resolved

## Root cause
<1–3 paragraphs. Avoid blaming individuals.>

## Detection
<How we noticed. Could detection have been faster?>

## Resolution
<What we did. What worked, what didn't.>

## Action items
| #   | Owner   | Action                          | Due date    | Status |
| --- | ------- | ------------------------------- | ----------- | ------ |

## Lessons learned
- <Did our runbook hold up? What changes are needed?>
```

---

## 3.5 Evidence Preservation (Security)

For any SEV-1 with a security dimension, capture the following before
remediating. Save outputs to S3 under
`s3://kapp-evidence/<incident-id>/` with `--storage-class=STANDARD_IA`
(immutable for 7 years per [COMPLIANCE.md §10.3](./COMPLIANCE.md#103-data-retention)).

```bash
INC=incident-$(date +%Y%m%d-%H%M%S)

# Live database state
psql "$KAPP_ADMIN_DB_URL" -At -c "COPY (SELECT * FROM pg_stat_activity) TO STDOUT WITH CSV HEADER" \
  > "$INC/pg_stat_activity.csv"
psql "$KAPP_ADMIN_DB_URL" -At -c "COPY (SELECT * FROM pg_locks) TO STDOUT WITH CSV HEADER" \
  > "$INC/pg_locks.csv"

# Tenant audit log dump (RLS-bypassed admin connection)
psql "$KAPP_ADMIN_DB_URL" -At <<SQL > "$INC/audit_log_window.csv"
COPY (
  SELECT * FROM audit_log
   WHERE created_at BETWEEN now() - interval '1 hour' AND now()
   ORDER BY tenant_id, created_at
) TO STDOUT WITH CSV HEADER
SQL

# K8s state
kubectl -n kapp get all -o yaml > "$INC/k8s-state.yaml"
for d in api worker kchat-bridge; do
  kubectl -n kapp logs --since=2h --all-containers=true -l app=$d \
    > "$INC/logs-$d.jsonl"
done

# Upload (immutable)
aws s3 sync "$INC" "s3://kapp-evidence/$INC/" \
  --metadata "incident=$INC,sealed=true"
aws s3api put-object-lock-configuration --bucket kapp-evidence \
  --object-lock-configuration '{"ObjectLockEnabled":"Enabled","Rule":{"DefaultRetention":{"Mode":"COMPLIANCE","Years":7}}}'
```

### Forensic query toolkit

```sql
-- All actions by a specific user in the last 24h, across tenants
SELECT created_at, tenant_id, action, target_type, target_id, payload
FROM audit_log
WHERE actor_id = $1
  AND created_at > now() - interval '24 hours'
ORDER BY created_at;

-- Cross-tenant access attempts (must always be empty in a healthy
-- system; any non-empty result is an RLS bypass and SEV-1):
SELECT * FROM audit_log
WHERE tenant_id != $expected_tenant_id
  AND actor_id   = $suspect_user_id;

-- Audit-chain integrity check for a single tenant
-- (uses migrations/000016_audit_hash_chain.sql columns):
SELECT id, created_at,
       encode(prev_hash, 'hex')    AS stored_prev,
       encode(row_hash, 'hex')     AS stored_row,
       encode(
         digest(
           coalesce(prev_hash, '\x'::bytea) ||
           id::text::bytea ||
           created_at::text::bytea ||
           coalesce(payload::text, '')::bytea,
           'sha256'
         ), 'hex'
       )                            AS recomputed_row
FROM audit_log
WHERE tenant_id = $1
ORDER BY created_at;
-- A row where stored_row != recomputed_row is tampering.
```

A SEV-1 audit-chain break triggers `KappAuditChainBroken`
(see [ONCALL_PLAYBOOK.md §4.6](./ONCALL_PLAYBOOK.md#46-additional-alerts-to-document)).
