-- Phase O — denormalised KRecord counter per tenant.
--
-- The Phase A QuotaEnforcer.CheckRecordCount runs
--     SELECT count(*) FROM krecords WHERE tenant_id = $1 AND status != 'deleted'
-- on every mutating request through the quota middleware. krecords is
-- the largest tenant-scoped table in the system and is range-partitioned
-- by tenant_id, so this query is forced to evaluate one sequential scan
-- per partition just to confirm "do we have headroom?" — pure write
-- amplification on the hot path. A tenant with a few million krecords
-- pays a multi-millisecond DB roundtrip on every POST / PATCH purely to
-- decide whether the next insert is allowed, even when usage is nowhere
-- near the plan limit.
--
-- This migration introduces tenant_record_counts, a single-row-per-
-- tenant counter that the record store mutates transactionally on
-- create / delete. CheckRecordCount then becomes an O(1) point lookup
-- on the tenant_id primary key — millisecond-scale becomes microsecond-
-- scale and the krecords partitions are no longer touched by the
-- quota check at all.
--
-- Design choices:
--
--   * Transactional updates. The counter UPSERT happens inside the
--     same WithTenantTx that does the krecords INSERT / soft-delete,
--     so the counter is always consistent with the source-of-truth
--     row state — committed together, rolled back together. No
--     async drain, no separate outbox, no race window.
--
--   * UPSERT on insert (not "row must exist first"). The record
--     store does not get to assume the counter row already exists —
--     a brand-new tenant has never had a krecord, so its counter row
--     does not exist either. INSERT … ON CONFLICT DO UPDATE handles
--     both first-insert and steady-state in one statement.
--
--   * CHECK (record_count >= 0). The decrement path subtracts one
--     per soft-delete, but the record store's Delete already returns
--     ErrNotFound on a row that is already in status='deleted' (so
--     re-delete is a no-op and never produces a second decrement).
--     The CHECK is defense-in-depth: if anyone ever wires a new
--     deletion path that double-decrements, the transaction fails
--     loudly at COMMIT instead of letting the counter drift negative.
--
--   * RLS isolation matching every other tenant-scoped table — the
--     daily reconciliation handler runs under dbutil.WithTenantTx so
--     RLS continues to be the final guarantor of cross-tenant
--     isolation.
--
--   * Backfill from the existing krecords data. The table starts
--     non-empty for already-onboarded tenants so production cutover
--     does not have to wait for the reconciliation handler's first
--     tick. The backfill counts active rows (status != 'deleted')
--     just like CheckRecordCount, so the seeded value exactly
--     matches what the old O(n) query would have returned at
--     migration time.

CREATE TABLE IF NOT EXISTS tenant_record_counts (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    record_count BIGINT NOT NULL DEFAULT 0 CHECK (record_count >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id)
);

ALTER TABLE tenant_record_counts ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_record_counts_isolation ON tenant_record_counts;
CREATE POLICY tenant_record_counts_isolation ON tenant_record_counts
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_record_counts TO kapp_app;

-- One-time backfill. Counts active (non-deleted) krecords per tenant
-- so the post-migration first request sees an accurate value without
-- waiting for the reconciliation handler's first tick. Uses INSERT
-- ON CONFLICT so a re-run of the migration after the handler has
-- already touched the rows is a no-op on the backfilled-tenant set
-- and an UPDATE-to-current-count on tenants that have written since.
INSERT INTO tenant_record_counts (tenant_id, record_count, updated_at)
SELECT tenant_id, COUNT(*), now()
  FROM krecords
 WHERE status != 'deleted'
 GROUP BY tenant_id
ON CONFLICT (tenant_id) DO UPDATE SET
    record_count = EXCLUDED.record_count,
    updated_at   = EXCLUDED.updated_at;

COMMENT ON TABLE tenant_record_counts IS
    'Denormalised live KRecord counter per tenant. Maintained transactionally by internal/record.PGStore.Create / Delete / BulkDelete. Powers QuotaEnforcer.CheckRecordCount in O(1) instead of O(n) per partition. Reconciled daily by platform.RecordCountReconciler against the krecords source of truth.';
COMMENT ON COLUMN tenant_record_counts.record_count IS
    'Active (status != ''deleted'') row count for the tenant. Never negative — the CHECK constraint backs up the record-store accounting. The daily reconciliation handler rewrites this to match krecords if drift is detected (e.g. after a direct-SQL data fix that bypassed the store).';
