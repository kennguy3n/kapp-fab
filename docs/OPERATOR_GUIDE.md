# Operator Guide

This guide is for operators running Kapp in production (or
production-like) environments. It covers deployment, environment
configuration, backup / restore, monitoring, multi-tenancy
operations, and the disaster-recovery process.

## Architecture in one paragraph

Kapp is a Go monorepo plus a React frontend. Three Go services
(`services/api`, `services/worker`, `services/kchat-bridge`) talk
to a shared PostgreSQL database (RLS-enforced multi-tenancy) and
NATS (events). Every workload — record CRUD, scheduled jobs,
event delivery, KChat actions — flows through these three
binaries. There is no per-tenant process or container. Tenant
isolation is the database's responsibility: every query runs
under `SET LOCAL app.tenant_id = …` against
`POLICY tenant_isolation USING (tenant_id = …)`.

## Deployment

### Local / development

```sh
docker compose up -d              # postgres, nats, mailhog, etc.
make migrate                      # apply every migrations/*.sql in order
make build                        # produce ./bin/{api,worker,kchat-bridge}
make run-api &                    # serves on :8080
make run-worker &
make run-kchat-bridge &
```

Frontend:

```sh
npm install
npm run dev -w @kapp/web          # vite dev server on :5173
```

### Production (single-binary)

The three Go services are statically linked. Drop them on a host
behind your load balancer, run them under systemd / nomad / k8s,
and point them at the production database via `DB_URL`. There is
nothing else to deploy. Migrations are run separately with
`scripts/migrate.sh` (or `make migrate`) before the new binaries
roll out — every migration is idempotent.

### Environment variables

Every service reads its config from environment variables. A
working `.env.example` ships at the repo root; the canonical list
is on `internal/platform/config.go`. The minimum a production
deployment must set:

| Variable | Purpose |
| --- | --- |
| `DB_URL` | App-role DSN (`kapp_app`); RLS-enforced. |
| `ADMIN_DB_URL` | Admin-role DSN (`kapp_admin`); BYPASSRLS for cross-tenant ops. |
| `NATS_URL` | NATS connection string. |
| `KAPP_KCHAT_BRIDGE_URL` | Where the worker posts cards / DMs. |
| `KAPP_FORMS_BASE_URL` | Public host for anonymous form submission links. |
| `KAPP_SMTP_HOST` / `KAPP_SMTP_PORT` / `KAPP_SMTP_USER` / `KAPP_SMTP_PASS` | Outbound email; absence = no-op. |
| `KAPP_FILES_S3_BUCKET` (etc.) | ZK Object Fabric bucket for file uploads. |

## Backup and restore

`services/kapp-backup` is the per-tenant export / restore tool.

```sh
# Extract a single tenant to a JSONL file
go run ./services/kapp-backup extract \
    --tenant <tenant_id> \
    --out tenant-<tenant_id>.jsonl

# Restore into a different cell, optionally remapping the tenant id
go run ./services/kapp-backup restore \
    --in tenant-<tenant_id>.jsonl \
    --remap <src_tenant_id>:<dst_tenant_id>
```

Backups are written one row per JSON line so they stream into
piped consumers without holding the whole dump in memory. The
extractor walks every tenant-scoped table declared in
`scripts/upgrade_tier.sh`; the test
`services/kapp-backup/main_test.go::TestTenantScopedTablesCovered`
fails CI when a new tenant-scoped table is added without being
registered.

For full-cell backups, run the underlying Postgres `pg_dump` on a
read replica.

## Tier upgrade

`scripts/upgrade_tier.sh` migrates a single tenant from a shared
DB cell into a dedicated cell (or back). It is idempotent: re-running
after a partial failure resumes from the last completed table.

```sh
scripts/upgrade_tier.sh \
    --tenant <tenant_id> \
    --src-db postgres://… \
    --dst-db postgres://…
```

## Monitoring

Each service exposes Prometheus metrics on `/metrics`. The metrics
that matter most:

| Metric | Why |
| --- | --- |
| `kapp_api_request_duration_seconds` | Tail latency per route. |
| `kapp_api_request_errors_total{code="5xx"}` | Server-side failure rate. |
| `kapp_outbox_lag_events` | Worker drain backlog. If this grows, NATS or DB IO is starved. |
| `kapp_scheduled_actions_due_total` | Scheduler backlog. |
| `kapp_export_jobs_pending_total` | Phase K export queue depth. |
| `kapp_lru_cache_evictions_total{cache="zk_fabric"}` | Per-tenant S3 store eviction churn. |
| `kapp_db_pool_in_use_connections` | DB pool saturation. |
| `pg_stat_user_tables.n_live_tup{table=tenants}` | Active tenant count. |

Recommended alerts:

* outbox lag > 1000 events for 5 minutes → page on-call;
* 5xx rate > 1% for 5 minutes → page on-call;
* DB pool in-use > 80% of max for 10 minutes → scale out replicas;
* migration job last status != success → page release.

Standard logging is structured JSON to stdout; ship to your log
sink of choice.

## Multi-tenancy operations

Tenant lifecycle is owned by the control plane:

* **Create** — `tenant.WizardStore.Create` provisions the row,
  seeds default features, retention policies, and scheduled
  actions. Exposed via `POST /api/v1/admin/tenants` (admin scope).
* **Suspend** — `tenant.SetStatus` flips `tenants.status` to
  `suspended`. The session middleware revokes active sessions and
  every subsequent API call short-circuits with 423 Locked.
* **Archive** — `tenant.SetStatus` to `archived`. Read-only
  through the admin API; users cannot log in.
* **Delete** — irreversible. Drops every tenant-scoped row via
  `ON DELETE CASCADE`; the kapp-backup extract is your only
  recovery path.

### Feature flags

Per-tenant `tenant_features` overrides the defaults set by the
plan. Toggle through the admin API or directly in SQL:

```sql
UPDATE tenant_features
   SET enabled = TRUE
 WHERE tenant_id = <id> AND feature = 'multi_currency';
```

Features are enforced both server-side (middleware) and client-side
(feature gates in `apps/web`).

### Quotas

`tenant_quotas` caps per-tenant `api_calls_per_day`,
`storage_bytes`, and `krecord_count`. The quota middleware (
`internal/platform/quota.go`) hard-rejects with 429 once the
running counter exceeds the cap. Reset is daily at 00:00 UTC.

### Encryption key rotation

Per-KType field-level encryption uses
`tenant_encryption_keys.active_kid`. Rotate by:

```sh
scripts/rotate_tenant_keys.sh --tenant <id>
```

The script issues a new active key and re-encrypts the affected
records under the new key. The previous key is retained until
every record has been migrated, after which it is marked
`status = 'retired'` and decrypted-only.

## Disaster recovery

The full DR procedure is in [DR_RUNBOOK.md](./DR_RUNBOOK.md). The
30-second summary:

1. Promote the warm standby. RPO ≤ 60 s.
2. Update DNS for the API / KChat-bridge service hosts.
3. Re-run `make migrate` against the promoted DB.
4. Restart all three Go services pointing at the new DB.
5. Verify `GET /healthz` is green and the outbox is draining.

Backups are run hourly. Restores are tested monthly via the
`scripts/dr-restore-drill.sh` script.
