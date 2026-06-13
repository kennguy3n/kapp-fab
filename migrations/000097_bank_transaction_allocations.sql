-- Workstream 1 (backend): split reconciliation — record partial allocations
-- of one bank line across multiple journal entries.
--
-- The reconciliation console (web) lets an operator split a single bank
-- statement line across several ledger entries with a running difference,
-- reconciling only when the split nets to zero. Until now the backend had
-- nowhere to record that: bank_transactions carries a single
-- matched_entry_id (a strict 1:1 pairing) and bank_match_suggestions has no
-- amount column, so a split could only be persisted by accepting each
-- suggestion at its full amount — losing the partial figures the operator
-- actually entered.
--
-- This migration adds bank_transaction_allocations: one row per
-- (transaction, journal_entry) leg of a split, carrying the signed partial
-- amount in the bank line's own currency. The common 1:1 path is untouched
-- (matched_entry_id stays authoritative); a split leaves matched_entry_id
-- NULL and uses this table as the source of truth, with bank_transactions
-- still moving to the 'matched' status once the line is fully allocated.

CREATE TABLE IF NOT EXISTS bank_transaction_allocations (
    tenant_id        UUID NOT NULL,
    id               UUID NOT NULL,
    transaction_id   UUID NOT NULL,
    journal_entry_id UUID NOT NULL,
    -- Signed partial amount in the bank line's currency. NUMERIC(20,4)
    -- mirrors bank_transactions.amount so a split's legs sum exactly to the
    -- line with no float drift.
    amount           NUMERIC(20,4) NOT NULL,
    created_by       UUID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, transaction_id)
        REFERENCES bank_transactions (tenant_id, id) ON DELETE CASCADE
);

-- One allocation per (transaction, journal_entry): re-allocating the same
-- entry updates in place rather than accumulating duplicate legs, and
-- enforces the "distinct entries" rule the composer surfaces in the UI.
CREATE UNIQUE INDEX IF NOT EXISTS bank_txn_alloc_uniq
    ON bank_transaction_allocations (tenant_id, transaction_id, journal_entry_id);

-- Lookup all legs of a line when rendering / unwinding a split.
CREATE INDEX IF NOT EXISTS bank_txn_alloc_txn_idx
    ON bank_transaction_allocations (tenant_id, transaction_id);

ALTER TABLE bank_transaction_allocations ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_transaction_allocations;
CREATE POLICY tenant_isolation ON bank_transaction_allocations
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_transaction_allocations TO kapp_app;
