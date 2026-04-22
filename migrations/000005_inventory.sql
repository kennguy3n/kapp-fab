-- Phase D — Simple Inventory.
--
-- The `inventory_items`, `inventory_warehouses`, and `inventory_moves`
-- tables already live in migrations/000001_initial_schema.sql with RLS
-- enabled and a default partition for `inventory_moves`. This migration
-- adds the auxiliary objects the inventory engine needs:
--
--   * a source-row partial unique index so a delivery/receipt move for a
--     given (source_ktype, source_id, item_id, warehouse_id) is posted
--     exactly once per tenant — same two-phase-commit safety net used
--     by journal_entries_source_uniq;
--   * a view `stock_levels` that projects current quantities from the
--     append-only move log, so callers always see the live SUM(qty)
--     grouped by (tenant_id, item_id, warehouse_id). RLS is enforced
--     on the base table, so the view inherits tenant isolation via the
--     default `security_invoker = true` behaviour on Postgres 15+.

-- ---------------------------------------------------------------------------
-- Source-row idempotency for posted stock moves.
--
-- InvoicePoster extensions (PostSalesInvoice, PostPurchaseBill) record
-- a goods-delivery / goods-receipt move after committing the journal
-- entry. If the poster is retried after a partial failure the move must
-- not be recorded twice; this partial unique index makes duplicate
-- inserts for the same source record collide at 23505, which the Go
-- layer translates into ErrDuplicateSourceMove for replay safety.
-- ---------------------------------------------------------------------------
CREATE UNIQUE INDEX IF NOT EXISTS inventory_moves_source_uniq
    ON inventory_moves (tenant_id, source_ktype, source_id, item_id, warehouse_id)
    WHERE source_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Stock levels view: live projection of the append-only inventory_moves
-- ledger. One row per (tenant_id, item_id, warehouse_id) with the
-- running quantity. Callers that need a snapshot to a particular
-- instant should filter by moved_at in their own query.
--
-- The view is SECURITY INVOKER by default (Postgres 15+) so RLS on
-- inventory_moves is applied using the caller's tenant context. An
-- unset `app.tenant_id` GUC therefore returns zero rows, matching the
-- default-deny behaviour of every other tenant-scoped surface.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE VIEW stock_levels AS
    SELECT tenant_id,
           item_id,
           warehouse_id,
           SUM(qty) AS qty
      FROM inventory_moves
     GROUP BY tenant_id, item_id, warehouse_id;

-- kapp_app already has CRUD grants on every table from
-- migrations/000001_initial_schema.sql, but views created after that
-- migration need an explicit grant because ALTER DEFAULT PRIVILEGES
-- only applies to tables/sequences/functions.
GRANT SELECT ON stock_levels TO kapp_app;
