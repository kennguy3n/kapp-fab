-- Phase K — Saved-report sharing.
--
-- Adds two columns to saved_reports so a tenant can share an
-- author-owned report with other users / roles inside the same
-- tenant. The RLS policy on the table is unchanged — visibility is
-- a per-row gate INSIDE a tenant; cross-tenant isolation still flows
-- entirely through tenant_id + the tenant_isolation policy.
--
-- visibility    enum-ish text column with a CHECK constraint.
--               'private'  → only owner / admin sees the row
--               'shared'   → owner + anyone listed in shared_with
--               'public'   → every user in the tenant
--
-- shared_with   JSONB array of `{ "type": "role"|"user", "id": "..." }`.
--               The store filters on this column with @> containment
--               so the predicate is index-friendly when we add a
--               GIN index later (out of scope for this migration).
--
-- Reference: frappe/frappe Report Builder share-by-role.

ALTER TABLE saved_reports
    ADD COLUMN IF NOT EXISTS visibility   TEXT NOT NULL DEFAULT 'private',
    ADD COLUMN IF NOT EXISTS shared_with  JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE saved_reports
    DROP CONSTRAINT IF EXISTS saved_reports_visibility_check;
ALTER TABLE saved_reports
    ADD CONSTRAINT saved_reports_visibility_check
    CHECK (visibility IN ('private', 'shared', 'public'));

COMMENT ON COLUMN saved_reports.visibility IS
    'Per-tenant visibility: private (owner only), shared (owner + shared_with), public (every user in the tenant).';
COMMENT ON COLUMN saved_reports.shared_with IS
    'JSONB array of {type: role|user, id: <uuid|role-name>} the report is shared with when visibility = shared.';
