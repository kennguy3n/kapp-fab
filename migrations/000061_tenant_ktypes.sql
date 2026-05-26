-- Phase N8b — tenant-authored ("low-code") KTypes.
--
-- KType authoring has historically required a Go developer to write
-- internal/<module>/ktypes.go, attach posting hooks (where relevant),
-- and ship a release. ERPNext's DocType Builder shows that letting
-- power users define their own custom business objects (asset
-- registers, approval forms, compliance checklists, etc.) is a
-- significant adoption driver and the audit recommendation #6 asked
-- us to close that gap without compromising the integrity of
-- platform-authored types.
--
-- Design:
--
--   * Custom KTypes are stored in a NEW table `tenant_ktypes`,
--     fully separated from the install-wide `ktypes` table. They
--     never share key space with platform types, never overwrite
--     them, and never participate in the global content-hash boot
--     cache.
--
--   * Names are forced into the `custom.<slug>` namespace by a DB
--     CHECK. This prevents a tenant from authoring `crm.deal` and
--     overriding the platform CRM contract, and gives every other
--     query in the platform (record store, agent tools, exports) a
--     reliable way to tell apart platform vs tenant types by prefix.
--
--   * RLS is enforced via app.tenant_id GUC — the same scheme every
--     other tenant-scoped table in the schema uses. The record
--     store sets the GUC inside WithTenantTx for any custom-type
--     CRUD, so tenant_ktypes rows are isolated identically to
--     krecords.
--
--   * `status` is `draft | active | archived`. Only `active` rows
--     can back record creates — drafts stay editable in the builder
--     UI and archived rows freeze the schema so existing krecords
--     stay readable but no further inserts happen.
--
--   * Posting hooks, custom Go logic, and arbitrary calculated
--     fields stay developer-only — those still require shipping
--     code in internal/<module>/. The `schema` JSONB is restricted
--     to the safe field-type subset enforced by
--     internal/ktype/tenant_store.go before insert.
--
-- The migration is forward-only: krecords already reference KType
-- names via their `ktype` column, no schema change is needed there.

CREATE TABLE IF NOT EXISTS tenant_ktypes (
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    -- title is the human-readable label shown in the builder list
    -- and on the generated record forms. Separate from `name`
    -- because `name` is a machine identifier (custom.asset_register)
    -- while title can change freely (e.g. "Asset Register" → "Fixed
    -- Assets") without renaming the underlying KType.
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    schema      JSONB NOT NULL,
    status      TEXT NOT NULL DEFAULT 'draft',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by  UUID NOT NULL,
    PRIMARY KEY (tenant_id, name, version),

    -- Names must be inside the custom.* namespace. The DB CHECK is
    -- the last line of defence — internal/ktype/tenant_store.go
    -- also rejects names that don't match this pattern before the
    -- INSERT. Lower-case slug enforced (no uppercase, no dots after
    -- the prefix) so the namespace stays predictable across all
    -- consumers (record store, agent tools, exports).
    CONSTRAINT tenant_ktypes_name_chk
        CHECK (name ~ '^custom\.[a-z][a-z0-9_]*$'),
    CONSTRAINT tenant_ktypes_status_chk
        CHECK (status IN ('draft', 'active', 'archived')),
    CONSTRAINT tenant_ktypes_version_chk
        CHECK (version >= 1)
);

CREATE INDEX IF NOT EXISTS tenant_ktypes_status_idx
    ON tenant_ktypes (tenant_id, status);

ALTER TABLE tenant_ktypes ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_ktypes_isolation ON tenant_ktypes;
CREATE POLICY tenant_ktypes_isolation ON tenant_ktypes
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_ktypes TO kapp_app;

COMMENT ON TABLE tenant_ktypes IS
    'Tenant-authored (low-code) custom KTypes. Distinct table + custom.* namespace keeps them isolated from platform-shipped KTypes in the global `ktypes` table; RLS isolates rows per tenant. Posting hooks and custom Go logic are NOT supported here — those still require developer-authored types.';
COMMENT ON COLUMN tenant_ktypes.status IS
    'draft = editable in the builder, no records may be created; active = backing record creates; archived = frozen, existing records readable, no new records.';
COMMENT ON COLUMN tenant_ktypes.schema IS
    'KType schema (same JSON shape as platform KTypes) but restricted to the safe field-type subset enforced by internal/ktype/tenant_store.go.';
