# Disaster Recovery & Operational Runbook (Phase K)

This runbook collects every operator procedure that touches the Kapp
control plane: backup/restore, tenant tier upgrades, cross-region
failover, encryption-key rotation, ZK Object Fabric migration, and
the chaos-drill checklist used to validate the platform.

> Convention: every command in this doc assumes you have shell access
> to a host with the Kapp binaries on PATH and `KAPP_DB_URL` /
> `KAPP_ADMIN_DB_URL` set to the cell you are operating against. All
> destructive steps include a "verify" sub-step — never proceed past
> the verify step on a failure.

---

## 1. Backup & Restore

### 1.1 Full-cell backup

```bash
kapp-backup snapshot \
  --db   "$KAPP_ADMIN_DB_URL" \
  --out  s3://kapp-backups/cell-a/$(date -u +%Y-%m-%d)/full.tar.gz
```

### 1.2 Per-tenant extract

```bash
kapp-backup extract \
  --db        "$KAPP_ADMIN_DB_URL" \
  --tenant    "$TENANT_UUID" \
  --include   "krecord,journal_entry,journal_line,audit_log,workflow_runs,sla_event_log" \
  --out       /tmp/$TENANT_UUID.tar.gz
```

The extract is a self-describing tarball with one TSV per table plus a
`manifest.json` containing the schema versions of every table. The
manifest is the contract `kapp-backup restore` validates before any
write happens — a mismatched schema fails fast.

### 1.3 Restore

```bash
kapp-backup restore \
  --src       /tmp/$TENANT_UUID.tar.gz \
  --remap     "$TENANT_UUID:$NEW_TENANT_UUID" \
  --db        "$KAPP_ADMIN_DB_URL"
```

Verify after restore:

```sql
SET LOCAL app.tenant_id = '<NEW_TENANT_UUID>';
SELECT count(*) FROM krecord;
SELECT count(*) FROM journal_entry;
SELECT count(*) FROM audit_log;
```

If row counts diverge from the manifest's `row_count` column, abort
and roll back via `kapp-backup restore --rollback`.

---

## 2. Tenant Tier Upgrade

```bash
scripts/upgrade_tier.sh \
  --tenant   "$TENANT_UUID" \
  --from     starter \
  --to       enterprise
```

Pre-flight (the script emits these on `--check`):

1. Confirm `tenants.plan = 'starter'` for the target tenant.
2. Confirm the destination plan exists in `plans` (FK guard).
3. Confirm no running `scheduled_actions` row is in `state='claimed'`
   for the tenant — a mid-flight job would be reassigned to the new
   plan's cadence.
4. Confirm a fresh per-tenant backup exists in `s3://kapp-backups/cell-*/$(date -u +%Y-%m-%d)/`.

The script:

1. Issues `UPDATE tenants SET plan = $2 WHERE id = $1`.
2. Re-seeds plan-appropriate features via the wizard's
   `seedDefaultFeatures` path so the new flags appear without a manual
   API call.
3. Re-seeds plan-appropriate retention windows
   (`seedDefaultRetentionPolicies`).
4. Provisions ZK fabric resources if the new tier's contract type
   differs from the old (`b2b_shared` → `b2b_dedicated`).

Rollback: `kapp-backup restore --tenant $TENANT_UUID --to-plan
$ORIGINAL_PLAN` from the pre-flight backup.

---

## 3. Cross-Region Failover

When the primary region is degraded:

1. **Drain the API pool** in the active region:
   ```bash
   kubectl -n kapp scale deploy/api --replicas=0
   ```
   The worker keeps draining the outbox so in-flight events finish.

2. **Promote the read replica** in the secondary region:
   ```bash
   pg_ctl promote -D $REPLICA_DATA_DIR
   ```
   Confirm: `SELECT pg_is_in_recovery();` returns `f`.

3. **DNS cutover**: flip the weighted record so 100 % of traffic hits
   the secondary region's API gateway.

4. **ZK Object Fabric cell failover**: update each tenant's
   `zk_fabric_endpoint` to the secondary fabric console:
   ```sql
   UPDATE tenants SET zk_fabric_endpoint = $1
     WHERE cell = 'us-east-1';
   ```
   The S3 store cache (LRU keyed by tenant_id) auto-invalidates on
   first access miss; explicit `Invalidate(tenantID)` is only required
   for cached connections that should drop immediately.

