-- Phase N9d — Cycle counts.
--
-- A cycle count is a physical inventory audit: an operator walks a
-- warehouse with a clipboard (or scanner), counts the actual quantity
-- of every SKU in a bin, and the system records the variance vs the
-- system-recorded qty_on_hand. Variances generate inventory_moves
-- (positive or negative) that true up the stock_levels view so the
-- ledger matches reality.
--
-- Design:
--
--   * cycle_count_sessions is the header. One session = one count
--     event scoped to a warehouse (and optionally narrowed by item
--     filter at the line level). Status walks draft -> counting ->
--     reconciled -> posted. Only `posted` is terminal; all other
--     states allow line edits.
--
--   * cycle_count_lines holds one row per item counted. The
--     `expected_qty` column captures the system-recorded quantity at
--     the moment the line was created so concurrent inventory moves
--     between session creation and post-time don't change the
--     variance under the operator's feet. `counted_qty` is what the
--     operator entered. `variance` is a STORED computed column
--     (counted_qty - expected_qty) so the DB always derives it
--     consistently rather than relying on the application to write
--     the right thing.
--
--   * On post, each line with a non-zero variance produces ONE
--     inventory_move with source_ktype = 'inventory.cycle_count' and
--     source_id = line.id. Keying on line.id (not session.id) means
--     each line — not each session — owns one slot in the
--     inventory_moves_source_uniq partial index, so the poster is
--     idempotent: a retry of the same line folds into a no-op via
--     ErrDuplicateSourceMove. The current schema enforces ONE line
--     per (tenant_id, session_id, item_id) (see
--     cycle_count_lines_session_item_uniq below), which deliberately
--     forbids the multiple-lines-per-item case (e.g. counting two
--     separate bins of the same SKU) until a `location` /
--     `bin_code` column is added to both the line table and the
--     uniq index. Until then, multi-bin counts must be merged into
--     a single line per item per session at data-entry time.
--
--   * RLS is enforced via app.tenant_id GUC, same scheme as every
--     other tenant-scoped table.

CREATE TABLE IF NOT EXISTS cycle_count_sessions (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    id           UUID NOT NULL DEFAULT gen_random_uuid(),
    code         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    warehouse_id UUID NOT NULL,
    status       TEXT NOT NULL DEFAULT 'draft',
    created_by   UUID NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    posted_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT cycle_count_sessions_status_chk
        CHECK (status IN ('draft', 'counting', 'reconciled', 'posted')),
    CONSTRAINT cycle_count_sessions_code_not_blank_chk
        CHECK (length(btrim(code)) > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS cycle_count_sessions_code_uniq
    ON cycle_count_sessions (tenant_id, code);

CREATE INDEX IF NOT EXISTS cycle_count_sessions_status_idx
    ON cycle_count_sessions (tenant_id, status);

ALTER TABLE cycle_count_sessions ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS cycle_count_sessions_isolation ON cycle_count_sessions;
CREATE POLICY cycle_count_sessions_isolation ON cycle_count_sessions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON cycle_count_sessions TO kapp_app;

CREATE TABLE IF NOT EXISTS cycle_count_lines (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    id           UUID NOT NULL DEFAULT gen_random_uuid(),
    session_id   UUID NOT NULL,
    item_id      UUID NOT NULL,
    expected_qty NUMERIC(18, 4) NOT NULL,
    counted_qty  NUMERIC(18, 4) NOT NULL DEFAULT 0,
    -- STORED computed so the DB owns the derivation; the application
    -- never writes to this column. Selecting the row therefore
    -- always returns a self-consistent (expected, counted, variance)
    -- triple.
    variance     NUMERIC(18, 4) GENERATED ALWAYS AS (counted_qty - expected_qty) STORED,
    notes        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, id),
    CONSTRAINT cycle_count_lines_session_fk
        FOREIGN KEY (tenant_id, session_id)
        REFERENCES cycle_count_sessions (tenant_id, id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS cycle_count_lines_session_idx
    ON cycle_count_lines (tenant_id, session_id);

-- One line per item per session. Without this index
-- SeedExpectedFromStock's INSERT ... ON CONFLICT cannot deduplicate
-- by item, and a re-seed would produce one extra row per item per
-- call. Each duplicate row would carry its own line.id and therefore
-- trip the inventory_moves_source_uniq tuple as a *distinct* source
-- at post time, double-adjusting the stock ledger.
CREATE UNIQUE INDEX IF NOT EXISTS cycle_count_lines_session_item_uniq
    ON cycle_count_lines (tenant_id, session_id, item_id);

ALTER TABLE cycle_count_lines ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS cycle_count_lines_isolation ON cycle_count_lines;
CREATE POLICY cycle_count_lines_isolation ON cycle_count_lines
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON cycle_count_lines TO kapp_app;

COMMENT ON TABLE cycle_count_sessions IS
    'Phase N9d cycle-count session header. Status walks draft -> counting -> reconciled -> posted. On post each cycle_count_line with non-zero variance produces an inventory_move keyed on (inventory.cycle_count, line.id).';
COMMENT ON TABLE cycle_count_lines IS
    'Phase N9d cycle-count line. expected_qty is the system snapshot at create time; counted_qty is operator-entered; variance is STORED computed.';
