-- Session 15: live bank feeds + smart reconciliation.
--
-- Adds the persistence backing the internal/ledger/bankfeed package:
--   * bank_feed_connections   — one per (bank_account, provider) link,
--                               with the provider's OAuth/API credentials
--                               stored field-encrypted (KAPP_MASTER_KEY,
--                               see internal/tenant/encryption.go). The
--                               *_enc columns hold the kapp:enc:v1: envelope
--                               as text bytes; they are never logged.
--   * bank_reconciliation_rules — tenant-configurable auto-categorization
--                               rules evaluated in priority order against
--                               freshly-synced transactions.
--   * bank_match_suggestions  — ML-assisted match candidates produced by
--                               the smart matcher, surfaced to the operator
--                               for accept/reject.
--
-- Everything is tenant-scoped with RLS and uses the same (tenant_id, id)
-- PK convention + kapp_app grant as the rest of the schema. The latest
-- prior migration is 000081 (cell_region_metadata); this is 000082.

-- ---------------------------------------------------------------------------
-- Feed idempotency: dedupe synced statement lines by the provider's stable
-- external reference. The hourly scheduler re-fetches an overlapping window
-- each tick (providers expose no exact cursor for already-seen rows), so the
-- ingest path relies on this partial unique index to no-op duplicates via
-- ON CONFLICT instead of accumulating repeats. Partial on a non-empty
-- external_ref so the legacy CSV import path (which may leave it NULL) is
-- unaffected and pre-existing rows need no backfill.
-- ---------------------------------------------------------------------------
CREATE UNIQUE INDEX IF NOT EXISTS bank_transactions_external_ref_uniq
    ON bank_transactions (tenant_id, bank_account_id, external_ref)
    WHERE external_ref IS NOT NULL AND external_ref <> '';

-- ---------------------------------------------------------------------------
-- Bank feed connections
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS bank_feed_connections (
    tenant_id        UUID NOT NULL,
    id               UUID NOT NULL,
    bank_account_id  UUID NOT NULL,
    provider         TEXT NOT NULL,
    -- Field-encrypted credential envelopes (kapp:enc:v1:...). BYTEA so a
    -- future binary envelope format needs no migration; the current
    -- text envelope is stored as its UTF-8 bytes. NULL when the provider
    -- needs no stored secret (e.g. the csv provider).
    access_token_enc  BYTEA,
    refresh_token_enc BYTEA,
    -- Opaque provider sync cursor (Plaid transactions/sync cursor,
    -- GoCardless last-seen marker). Not a secret, stored in clear.
    cursor            TEXT,
    -- external_id is the provider's stable handle for the linked account
    -- (Plaid account_id / item_id, GoCardless requisition/account id) so
    -- a re-link reuses the same connection row instead of duplicating.
    external_id       TEXT,
    status            TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'expired', 'revoked')),
    last_sync_at      TIMESTAMPTZ,
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, bank_account_id) REFERENCES bank_accounts (tenant_id, id)
);

-- The sync scheduler walks active connections per tenant; index the hot
-- predicate (tenant_id, status) and the per-account lookup the connect /
-- disconnect routes use.
CREATE INDEX IF NOT EXISTS bank_feed_connections_tenant_status_idx
    ON bank_feed_connections (tenant_id, status) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS bank_feed_connections_tenant_account_idx
    ON bank_feed_connections (tenant_id, bank_account_id);

ALTER TABLE bank_feed_connections ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_feed_connections;
CREATE POLICY tenant_isolation ON bank_feed_connections
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_feed_connections TO kapp_app;

-- ---------------------------------------------------------------------------
-- Reconciliation rules (tenant-configurable auto-categorization)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS bank_reconciliation_rules (
    tenant_id          UUID NOT NULL,
    id                 UUID NOT NULL,
    -- Lower priority runs first; the first matching rule wins. Not unique
    -- so two rules may share a priority (insertion order then breaks ties).
    priority           INT NOT NULL DEFAULT 100,
    condition_type     TEXT NOT NULL
        CHECK (condition_type IN ('description_contains', 'description_regex', 'amount_range', 'counterparty')),
    condition_value    TEXT NOT NULL,
    target_account_code TEXT,
    target_cost_center  TEXT,
    auto_approve       BOOLEAN NOT NULL DEFAULT FALSE,
    -- bank_account_id NULL means the rule applies to every account in the
    -- tenant; a value scopes it to one account.
    bank_account_id    UUID,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS bank_recon_rules_tenant_priority_idx
    ON bank_reconciliation_rules (tenant_id, priority) WHERE enabled;

ALTER TABLE bank_reconciliation_rules ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_reconciliation_rules;
CREATE POLICY tenant_isolation ON bank_reconciliation_rules
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_reconciliation_rules TO kapp_app;

-- ---------------------------------------------------------------------------
-- Match suggestions (ML-assisted candidates awaiting operator decision)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS bank_match_suggestions (
    tenant_id        UUID NOT NULL,
    id               UUID NOT NULL,
    transaction_id   UUID NOT NULL,
    journal_entry_id UUID NOT NULL,
    confidence       NUMERIC(4,3) NOT NULL DEFAULT 0
        CHECK (confidence >= 0 AND confidence <= 1),
    match_reason     TEXT,
    status           TEXT NOT NULL DEFAULT 'suggested'
        CHECK (status IN ('suggested', 'accepted', 'rejected')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, transaction_id) REFERENCES bank_transactions (tenant_id, id) ON DELETE CASCADE
);

-- One open suggestion per (transaction, journal_entry) so repeated syncs
-- re-rank in place instead of accumulating duplicates. Partial on the
-- 'suggested' state so an accepted/rejected pair can be re-suggested later
-- if the operator changes their mind and the matcher reruns.
CREATE UNIQUE INDEX IF NOT EXISTS bank_match_suggestions_open_uniq
    ON bank_match_suggestions (tenant_id, transaction_id, journal_entry_id)
    WHERE status = 'suggested';
CREATE INDEX IF NOT EXISTS bank_match_suggestions_tenant_txn_idx
    ON bank_match_suggestions (tenant_id, transaction_id);

ALTER TABLE bank_match_suggestions ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_match_suggestions;
CREATE POLICY tenant_isolation ON bank_match_suggestions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_match_suggestions TO kapp_app;

-- ---------------------------------------------------------------------------
-- Learned matches (historical pattern learning for the smart matcher)
-- ---------------------------------------------------------------------------
-- When an operator manually reconciles (or accepts a suggestion for) a
-- transaction, we remember the normalized description -> account_code
-- association so a future transaction with the same counterparty is
-- nudged toward the same account. Keyed on a normalized description hash
-- so lookups are O(1) and the raw (possibly PII-bearing) description is
-- not duplicated as a key.

CREATE TABLE IF NOT EXISTS bank_learned_matches (
    tenant_id        UUID NOT NULL,
    bank_account_id  UUID NOT NULL,
    description_key  TEXT NOT NULL,
    account_code     TEXT NOT NULL,
    hit_count        INT NOT NULL DEFAULT 1,
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bank_account_id, description_key, account_code)
);

ALTER TABLE bank_learned_matches ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON bank_learned_matches;
CREATE POLICY tenant_isolation ON bank_learned_matches
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON bank_learned_matches TO kapp_app;
