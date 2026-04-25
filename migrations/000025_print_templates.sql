-- Phase J — tenant-customizable print templates for KRecord PDFs.
--
-- Reference: frappe/frappe Print Format + erpnext Sales Invoice print.
--
-- `print_templates` stores per-tenant overrides for each KType's
-- print HTML. The renderer resolves a template by (tenant_id, ktype)
-- with is_default=TRUE; when no row exists the renderer falls back
-- to the package-embedded defaults in internal/print/templates/.
-- This means new tenants get a sensible printable invoice / payslip
-- / purchase-order layout without any seed data, and admins can
-- customise later without migrations.

CREATE TABLE IF NOT EXISTS print_templates (
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    id              UUID NOT NULL,
    ktype           TEXT NOT NULL,
    name            TEXT NOT NULL,
    html_template   TEXT NOT NULL,
    is_default      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- One default per (tenant, ktype). Enforced as a partial unique
-- index so non-default templates can coexist.
CREATE UNIQUE INDEX IF NOT EXISTS print_templates_default_idx
    ON print_templates (tenant_id, ktype)
    WHERE is_default;

CREATE INDEX IF NOT EXISTS print_templates_ktype_idx
    ON print_templates (tenant_id, ktype);

ALTER TABLE print_templates ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON print_templates;
CREATE POLICY tenant_isolation ON print_templates
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON print_templates TO kapp_app;
