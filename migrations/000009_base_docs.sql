-- Phase F — Base KApp and Docs KApp.
--
-- Base KApp lets tenants define ad-hoc tables (spreadsheet-style) that
-- don't warrant a full KType. A `base.table` row stores its column
-- schema as JSON; `base.row` carries one row of arbitrary JSON keyed
-- by that schema. Both are tenant-scoped, RLS-isolated, and rate-limit
-- / quota-enforced through the same middleware stack as the generic
-- KRecord surface.
--
-- Docs KApp stores artifact documents with append-only version history.
-- `docs.document` holds the current version (title, content, doc_type,
-- version number); `docs.document_version` is the versioned history
-- table and `POST /docs/:id/versions` writes a new row each time. The
-- restore endpoint copies any historical row's content back onto the
-- live document and writes a new history row — no row is ever deleted.

-- ---------------------------------------------------------------------------
-- Base KApp
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS base_tables (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    slug            TEXT NOT NULL,
    name            TEXT NOT NULL,
    description     TEXT,
    columns         JSONB NOT NULL DEFAULT '[]'::jsonb,
    shared_view     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, slug)
);

ALTER TABLE base_tables ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON base_tables;
CREATE POLICY tenant_isolation ON base_tables
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON base_tables TO kapp_app;

CREATE TABLE IF NOT EXISTS base_rows (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    table_id        UUID NOT NULL,
    data            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS base_rows_tenant_table_idx
    ON base_rows (tenant_id, table_id, updated_at DESC);

ALTER TABLE base_rows ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON base_rows;
CREATE POLICY tenant_isolation ON base_rows
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON base_rows TO kapp_app;

-- ---------------------------------------------------------------------------
-- Docs KApp
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS docs_documents (
    tenant_id       UUID NOT NULL,
    id              UUID NOT NULL,
    title           TEXT NOT NULL,
    doc_type        TEXT NOT NULL DEFAULT 'note',
    content         JSONB NOT NULL DEFAULT '{}'::jsonb,
    current_version INT NOT NULL DEFAULT 1,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by      UUID,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

ALTER TABLE docs_documents ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON docs_documents;
CREATE POLICY tenant_isolation ON docs_documents
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON docs_documents TO kapp_app;

-- Append-only history. No UPDATE / DELETE — a "revert" is written as
-- a new row pointing at the restored version number so the audit
-- trail retains every edit.
CREATE TABLE IF NOT EXISTS docs_document_versions (
    tenant_id       UUID NOT NULL,
    document_id     UUID NOT NULL,
    version         INT NOT NULL,
    title           TEXT NOT NULL,
    content         JSONB NOT NULL DEFAULT '{}'::jsonb,
    diff            JSONB NOT NULL DEFAULT '{}'::jsonb,
    change_summary  TEXT,
    restored_from   INT,
    created_by      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, document_id, version)
);

CREATE INDEX IF NOT EXISTS docs_document_versions_tenant_doc_idx
    ON docs_document_versions (tenant_id, document_id, version DESC);

ALTER TABLE docs_document_versions ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON docs_document_versions;
CREATE POLICY tenant_isolation ON docs_document_versions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT ON docs_document_versions TO kapp_app;
