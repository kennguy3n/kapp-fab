-- Session 15 follow-up: support provider-driven mutation/void of synced
-- bank statement lines.
--
-- Plaid's /transactions/sync delivers `modified` (a posted transaction
-- whose amount/date/description changed after first posting, e.g. a
-- returned payment) and `removed` (a transaction the provider retracted)
-- alongside `added`. Applying a `removed` line must take the statement
-- row out of the reconciliation surface without deleting it: the row is
-- still referenced by bank_match_suggestions (FK) and by the tamper-
-- evident audit trail, and operators need to see that the bank retracted
-- it (distinct from a human choosing to `ignore` a line).
--
-- This widens the bank_transactions.status CHECK (from migration 000011)
-- with a dedicated 'voided' state. Widening a CHECK is backward
-- compatible — every pre-existing row already satisfies the new, larger
-- set — and needs no data backfill. The latest prior migration is 000082
-- (bank_feed); this is 000083.

ALTER TABLE bank_transactions
    DROP CONSTRAINT IF EXISTS bank_transactions_status_check;

ALTER TABLE bank_transactions
    ADD CONSTRAINT bank_transactions_status_check
    CHECK (status IN ('unreconciled', 'matched', 'ignored', 'voided'));
