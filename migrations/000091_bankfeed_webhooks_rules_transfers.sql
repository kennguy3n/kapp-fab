-- Workstream 1 (backend): bank feeds + reconciliation toward Xero parity.
--
-- Adds the persistence backing three capabilities layered on the existing
-- bank-feed surface (migrations 000085 / 000086):
--
--   1. Bank-rules engine depth — compound, multi-field conditions and an
--      extra tax-code action on bank_reconciliation_rules, so a rule can
--      match on payee / reference / amount with contains / equals / regex
--      semantics (Xero "bank rule" parity) and allocate an account + tax
--      code + tracking/cost-center.
--
--   2. Inter-account transfer auto-pairing — a new bank_transfer_pairs
--      table recording a debit in one account paired with the matching
--      credit in another as a single transfer, plus a dedicated
--      'transfer' status on bank_transactions so a paired line drops out
--      of the income/expense reconciliation surface (it is money moving
--      between the tenant's own accounts, not P&L activity).
--
--   3. Duplicate detection — a self-referential duplicate_of pointer on
--      bank_transactions flagging a line the detector believes restates an
--      earlier one (e.g. the same real transaction ingested via two
--      overlapping feeds). It is a conservative *flag* only: the line is
--      never hidden or deleted, so a false positive can never drop a
--      genuine statement line off the books.
--
-- Everything is tenant-scoped with RLS, uses the (tenant_id, id) PK
-- convention, and grants kapp_app the same way as the rest of the schema.
-- Reserved migration number for this workstream is 000091.

-- ---------------------------------------------------------------------------
-- 1. Bank-rules engine depth
-- ---------------------------------------------------------------------------

-- target_tax_code: the tax/VAT code a rule allocates alongside the account
-- and cost-center. Like the other target_* columns it is advisory rule
-- configuration consumed by the (separate) auto-posting path, not by the
-- reconciliation matcher; NULL when the rule sets no tax treatment.
ALTER TABLE bank_reconciliation_rules
    ADD COLUMN IF NOT EXISTS target_tax_code TEXT;

-- conditions: an optional JSONB array of structured, ANDed/ORed conditions
-- ([{ "field": "...", "op": "...", "value": "..." }, ...]) that supersedes
-- the legacy single condition_type/condition_value pair when present. The
-- legacy columns are retained for backward compatibility: a row with a NULL
-- / empty conditions array is still evaluated via condition_type. Stored as
-- JSONB so the evaluator reads the whole rule in one row read and the shape
-- can grow without a further migration.
ALTER TABLE bank_reconciliation_rules
    ADD COLUMN IF NOT EXISTS conditions JSONB;

-- condition_match selects how a multi-condition rule combines: 'all' (every
-- condition must match, the default and the common case) or 'any' (the rule
-- fires if at least one matches). Ignored for the legacy single-condition
-- path.
ALTER TABLE bank_reconciliation_rules
    ADD COLUMN IF NOT EXISTS condition_match TEXT NOT NULL DEFAULT 'all';

ALTER TABLE bank_reconciliation_rules
    DROP CONSTRAINT IF EXISTS bank_reconciliation_rules_condition_match_check;
ALTER TABLE bank_reconciliation_rules
    ADD CONSTRAINT bank_reconciliation_rules_condition_match_check
    CHECK (condition_match IN ('all', 'any'));

-- The legacy single-condition path requires condition_type/condition_value
-- NOT NULL (migration 000085). A compound-condition rule has no single
-- condition, so relax those columns to nullable; the application validates
-- that a rule carries either a legacy condition or a non-empty conditions
-- array.
ALTER TABLE bank_reconciliation_rules
    ALTER COLUMN condition_type DROP NOT NULL;
ALTER TABLE bank_reconciliation_rules
    ALTER COLUMN condition_value DROP NOT NULL;

-- ---------------------------------------------------------------------------
-- 2. Duplicate-detection flag on statement lines
-- ---------------------------------------------------------------------------

-- duplicate_of points at the earlier bank_transactions row this line is
-- suspected to duplicate. NULL for the overwhelming majority of lines. The
-- FK is composite on (tenant_id, duplicate_of) so the pointer can never
-- cross a tenant boundary, and ON DELETE SET NULL keeps a line visible if
-- its canonical original is later removed.
ALTER TABLE bank_transactions
    ADD COLUMN IF NOT EXISTS duplicate_of UUID;

ALTER TABLE bank_transactions
    DROP CONSTRAINT IF EXISTS bank_transactions_duplicate_of_fkey;
ALTER TABLE bank_transactions
    ADD CONSTRAINT bank_transactions_duplicate_of_fkey
    FOREIGN KEY (tenant_id, duplicate_of)
    REFERENCES bank_transactions (tenant_id, id) ON DELETE SET NULL;

-- Index the (rare) flagged rows so the reconciliation surface can list
-- suspected duplicates cheaply without scanning the whole table.
CREATE INDEX IF NOT EXISTS bank_transactions_duplicate_of_idx
    ON bank_transactions (tenant_id, duplicate_of)
    WHERE duplicate_of IS NOT NULL;

-- Widen the status CHECK with a dedicated 'transfer' state. Widening a CHECK
-- is backward compatible — every existing row already satisfies the larger
-- set — and needs no data backfill. A 'transfer' line is one half of an
-- auto-paired inter-account transfer: resolved, but neither matched to a
-- journal entry nor a human-ignored line.
ALTER TABLE bank_transactions
    DROP CONSTRAINT IF EXISTS bank_transactions_status_check;
ALTER TABLE bank_transactions
    ADD CONSTRAINT bank_transactions_status_check
    CHECK (status IN ('unreconciled', 'matched', 'ignored', 'voided', 'transfer'));

-- ---------------------------------------------------------------------------
-- 3. Inter-account transfer pairs
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS bank_transfer_pairs (
    tenant_id      UUID NOT NULL,
    id             UUID NOT NULL,
    -- The money-out line (negative amount) and the money-in line (positive
    -- amount). They live in different bank accounts of the same tenant.
    debit_txn_id   UUID NOT NULL,
    credit_txn_id  UUID NOT NULL,
    -- Absolute magnitude + currency of the transfer, denormalized so a
    -- listing of transfers needs no join back to the two lines.
    amount         NUMERIC(20,4) NOT NULL,
    currency       TEXT NOT NULL,
    confidence     NUMERIC(4,3) NOT NULL DEFAULT 0
        CHECK (confidence >= 0 AND confidence <= 1),
    detected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, debit_txn_id) REFERENCES bank_transactions (tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, credit_txn_id) REFERENCES bank_transactions (tenant_id, id) ON DELETE CASCADE,
    -- A line can be one half of at most one transfer.
    CONSTRAINT bank_transfer_pairs_distinct_legs CHECK (debit_txn_id <> credit_txn_id)
);

-- Each statement line participates in at most one transfer pair, on either
-- leg: the partial unique indexes make a second pairing attempt a no-op via
-- ON CONFLICT instead of double-pairing a line.
CREATE UNIQUE INDEX IF NOT EXISTS bank_transfer_pairs_debit_uniq
    ON bank_transfer_pairs (tenant_id, debit_txn_id);
CREATE UNIQUE INDEX IF NOT EXISTS bank_transfer_pairs_credit_uniq
    ON bank_transfer_pairs (tenant_id, credit_txn_id);

ALTER TABLE bank_transfer_pairs ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_transfer_pairs;
CREATE POLICY tenant_isolation ON bank_transfer_pairs
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_transfer_pairs TO kapp_app;
