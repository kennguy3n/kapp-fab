-- Down migration for 000091. Reverses the rules-depth columns, the
-- transfer-pair table + 'transfer' status, and the duplicate_of flag.

-- ---------------------------------------------------------------------------
-- 3. Inter-account transfer pairs (drop first: nothing depends on it)
-- ---------------------------------------------------------------------------
DROP TABLE IF EXISTS bank_transfer_pairs;

-- ---------------------------------------------------------------------------
-- 2. Duplicate flag + 'transfer' status
-- ---------------------------------------------------------------------------

-- Any line auto-resolved as a transfer reverts to unreconciled so it still
-- satisfies the narrowed status CHECK re-added below.
UPDATE bank_transactions SET status = 'unreconciled' WHERE status = 'transfer';

ALTER TABLE bank_transactions
    DROP CONSTRAINT IF EXISTS bank_transactions_status_check;
ALTER TABLE bank_transactions
    ADD CONSTRAINT bank_transactions_status_check
    CHECK (status IN ('unreconciled', 'matched', 'ignored', 'voided'));

DROP INDEX IF EXISTS bank_transactions_duplicate_of_idx;
ALTER TABLE bank_transactions
    DROP CONSTRAINT IF EXISTS bank_transactions_duplicate_of_fkey;
ALTER TABLE bank_transactions
    DROP COLUMN IF EXISTS duplicate_of;

-- ---------------------------------------------------------------------------
-- 1. Bank-rules engine depth
-- ---------------------------------------------------------------------------

-- Restore the legacy single-condition NOT NULL invariant. Backfill any
-- compound-only rule (which left the legacy columns NULL) with an inert
-- placeholder so the constraint can be re-applied; such rows were created
-- against the newer schema and are not expected to round-trip.
UPDATE bank_reconciliation_rules
   SET condition_type = COALESCE(condition_type, 'description_contains'),
       condition_value = COALESCE(condition_value, '')
 WHERE condition_type IS NULL OR condition_value IS NULL;

ALTER TABLE bank_reconciliation_rules
    ALTER COLUMN condition_type SET NOT NULL;
ALTER TABLE bank_reconciliation_rules
    ALTER COLUMN condition_value SET NOT NULL;

ALTER TABLE bank_reconciliation_rules
    DROP CONSTRAINT IF EXISTS bank_reconciliation_rules_condition_match_check;
ALTER TABLE bank_reconciliation_rules
    DROP COLUMN IF EXISTS condition_match;
ALTER TABLE bank_reconciliation_rules
    DROP COLUMN IF EXISTS conditions;
ALTER TABLE bank_reconciliation_rules
    DROP COLUMN IF EXISTS target_tax_code;
