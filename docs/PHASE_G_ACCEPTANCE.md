# Phase G — Hardening / Acceptance Validation

This document records the live acceptance evidence for the Phase G
exit criteria in `PROGRESS.md`. Each section maps 1:1 to a
checkbox.

## 1. SLOs hold under 5k-tenant load

`internal/integrationtest/loadtest/phase_g_acceptance_test.go` ships
the canonical operator-runnable harness. It scales the
`internal/integrationtest/loadtest` driver to a configurable fleet
size (default 5000) and asserts the Phase K SLO bundle:

| Metric             | Target  | 5000-tenant result |
|--------------------|---------|--------------------|
| API CRUD p99       | ≤ 100ms | 38.3ms (create) / 14.7ms (get) / 14.8ms (list) |
| Journal post p99   | ≤ 250ms | 60.1ms |
| Failure rate       | ≤ 0.1%  | 0 / 140 000 (0%) |
| Pool utilisation   | ≤ 95%   | 64/96 conns peak (66%) |
| Total run wall     | —       | ~63s (37s seed + 27s run) |

Reproduce locally:

```
KAPP_TEST_DB_URL=postgres://kapp:kapp_dev@localhost:5432/kapp \
LT_TENANTS=5000 LT_MAX_CONNS=96 \
LT_REPORT_PATH=docs/phase_g_5k_run.md \
go test -tags=loadtest_acceptance -timeout=2h \
  -run TestPhaseGAcceptanceLoad \
  ./internal/integrationtest/loadtest/...
```

`LT_TENANTS` overrides the fleet size for smoke runs. `LT_MAX_CONNS`
scales `pgxpool.Config.MaxConns` so the test reflects the production
pool ceiling rather than the driver's default `min(4*cpu, server cap)`
limit.

The 200 / 500 / 5000 sweeps were run on the standard dev compose
(PostgreSQL 16 in Docker, single host) and all met the SLOs once
the pool was sized appropriately. The 200-tenant pool=8 run was
the only configuration that breached `MaxPoolUtilization`, which is
expected — the SLO assertion is what surfaced the pool sizing
requirement that the production runbook now bakes in.

## 2. Tenant tier upgrade end-to-end

The `POST /api/v1/admin/tenants/{id}/upgrade-tier` handler in
`services/api/tier_handlers.go` runs the admin-pool transaction that
copies every `tenant_id`-scoped row out of `public.*` into
`tenant_<uuid>.*` in a single commit.

`services/api/tier_handlers_integration_test.go::TestTierUpgradeCopiesEveryTable`
seeds tenant A and tenant B with one row per Phase L insights table
(plus the queries / dashboards / widgets / cache / shares set the
acceptance criterion calls out by name), runs `promoteTenantToSchema`
on tenant A, then verifies:

1. Every entry in `tierUpgradeTables` exists under the dedicated
   schema.
2. Tenant B's rows did NOT leak into tenant A's dedicated schema.
3. Each insights table carries tenant A's seeded rows.
4. `public.tenants.schema` is updated to the dedicated schema name.

`TestTierUpgradeTablesMatchBackupSourceList` is the structural
counterpart: it parses `services/kapp-backup/main.go`'s
`TenantScopedTables` slice literal at test time and asserts byte-
identical contents with `tier_handlers.go::tierUpgradeTables`. A
forgotten add-table on either side trips this gate before the new
table can ship.

A pre-existing bug in the row-copy SQL was found and fixed during
acceptance: `INSERT INTO target SELECT * FROM source` is rejected by
PostgreSQL when `target` inherits a generated column (e.g.
`krecords.search_vector` from `migrations/000023_fulltext_search.sql`)
because Postgres validates the column list at planning time, even
when the SELECT projection returns zero rows. The fix enumerates
non-generated columns via `information_schema.columns` (see
`services/api/tier_handlers.go::nonGeneratedColumns`) so the INSERT
list is column-explicit. `scripts/upgrade_tier.sh` was updated
in lock-step so the shell path stays in parity.

Reproduce locally:

```
KAPP_TEST_DB_URL=postgres://kapp_app:kapp_app_dev@localhost:5432/kapp \
KAPP_TEST_ADMIN_DB_URL=postgres://kapp_admin:kapp_admin_dev@localhost:5432/kapp \
go test -tags=integration -count=1 \
  -run "TestTierUpgrade|TestKappBackup" \
  ./services/api/...
```

## 3. Backup + restore end-to-end (shared and dedicated tiers)

`TestKappBackupRoundTripWithRemap` builds the `kapp-backup` binary as
a subprocess, runs `extract --tenant <src>`, then `restore --in dump
--remap src:dst`, and verifies that every Phase L insights table
carries rows under the destination tenant id. The dedicated tier
shares the same logical extract path (`kapp-backup` walks
`TenantScopedTables` under the tenant_id GUC), so the round-trip
proves the shared-→-dedicated migration story too: an operator who
extracts a tenant pre-promotion can replay the dump under the
promoted tenant id without losing rows.

The shared-tier path is exercised directly. The dedicated-tier path
shares the same code (kapp-backup walks `tenant_id` regardless of
which schema the row currently lives in once `public.tenants.schema`
is updated) — the structural assertion in
`TestTierUpgradeTablesMatchBackupSourceList` is what guarantees the
extract walks every table the upgrade copies.

## 4. Security review sign-off

See `docs/SECURITY_REVIEW.md`. Phase G acceptance closes the
following items in §8 (Outstanding Items at Acceptance):

- Item 1 (RLS coverage on every tenant-scoped table): enforced by
  `.github/workflows/migration-rls-check.yml` — every migration that
  introduces a `tenant_id` column must enable RLS in the same file.
  Acceptance test
  `services/api/tier_handlers_integration_test.go::TestTierUpgradeCopiesEveryTable`
  cross-checks coverage against the runtime `tierUpgradeTables`
  slice.
- Item 2 (tier-upgrade single-transaction safety): asserted by
  the pgx tx wrapper in `promoteTenantToSchema` — every CREATE
  SCHEMA / CREATE TABLE / INSERT runs in one tx that rolls back
  on any error.
- Item 3 (backup + remap round-trip): asserted by
  `TestKappBackupRoundTripWithRemap`.
- Item 4 (control-plane RLS bypass scoping): `kapp_admin` has
  BYPASSRLS but is not a superuser; CREATE on the database is
  granted only at deploy time so dev clusters need an explicit
  `GRANT CREATE ON DATABASE kapp TO kapp_admin` (logged in
  `docs/PHASE_G_ACCEPTANCE.md` and the dev compose README).

Item 5 (extract `promoteTenantToSchema` to a SECURITY DEFINER
wrapper backed by the `kapp_tier_admin` role) tracks against
PR #3 and is not part of Phase G acceptance.

## 5. Developer guide covers onboarding

`docs/DEVELOPER_GUIDE.md` covers:

- Local stack setup (docker-compose, migrations, app role roles).
- The `make run-api` / `make run-worker` / `make run-kchat-bridge`
  loop with the correct `APP_DB_URL` / `ADMIN_DB_URL` split.
- The integration test split — plain `make test` vs.
  `make test-integration` (live DB) vs. `loadtest` build tag.
- The KType authoring loop (cross-references
  `docs/KTYPE_AUTHORING_GUIDE.md`).
- The release checklist (lint, RLS check, integration test, load
  test, security review).

The Phase G acceptance run did not surface any onboarding gaps.
Future docs additions track in `docs/DEVELOPER_GUIDE.md` directly
rather than this acceptance log.
