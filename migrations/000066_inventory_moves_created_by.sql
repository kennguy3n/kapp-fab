-- Phase N9d follow-up — persist the actor who recorded each
-- inventory_move.
--
-- The `Move` Go struct in internal/inventory/inventory.go has carried a
-- `CreatedBy uuid.UUID` field for a long time and every caller that
-- writes a move populates it (e.g. CycleCountStore.PostSession threads
-- the operator's user id through to the variance move; ReverseMove
-- stamps the actor on the contra-entry; the auditor reads `CreatedBy`
-- when building outbox events). But the corresponding column was never
-- added to `inventory_moves`, so the INSERT statements in
-- recordMoveInTx / RecordTransfer / ReverseMove silently dropped the
-- value. Downstream consumers that observe the outbox event still see
-- the actor (the event payload is built from the in-memory Move
-- struct), but anything that reads `inventory_moves` directly — for
-- example a reconciliation report or an audit query — saw no record of
-- who created the row.
--
-- Adding the column is additive and backward-compatible:
--
--   * Nullable so existing rows (and any out-of-band INSERTs that
--     pre-date this migration) don't need backfill.
--   * New writes populate it. Reads COALESCE NULL → nil UUID the same
--     way internal/inventory/store.go already does for
--     `inventory_batches.created_by`, so callers get a non-nil
--     uuid.UUID value with the documented "unknown actor" sentinel.
--   * No index — the column is for audit display, not for query
--     filtering. Adding one would just be write amplification on the
--     hot path.

ALTER TABLE inventory_moves
    ADD COLUMN IF NOT EXISTS created_by UUID;

COMMENT ON COLUMN inventory_moves.created_by IS
    'User id (auth subject) of the actor who recorded this move. NULL on rows that pre-date migration 000066 or were written by background jobs without a human actor. New writes populate it from Move.CreatedBy.';
