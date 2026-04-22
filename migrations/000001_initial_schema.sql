-- Kapp Business Suite — Phase A initial schema.
--
-- This migration provisions the kernel tables described in ARCHITECTURE.md
-- §7: tenants/users/roles (control plane), KType registry, KRecord store,
-- workflows, approvals, events/audit outboxes, finance + inventory tables,
-- files, and idempotency keys.
--
-- Tenant-scoped tables that hold write-heavy or per-tenant extractable data
-- (krecords, events, audit_log, journal_lines, inventory_moves) are
-- partitioned by `tenant_id` range so that large tenants can be split out
-- and vacuum/backup work scales by range rather than across the whole table.
-- A DEFAULT partition is created for each so that Phase A queries work
-- immediately; per-range partitions are added as tenants arrive.
--
-- Row-level security is enabled on every tenant-scoped table as
-- defense-in-depth. The policy reads `app.tenant_id` from the current GUC,
-- which application code must set via `SET LOCAL` inside each transaction
-- (see internal/platform/db.go:SetTenantContext). The `tenants`, `users`,
-- and `ktypes` tables are intentionally excluded from RLS: they are either
-- control-plane or globally shared metadata.
--
-- The migration also provisions a non-superuser role `kapp_app` that the
-- application connects as. RLS policies are enforced against this role (the
-- database owner can still read all rows for administration and backups).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Control plane: tenants, users, memberships, roles
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS tenants (
    id              UUID PRIMARY KEY,
    slug            TEXT NOT NULL UNIQUE,
    name            TEXT NOT NULL,
    cell            TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('active','suspended','archived','deleting')),
    plan            TEXT NOT NULL,
    quota           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY,
    kchat_user_id   TEXT NOT NULL UNIQUE,
    email           TEXT,
    display_name    TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS user_tenants (
    user_id         UUID NOT NULL REFERENCES users(id),
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    role            TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('active','invited','suspended')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, tenant_id)
);
CREATE INDEX IF NOT EXISTS user_tenants_tenant_user_idx
    ON user_tenants (tenant_id, user_id);

CREATE TABLE IF NOT EXISTS roles (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    name            TEXT NOT NULL,
    permissions     JSONB NOT NULL DEFAULT '[]'::jsonb,
    PRIMARY KEY (tenant_id, name)
);

-- ---------------------------------------------------------------------------
-- KType registry (shared, versioned). Not tenant-scoped.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS ktypes (
    name            TEXT NOT NULL,
    version         INT NOT NULL,
    schema          JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);