5. **Bring API pods up** in the secondary region:
   ```bash
   kubectl -n kapp scale deploy/api --replicas=4
   ```

6. **Verify**:
   - `GET /healthz` returns 200 from a public client.
   - `GET /api/v1/admin/isolation-audit` returns `passed=true`.
   - SLA breach scheduler in the secondary worker logs a tick within
     30 seconds.

---

## 4. Encryption Key Rotation

```bash
scripts/rotate_master_key.sh --to "$NEW_KMS_ARN"
```

The script enters the **dual-key window** by setting
`encryption.master_key_secondary` for 24 hours so any record
encrypted under the old key still decrypts. Background rebalancer
re-encrypts records lazily on read.

Verify under live load:

1. Spam-read a sample of recent records (`kapp-cli ledger list-recent`)
   and confirm zero decrypt failures in the API log.
2. After 24 hours, confirm
   `SELECT count(*) FROM krecord WHERE key_version = $OLD_VERSION` is
   zero before flipping `master_key_secondary` to NULL.

Rollback: re-set the secondary slot to the previous KMS ARN — the
dual-key window means cleartext was never lost.

---

## 5. ZK Object Fabric Migration

Phase J shipped the per-tenant ZK Object Fabric integration; Phase K
adds the **dual-write** procedure for migrating a tenant from a legacy
backend (Wasabi, raw S3) to a local fabric cell.

1. **Dual-write**: enable the `zk_fabric.dual_write` feature flag for
   the tenant. Every upload goes to both the legacy bucket and the new
   fabric bucket. Reads still come from the legacy bucket.

2. **Lazy read repair**: the worker's
   `zk_fabric.repair_sweep` action walks the legacy bucket's manifest
   and copies any object not yet present in the fabric bucket. Run
   the sweep until `repair_sweep_lag_objects` reports zero.

3. **Cutover**: flip the tenant's
   `placement_policy.placement.provider` allow-list to the fabric
   provider only. Reads now resolve through fabric; the LRU cache in
   `internal/files/zk_fabric.go` rebuilds per-tenant clients on
   demand.

4. **Decommission**: after a 7-day quiet period, disable the dual
   write and tombstone the legacy bucket. Verify with
   `kapp-cli files audit --tenant $TENANT_UUID`.

---

## 6. Chaos Drill Checklist

Run this checklist quarterly. Every item must produce a green tick or
a paged ticket.

| # | Drill                                 | Verify                                                                                                                       |
|---|---------------------------------------|------------------------------------------------------------------------------------------------------------------------------|
| 1 | `kill -9` worker mid-outbox-drain     | Replay is idempotent — `events.delivered` reaches 100 % within 60 s of restart.                                              |
| 2 | DB primary failover during traffic    | API requests recover within 30 s; the isolation auditor passes immediately after.                                            |
| 3 | Master key rotation under live load   | Dual-key window keeps decrypt success at 100 %; no error spike on encryption metrics.                                        |
| 4 | ZK fabric primary backend failure     | Reads fall through to the secondary provider in the placement policy; no 5xx on `GET /api/v1/files/{id}`.                    |
| 5 | Cross-tenant probe under chaos        | `IsolationAuditor.Run` reports `cross_tenant_probe_returns_zero` passed at peak stress.                                      |
| 6 | Retention sweep on a giant tenant     | `data_retention_sweep` deletes per-category rows within the per-run timeout; pool utilisation stays under 95 %.              |
| 7 | 5000-tenant mixed-load run            | `TestFiveThousandTenantMixedLoad` passes the SLO assertions (`APIp99 ≤ 100 ms`, `failure_rate = 0`, `pool_util ≤ 95 %`).      |

---

## Appendix A — Useful queries

```sql
-- Tenants with no recent successful audit-chain verify.
SELECT id, slug FROM tenants
 WHERE last_audit_chain_verified_at < now() - interval '7 days';

-- Pool utilisation right now.
SELECT count(*) FROM pg_stat_activity WHERE state = 'active';

-- Outbox lag.
SELECT count(*) FROM events WHERE delivered = FALSE;
```
