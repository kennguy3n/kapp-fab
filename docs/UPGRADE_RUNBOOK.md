# Upgrade and Rollback Procedures

Three release shapes are supported:

1. **Standard release** — zero-downtime, rolling deploy of backward-
   compatible binary + idempotent migrations.
2. **Breaking release** — requires a maintenance window.
3. **Canary release** — a portion of traffic on the new version
   before promotion.

Cross-references:

- DB migration safety guarantees: [DATABASE_MAINTENANCE.md §5.8](./DATABASE_MAINTENANCE.md#58-schema-migration-operations)
- DR fallback: [DR_RUNBOOK.md](./DR_RUNBOOK.md)
- Pre-deploy verification: [SECURITY_HARDENING.md §17.1](./SECURITY_HARDENING.md#171-pre-deployment-checklist)
- CI gates: `.github/workflows/ci.yml`, `migration-numbering-check.yml`,
  `migration-rls-check.yml`, `api-versioning-check.yml`.

---

## 7.1 Standard Release (Zero-Downtime)

**Preconditions** (CI gates these; release is blocked if any fail):

- Every migration in this release is backward-compatible:
  - New columns are `NULL`-able or have a default
  - New tables only (no drops)
  - Index creation uses `CONCURRENTLY`
  - Tenant-scoped tables have RLS policies (the
    `migration-rls-check.yml` workflow enforces this)
- Old and new binary versions can coexist reading the same DB schema
  (canary-tested in CI integration suite).
- `api-versioning-check.yml` confirms no incompatible OpenAPI change.

**Procedure.**

```bash
# 1. Apply migrations (idempotent — re-runs are a no-op):
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate up

# Verify:
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate version
# Should print: version=<expected N>

# 2. Roll API:
kubectl -n kapp set image deploy/api api=ghcr.io/kennguy3n/kapp-fab/api:v0.1.1
kubectl -n kapp rollout status deploy/api --timeout=300s

# 3. Roll Worker:
kubectl -n kapp set image deploy/worker worker=ghcr.io/kennguy3n/kapp-fab/worker:v0.1.1
kubectl -n kapp rollout status deploy/worker --timeout=300s

# 4. Roll KChat Bridge:
kubectl -n kapp set image deploy/kchat-bridge bridge=ghcr.io/kennguy3n/kapp-fab/kchat-bridge:v0.1.1
kubectl -n kapp rollout status deploy/kchat-bridge --timeout=300s

# 5. Deploy frontend (Nginx serving static React build):
kubectl -n kapp set image deploy/web web=ghcr.io/kennguy3n/kapp-fab/web:v0.1.1
kubectl -n kapp rollout status deploy/web --timeout=300s

# 6. Smoke verify:
curl -s https://api.kapp.example.com/api/v1/ | jq
curl -s -H "Authorization: Bearer $SMOKE_TOKEN" \
  https://api.kapp.example.com/api/v1/krecords?ktype=crm.deal&limit=1 | jq

# 7. Watch error rate for 30 minutes:
#   - p99 latency stable (within 10% of pre-deploy)
#   - kapp_request_total{status=~"5.."} rate steady
#   - No outbox lag
```

**Verify.**

| Signal                                           | Expected after deploy |
| ------------------------------------------------ | --------------------- |
| `GET /api/v1/` returns build info                | New version tag       |
| Error rate (`kapp_request_total{status=~"5.."}`) | < 0.1 %               |
| p99 latency                                       | Within 10 % of baseline |
| Outbox depth                                      | Drained within 1 min    |
| Audit log writes                                  | Hash-chain intact       |

---

## 7.2 Rollback Procedure

Trigger conditions (any one):

- Error rate > 1 % for 10 minutes after deploy
- p99 latency > 2× baseline for 10 minutes
- Cross-tenant data exposure suspected (immediate, regardless of
  metrics)

```bash
# Immediate (binary rollback):
kubectl -n kapp rollout undo deploy/api
kubectl -n kapp rollout undo deploy/worker
kubectl -n kapp rollout undo deploy/kchat-bridge
kubectl -n kapp rollout undo deploy/web

# Verify previous version:
kubectl -n kapp rollout history deploy/api
curl -s https://api.kapp.example.com/api/v1/ | jq '.version'
```

Migration rollback (only if the migration itself caused the issue —
binary rollback usually suffices because migrations are written to be
backward-compatible):

```bash
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate down 1
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate version
```

If `down` fails because the migration has no `.down.sql` companion:

1. Engage the on-call platform lead.
2. Restore the affected tables from the most recent backup (see
   [DR_RUNBOOK.md §1.3](./DR_RUNBOOK.md#13-restore)).
3. Force the schema_migrations row to the previous version:
   ```bash
   DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate force <prev_V>
   ```
4. Open a SEV-2 retrospective.

---

## 7.3 Breaking Change Release (Maintenance Window)

For changes that cannot be made backward-compatible (e.g. drop a
column, change semantics of an enum, restructure a foreign key).

**Pre-maintenance.**

- T-72h: Schedule announcement to all tenants (status page,
  in-product banner via `apps/web/components/MaintenanceBanner.tsx`).
- T-24h: Pre-maintenance backup
  (`go run ./services/kapp-backup snapshot --db "$KAPP_ADMIN_DB_URL"
   --out s3://kapp-backups/maintenance-$(date -u +%Y-%m-%dT%H-%M-%S)/`).
- T-1h: Drain in-flight outbox events
  (`SELECT count(*) FROM events WHERE delivered_at IS NULL` must
  reach 0; if not, escalate).

**Maintenance.**

```bash
# 1. Enable maintenance mode at the load balancer:
kubectl -n kapp annotate ingress/kapp-api \
  nginx.ingress.kubernetes.io/server-snippet="return 503 'maintenance';"

# 2. Scale services to 0:
kubectl -n kapp scale deploy/api --replicas=0
kubectl -n kapp scale deploy/worker --replicas=0
kubectl -n kapp scale deploy/kchat-bridge --replicas=0

# 3. Run breaking migrations:
DB_URL="$KAPP_ADMIN_DB_URL" go run ./cmd/migrate up

# 4. Deploy new binaries (image push, then scale back up):
kubectl -n kapp set image deploy/api api=ghcr.io/kennguy3n/kapp-fab/api:v0.2.0
kubectl -n kapp set image deploy/worker worker=ghcr.io/kennguy3n/kapp-fab/worker:v0.2.0
kubectl -n kapp set image deploy/kchat-bridge bridge=ghcr.io/kennguy3n/kapp-fab/kchat-bridge:v0.2.0

kubectl -n kapp scale deploy/api --replicas=4
kubectl -n kapp scale deploy/worker --replicas=2
kubectl -n kapp scale deploy/kchat-bridge --replicas=2

# 5. Smoke test before disabling maintenance mode:
kubectl -n kapp port-forward svc/api 18080:8080 &
curl -s -H "Authorization: Bearer $SMOKE_TOKEN" http://localhost:18080/api/v1/krecords?ktype=crm.deal&limit=1 | jq

# 6. Disable maintenance mode:
kubectl -n kapp annotate ingress/kapp-api \
  nginx.ingress.kubernetes.io/server-snippet- --overwrite

# 7. Monitor for 30 minutes (same signals as §7.1).
```

---

## 7.4 Canary Deployments

Deploy v0.2.0 to a small subset of traffic via the Nginx /Istio canary
header (or a `canary=1` cookie). For Nginx ingress:

```yaml
# deploy/k8s/ingress-canary.yaml
metadata:
  annotations:
    nginx.ingress.kubernetes.io/canary: "true"
    nginx.ingress.kubernetes.io/canary-weight: "10"
spec:
  rules:
    - host: api.kapp.example.com
      http:
        paths:
          - backend:
              service:
                name: api-canary
                port:
                  number: 8080
```

Procedure:

```bash
# 1. Apply canary deployment (single replica, new image):
kubectl -n kapp apply -f deploy/k8s/canary.yaml
kubectl -n kapp rollout status deploy/api-canary --timeout=120s

# 2. Set weight to 10%:
kubectl -n kapp annotate ingress/kapp-api-canary \
  nginx.ingress.kubernetes.io/canary-weight=10 --overwrite

# 3. Monitor canary metrics for 15 minutes:
#   - kapp_request_total{instance=~"api-canary.*",status=~"5.."} / total
#   - p99 latency on canary vs. baseline
#   - No new error patterns in canary logs

# 4a. PROMOTE: set production image, remove canary:
kubectl -n kapp set image deploy/api api=ghcr.io/kennguy3n/kapp-fab/api:v0.2.0
kubectl -n kapp rollout status deploy/api --timeout=300s
kubectl -n kapp delete -f deploy/k8s/canary.yaml

# 4b. ABORT: scale canary to 0, set weight to 0:
kubectl -n kapp scale deploy/api-canary --replicas=0
kubectl -n kapp annotate ingress/kapp-api-canary \
  nginx.ingress.kubernetes.io/canary-weight=0 --overwrite
```

---

## 7.5 Database Migration Safety Checklist

Every migration PR must pass this list. CI enforces the ones tagged
**[ci]**.

- [ ] **[ci]** Migration file numbered contiguously from the previous
      (gap check via `scripts/check_migration_numbering.sh`).
- [ ] **[ci]** Tenant-scoped tables enable RLS and define
      `tenant_isolation` policies.
- [ ] Migration is **idempotent** (`IF NOT EXISTS`,
      `ADD COLUMN IF NOT EXISTS`, `ON CONFLICT DO NOTHING`).
- [ ] No `ALTER TABLE ... ADD COLUMN ... NOT NULL` without a
      `DEFAULT`. Postgres rewrites the entire table otherwise.
- [ ] No `DROP COLUMN`. Soft-deprecate: rename to `<col>_deprecated`
      first, then drop in a separate breaking release months later.
- [ ] Index creation uses `CREATE INDEX CONCURRENTLY`.
- [ ] Set explicit lock timeout to prevent table-rewrites holding
      `AccessExclusiveLock` indefinitely:
      ```sql
      SET LOCAL lock_timeout = '5s';
      SET LOCAL statement_timeout = '60s';
      ```
- [ ] Tested against a production-sized copy (`pg_dump` from the
      replica). Execution time documented in the migration's
      header comment.
- [ ] **Rollback migration** (`migrations/000XXX_<name>.down.sql`)
      exists and has been tested by `go run ./cmd/migrate down 1`.
- [ ] CHANGELOG entry mentions any operational follow-up
      (e.g. "vacuum krecords_p005 after deploy").

A migration that fails any of these in production triggers
`KappMigrationFailed` (see
[ONCALL_PLAYBOOK.md §4.6](./ONCALL_PLAYBOOK.md#46-additional-alerts-to-document))
and blocks further deploys until resolved.
