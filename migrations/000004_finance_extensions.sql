-- Phase C — Finance Basics extensions.
--
-- The `accounts`, `journal_entries`, and `journal_lines` tables already
-- exist in migrations/000001_initial_schema.sql with RLS enabled. This
-- migration adds the two auxiliary tables the ledger engine needs:
--
--   * fiscal_periods — period lockout for closed books
--   * tax_codes      — basic VAT/GST rate registry
--
-- Both are tenant-scoped and RLS-isolated via the same DO-block policy
-- pattern used for every other tenant table in the schema.

-- ---------------------------------------------------------------------------
-- Fiscal periods (period lockout)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS fiscal_periods (
    tenant_id       UUID NOT NULL,
    period_start    DATE NOT NULL,
    period_end      DATE NOT NULL,
    locked          BOOLEAN NOT NULL DEFAULT FALSE,
    locked_at       TIMESTAMPTZ,
    locked_by       UUID,
    PRIMARY KEY (tenant_id, period_start),
    CHECK (period_end >= period_start)
);

CREATE INDEX IF NOT EXISTS fiscal_periods_tenant_end_idx
    ON fiscal_periods (tenant_id, period_end);

-- ---------------------------------------------------------------------------
-- Tax codes (VAT/GST registry)
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS tax_codes (
    tenant_id       UUID NOT NULL,
    code            TEXT NOT NULL,
    name            TEXT NOT NULL,
    rate            NUMERIC(5,2) NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('inclusive','exclusive')),
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (tenant_id, code),
    CHECK (rate >= 0 AND rate <= 100)
);

-- ---------------------------------------------------------------------------
-- Row-level security: same tenant_isolation pattern as every other
-- tenant-scoped table (migrations/000001_initial_schema.sql lines 336-357).
-- ---------------------------------------------------------------------------

ALTER TABLE fiscal_periods ENABLE ROW LEVEL SECURITY;
ALTER TABLE tax_codes      ENABLE ROW LEVEL SECURITY;

DO $$
DECLARE
    t TEXT;
    tenant_tables TEXT[] := ARRAY['fiscal_periods', 'tax_codes'];
BEGIN
    FOREACH t IN ARRAY tenant_tables LOOP
        EXECUTE format(
            'DROP POLICY IF EXISTS tenant_isolation ON %I', t
        );
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I ' ||
            'USING (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid) ' ||
            'WITH CHECK (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), '''')::uuid)',
            t
        );
    END LOOP;
END $$;

-- Grant the standard app role the usual CRUD perms on the new tables so
-- kapp_app (non-superuser, RLS-enforced) can write under tenant context.
GRANT SELECT, INSERT, UPDATE, DELETE ON fiscal_periods TO kapp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON tax_codes      TO kapp_app;

-- ---------------------------------------------------------------------------
-- Source-row idempotency for posted journal entries.
--
-- InvoicePoster commits a journal entry in one transaction and then
-- patches the source KRecord (status=posted, journal_entry_id=…) in a
-- separate transaction. If the patch fails or two posters race, the
-- first-phase guard on the KRecord (status != 'posted') can miss the
-- conflict and a second JE gets inserted for the same invoice/bill —
-- double-posting the ledger.
--
-- This partial unique index is the DB-level safety net: every posted
-- entry that carries a source reference is unique per tenant. The Go
-- poster catches the 23505 SQL state and treats the collision as
-- "already posted, reuse the existing entry" for replay safety.
CREATE UNIQUE INDEX IF NOT EXISTS journal_entries_source_uniq
    ON journal_entries (tenant_id, source_ktype, source_id)
    WHERE source_id IS NOT NULL;
