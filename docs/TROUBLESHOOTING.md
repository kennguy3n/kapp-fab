# Troubleshooting Guide

Symptom → cause → diagnostic → fix, for the issues operators
realistically see in production. Decision trees for active incidents
live in [INCIDENT_RESPONSE.md §3.3](./INCIDENT_RESPONSE.md#33-decision-trees-for-common-incidents);
alert-specific first-response is in [ONCALL_PLAYBOOK.md](./ONCALL_PLAYBOOK.md).
This document is for the morning after — "we have a complaint, let's
chase it down".

---

## 9.1 Common Issues Decision Matrix

| Symptom                              | Likely cause                                              | Diagnostic                                                                                                                       | Fix                                                                                                                |
| ------------------------------------ | --------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| 401 Unauthorized on all requests     | `KAPP_JWT_SECRET` mismatched between pods                  | Diff the secret across pods:<br>`kubectl -n kapp get pods -l app=api -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].env[?(@.name=="KAPP_JWT_SECRET")].valueFrom.secretKeyRef.name}{"\n"}{end}'` | Align secret in `kubectl edit secret kapp-jwt`, then rolling restart. Verify SECURITY_HARDENING.md §17.1.           |
| 403 Forbidden after plan change       | Feature flags not re-seeded                                | `SELECT * FROM tenant_features WHERE tenant_id = $1;`                                                                            | Re-run the wizard seeding: `POST /api/v1/tenants/me/plan` with the same plan id (idempotent).                       |
| 500 on record create                  | KType schema version drift                                 | `SELECT * FROM ktypes WHERE name = $1 ORDER BY version DESC;` vs. `ktype_version` on the request body                            | Re-register the KType: `POST /api/v1/ktypes` with the current definition. See `docs/KTYPE_AUTHORING_GUIDE.md`.       |
| Slow list queries (`/api/v1/records`) | Missing `tenant_id` predicate in the query plan            | `EXPLAIN (ANALYZE, BUFFERS) SELECT ... WHERE tenant_id = $1 AND ...;` — look for `Seq Scan` on `krecords_default`                | Audit the call site for the missing `WHERE tenant_id = ...` predicate. The query must always include it explicitly. |
| Worker not draining outbox            | Stale advisory lock from a dead backend                    | `SELECT * FROM pg_locks WHERE locktype='advisory';`                                                                              | Terminate the stale backend: `SELECT pg_terminate_backend(<pid>)`. Worker will re-acquire within 60 s.              |
| SSE connections dropping              | `KAPP_HTTP_WRITE_TIMEOUT` set too low for SSE              | `kubectl -n kapp get configmap kapp-config -o yaml \| grep -E 'KAPP_(HTTP\|SSE)'`                                                | Set `KAPP_SSE_ADDR` for a split listener with `KAPP_SSE_WRITE_TIMEOUT=0s` (default) — see `services/api/sse_routes.go`. |
| File upload fails                     | ZK Fabric credentials expired                              | `SELECT zk_access_key IS NOT NULL, zk_bucket FROM tenants WHERE id = $1;`                                                        | Rotate via `internal/files/store.go::PerTenantS3Store.Invalidate(tenantID)` then re-issue from the wizard.          |
| Audit chain broken                    | Concurrent writes without serialization                    | Run the audit-chain integrity check in [INCIDENT_RESPONSE.md §3.5](./INCIDENT_RESPONSE.md#35-evidence-preservation-security)     | **SEV-1.** Do NOT remediate the row. Engage security; reconstruct from the most recent valid `row_hash`.            |
| Single tenant 429 burst                | Rate-limit bucket exhausted; pl­an quota too tight        | `SELECT api_calls_per_hour FROM plan_definitions WHERE plan = (SELECT plan FROM tenants WHERE id = $1);`                          | Upgrade plan, or temporarily bump the rate-limit override via `POST /api/v1/admin/tenants/{id}/rate-limit`.         |
| KChat card actions silently fail      | `KCHAT_BASE_URL` mismatched or KChat token rotated         | `kubectl -n kapp logs -l app=kchat-bridge --since=5m \| grep -iE 'unauthorized\|forbidden\|kchat'`                                | Re-issue token in KChat; update `KCHAT_API_KEY` secret; restart `kchat-bridge`.                                    |
| Migrations dirty after deploy         | Manual psql ran a partial migration                        | `DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate version` (reports `dirty: true`)                                               | Roll forward if last migration is recoverable; otherwise `migrate force <V>` after manual cleanup — see [DATABASE_MAINTENANCE.md §5.8](./DATABASE_MAINTENANCE.md#58-schema-migration-operations). |

---

## 9.2 Diagnostic Toolkit

### Quick health check across all services

```bash
for svc in api worker kchat-bridge importer agent-tools; do
  status=$(kubectl -n kapp exec -i deploy/$svc -- wget -qO- http://localhost:8080/healthz 2>/dev/null || echo unreachable)
  echo "$svc: $status"
done
```

### Tenant isolation verification

```bash
# Admin endpoint that runs a cross-tenant access audit (planned in v0.2).
# Until shipped, the SQL below is the canonical check:
psql "$KAPP_ADMIN_DB_URL" <<SQL
SELECT 'krecords' AS t,
       (SELECT count(*) FROM pg_policies WHERE tablename = 'krecords' AND policyname = 'tenant_isolation') AS policies
UNION ALL
SELECT 'events', (SELECT count(*) FROM pg_policies WHERE tablename = 'events' AND policyname = 'tenant_isolation')
UNION ALL
SELECT 'audit_log', (SELECT count(*) FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'tenant_isolation')
UNION ALL
SELECT 'journal_lines', (SELECT count(*) FROM pg_policies WHERE tablename = 'journal_lines' AND policyname = 'tenant_isolation');
-- Each row's policy count MUST be > 0.
SQL
```

### Tenant snapshot

```bash
TENANT_ID=<uuid>
curl -s -H "Authorization: Bearer $ADMIN_TOKEN" \
  "https://api.kapp.example.com/api/v1/admin/tenants/$TENANT_ID" \
  | jq '{status, plan, cell_id, created_at, suspended_reason}'

psql "$KAPP_ADMIN_DB_URL" <<SQL
SET LOCAL app.tenant_id = '$TENANT_ID';
SELECT
  (SELECT count(*) FROM krecords)            AS records,
  (SELECT count(*) FROM events
    WHERE delivered_at IS NULL)              AS pending_events,
  (SELECT count(*) FROM files)                AS files,
  (SELECT count(*) FROM sessions
    WHERE expires_at > now())                AS active_sessions;
SQL
```

---

## 9.3 Performance Debugging Workflow

1. **Identify slow endpoint** from metrics:
   ```promql
   topk(5,
     histogram_quantile(0.99,
       sum by (path, le) (rate(kapp_request_duration_seconds_bucket[5m]))
     )
   )
   ```
2. **Get a trace** — copy a slow `request_id` from logs:
   ```logql
   {service="api"} |= "<endpoint>" | json | duration > 1.0 | line_format "{{ .request_id }}"
   ```
   Open Tempo/Jaeger and search by request_id (linked via `trace_id`).
3. **Identify the slow span**:
   - **DB span dominant** → grab the query, run
     `EXPLAIN (ANALYZE, BUFFERS)` (see
     [PERFORMANCE_TUNING.md](./PERFORMANCE_TUNING.md))
   - **External-call span dominant** → check the integration's status
     page / health dashboard
   - **Compute dominant** → enable pprof
     (`KAPP_PPROF=1`, planned; see §9.4 for the manual port-forward
     fallback)
4. **Reproduce in staging** with the load test profile from
   [LOAD_TESTING.md](./LOAD_TESTING.md).
5. **Land the fix** — and add a regression load test if the cause was
   a missing index or N+1 query.

---

## 9.4 Memory Leak Diagnosis

```bash
# 1. Enable pprof (planned env var; until shipped, use the debug port
#    in the dev deployment).
kubectl -n kapp port-forward deploy/api 6060:6060 &

# 2. Heap snapshot:
go tool pprof -http=:8090 http://localhost:6060/debug/pprof/heap
# Browse: heap-flat, inuse_objects, cum allocations.

# 3. Goroutine snapshot (detect stuck goroutines / leaked subscribers):
go tool pprof -http=:8091 http://localhost:6060/debug/pprof/goroutine

# 4. Profile under load (60 s CPU):
go tool pprof -http=:8092 http://localhost:6060/debug/pprof/profile?seconds=60
```

Common causes seen historically (see DEVELOPMENT_LOG.md):

- Unclosed `*http.Response.Body` in integration clients
- DB transactions started without `defer tx.Rollback()`
- NATS subscribers that close the message channel before
  `Unsubscribe`

---

## 9.5 Tenant-Specific Debugging

```sql
-- Recent errors involving this tenant
SELECT created_at, action, target_type, payload
FROM audit_log
WHERE tenant_id = $1
  AND action LIKE '%.error%'
ORDER BY created_at DESC LIMIT 20;

-- Resource usage snapshot (RLS-bypassed admin connection)
SELECT
  (SELECT count(*) FROM krecords
    WHERE tenant_id = $1)                                    AS records,
  (SELECT count(*) FROM events
    WHERE tenant_id = $1 AND delivered_at IS NULL)           AS pending_events,
  (SELECT count(*) FROM files
    WHERE tenant_id = $1)                                    AS files,
  (SELECT count(*) FROM sessions
    WHERE tenant_id = $1 AND expires_at > now())             AS active_sessions,
  (SELECT count(*) FROM scheduled_actions
    WHERE tenant_id = $1 AND state IN ('queued','claimed'))  AS pending_jobs;

-- Top KTypes by row count
SELECT ktype, count(*)
FROM krecords
WHERE tenant_id = $1
GROUP BY ktype ORDER BY count DESC LIMIT 10;

-- Workflow runs in a stuck state
SELECT id, workflow_name, state, updated_at
FROM workflow_runs
WHERE tenant_id = $1
  AND state IN ('running','awaiting_approval')
  AND updated_at < now() - interval '24 hours'
ORDER BY updated_at;
```

When the tenant has many pending events and the platform is healthy
overall, the cause is usually a tenant-specific consumer error.
Inspect:

```bash
kubectl -n kapp logs -l app=worker --since=1h \
  | grep "\"tenant_id\":\"<UUID>\"" \
  | jq 'select(.level == "ERROR")'
```