-- ---------------------------------------------------------------------------
-- Generic KRecord store — partitioned by tenant_id range
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS krecords (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    ktype           TEXT NOT NULL,
    ktype_version   INT NOT NULL,
    data            JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    version         INT NOT NULL DEFAULT 1,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      UUID,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS krecords_default PARTITION OF krecords DEFAULT;

CREATE INDEX IF NOT EXISTS krecords_tenant_ktype_updated_idx
    ON krecords (tenant_id, ktype, updated_at DESC);

-- ---------------------------------------------------------------------------
-- Workflows and approvals
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS workflows (
    tenant_id       UUID NOT NULL,
    name            TEXT NOT NULL,
    version         INT NOT NULL,
    definition      JSONB NOT NULL,
    PRIMARY KEY (tenant_id, name, version)
);

CREATE TABLE IF NOT EXISTS workflow_runs (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    workflow        TEXT NOT NULL,
    record_id       UUID NOT NULL,
    state           TEXT NOT NULL,
    history         JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS workflow_runs_tenant_record_idx
    ON workflow_runs (tenant_id, record_id);

CREATE TABLE IF NOT EXISTS approvals (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    record_ktype    TEXT NOT NULL,
    record_id       UUID NOT NULL,
    chain           JSONB NOT NULL,
    state           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS approvals_tenant_state_idx
    ON approvals (tenant_id, state);

-- ---------------------------------------------------------------------------
-- Event outbox — partitioned by tenant_id range
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS events (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    type            TEXT NOT NULL,
    payload         JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS events_default PARTITION OF events DEFAULT;

CREATE INDEX IF NOT EXISTS events_undelivered_idx
    ON events (tenant_id, created_at) WHERE delivered_at IS NULL;

-- ---------------------------------------------------------------------------
-- Append-only audit log — partitioned by tenant_id range
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS audit_log (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    actor_id        UUID,
    actor_kind      TEXT NOT NULL CHECK (actor_kind IN ('user','agent','system')),
    action          TEXT NOT NULL,
    target_ktype    TEXT,
    target_id       UUID,
    before          JSONB,
    after           JSONB,
    context         JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS audit_log_default PARTITION OF audit_log DEFAULT;

-- ---------------------------------------------------------------------------
-- Finance: chart of accounts + append-only journal
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS accounts (
    tenant_id       UUID NOT NULL,
    code            TEXT NOT NULL,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL CHECK (type IN ('asset','liability','equity','revenue','expense')),
    parent_code     TEXT,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (tenant_id, code)
);

CREATE TABLE IF NOT EXISTS journal_entries (
    id              UUID NOT NULL,
    tenant_id       UUID NOT NULL,
    posted_at       TIMESTAMPTZ NOT NULL,
    memo            TEXT,
    source_ktype    TEXT,
    source_id       UUID,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS journal_entries_tenant_posted_idx
    ON journal_entries (tenant_id, posted_at);

CREATE TABLE IF NOT EXISTS journal_lines (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    entry_id        UUID NOT NULL,
    account_code    TEXT NOT NULL,
    debit           NUMERIC(20,4) NOT NULL DEFAULT 0,
    credit          NUMERIC(20,4) NOT NULL DEFAULT 0,
    currency        TEXT NOT NULL,
    memo            TEXT,
    PRIMARY KEY (tenant_id, id),
    CHECK (debit >= 0 AND credit >= 0),
    CHECK (NOT (debit > 0 AND credit > 0))
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS journal_lines_default PARTITION OF journal_lines DEFAULT;

CREATE INDEX IF NOT EXISTS journal_lines_tenant_entry_idx
    ON journal_lines (tenant_id, entry_id);
CREATE INDEX IF NOT EXISTS journal_lines_tenant_account_idx
    ON journal_lines (tenant_id, account_code);

-- ---------------------------------------------------------------------------
-- Inventory: items, warehouses, append-only moves
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS inventory_items (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    sku             TEXT NOT NULL,
    name            TEXT NOT NULL,
    uom             TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, sku)
);

CREATE TABLE IF NOT EXISTS inventory_warehouses (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    code            TEXT NOT NULL,
    name            TEXT NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, code)
);

CREATE TABLE IF NOT EXISTS inventory_moves (
    id              BIGSERIAL,
    tenant_id       UUID NOT NULL,
    item_id         UUID NOT NULL,
    warehouse_id    UUID NOT NULL,
    qty             NUMERIC(20,4) NOT NULL,
    unit_cost       NUMERIC(20,4),
    source_ktype    TEXT,
    source_id       UUID,
    moved_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
) PARTITION BY RANGE (tenant_id);

CREATE TABLE IF NOT EXISTS inventory_moves_default PARTITION OF inventory_moves DEFAULT;

CREATE INDEX IF NOT EXISTS inventory_moves_tenant_item_warehouse_idx
    ON inventory_moves (tenant_id, item_id, warehouse_id, moved_at);

-- ---------------------------------------------------------------------------
-- Files / attachments
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS files (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    storage_key     TEXT NOT NULL,
    content_hash    TEXT NOT NULL,
    content_type    TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    uploaded_by     UUID NOT NULL,
    uploaded_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS files_tenant_hash_idx
    ON files (tenant_id, content_hash);

-- ---------------------------------------------------------------------------
-- Idempotency keys for mutating APIs (ARCHITECTURE.md §8 rule 6).
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id       UUID NOT NULL,
    key             TEXT NOT NULL,
    response_code   INT NOT NULL,
    response_body   JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);
CREATE INDEX IF NOT EXISTS idempotency_keys_created_idx
    ON idempotency_keys (created_at);

-- ---------------------------------------------------------------------------
-- Row-level security policies
-- ---------------------------------------------------------------------------

ALTER TABLE user_tenants         ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles                ENABLE ROW LEVEL SECURITY;
ALTER TABLE krecords             ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflows            ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_runs        ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals            ENABLE ROW LEVEL SECURITY;
ALTER TABLE events               ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log            ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts             ENABLE ROW LEVEL SECURITY;
ALTER TABLE journal_entries      ENABLE ROW LEVEL SECURITY;
ALTER TABLE journal_lines        ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_items      ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_warehouses ENABLE ROW LEVEL SECURITY;
ALTER TABLE inventory_moves      ENABLE ROW LEVEL SECURITY;
ALTER TABLE files                ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys     ENABLE ROW LEVEL SECURITY;

-- The policy body references the `app.tenant_id` GUC. If the GUC is unset or
-- empty, `current_setting('app.tenant_id', true)` returns NULL and the USING
-- clause evaluates to NULL → rejection. This gives default-deny behaviour
-- when no tenant context is established (e.g. direct DB access without
-- `SET LOCAL`).
DO $$
DECLARE
    t TEXT;
    tenant_tables TEXT[] := ARRAY[
        'user_tenants', 'roles', 'krecords', 'workflows', 'workflow_runs',
        'approvals', 'events', 'audit_log', 'accounts', 'journal_entries',
        'journal_lines', 'inventory_items', 'inventory_warehouses',
        'inventory_moves', 'files', 'idempotency_keys'
    ];
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

-- ---------------------------------------------------------------------------
-- Application role. The application connects as `kapp_app`, which is a
-- non-superuser and therefore subject to RLS. Superusers and the database
-- owner bypass RLS by default — keep those reserved for administration.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kapp_app') THEN
        CREATE ROLE kapp_app LOGIN PASSWORD 'kapp_app_dev';
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO kapp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO kapp_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kapp_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO kapp_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO kapp_app;
