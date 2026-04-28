-- Phase M Task 7 — Advanced accounting consolidation.
--
-- A consolidation_group rolls up the trial balances of several child
-- tenants into a single combined trial balance, eliminating
-- inter-company balances and converting every line into the group's
-- presentation currency.
--
-- The table is intentionally OPERATOR-SCOPED (no tenant_id, no RLS).
-- Group membership crosses tenant boundaries — a parent company
-- might consolidate three subsidiaries — so a per-tenant scope
-- doesn't make sense. The handler is restricted to control-plane
-- admins via the existing admin-only middleware on /api/v1/admin/*.
-- Reads use the admin pool (role `kapp_admin`, BYPASSRLS) so a
-- single fetch can read trial balances across the member tenants
-- without juggling per-tenant connection contexts.
--
-- Reference: ERPNext Period Closing Voucher + inter-company
-- transactions. The "elimination" map mirrors the manual
-- inter-company reconciliation step ERPNext exposes via the
-- "Inter Company Invoice" doctype.

CREATE TABLE IF NOT EXISTS consolidation_groups (
    id                     UUID    NOT NULL PRIMARY KEY,
    name                   TEXT    NOT NULL,
    presentation_currency  TEXT    NOT NULL,
    members                UUID[]  NOT NULL,
    -- elimination_pairs is a JSONB array of
    -- {"from_tenant": "...", "to_tenant": "...", "account_code": "..."}
    -- entries describing inter-company AR/AP balances that should
    -- net to zero in the consolidated trial balance.
    elimination_pairs      JSONB   NOT NULL DEFAULT '[]'::jsonb,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at             TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS consolidation_groups_name_idx
    ON consolidation_groups (lower(name))
    WHERE deleted_at IS NULL;

GRANT SELECT, INSERT, UPDATE ON consolidation_groups TO kapp_app;

-- consolidation_runs records the materialised output of one
-- RunConsolidation call. We persist the JSON envelope so the UI can
-- render historical runs without re-querying the member tenants.
-- Like consolidation_groups, it is operator-scoped.
CREATE TABLE IF NOT EXISTS consolidation_runs (
    id          UUID    NOT NULL PRIMARY KEY,
    group_id    UUID    NOT NULL REFERENCES consolidation_groups (id),
    as_of       TIMESTAMPTZ NOT NULL,
    result      JSONB   NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by  UUID
);

CREATE INDEX IF NOT EXISTS consolidation_runs_group_idx
    ON consolidation_runs (group_id, created_at DESC);

GRANT SELECT, INSERT ON consolidation_runs TO kapp_app;
