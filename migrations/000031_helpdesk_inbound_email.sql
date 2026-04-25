-- Phase J/K — Helpdesk inbound email tenant resolver.
--
-- Each tenant maps a set of recipient domains (e.g. acme.kapp.io,
-- support.acme.com) to its tenant_id. The InboundEmailHandler
-- resolves an incoming `To:` header against this table to decide
-- which tenant's RLS context to open the ticket under.
--
-- A single host belongs to at most one tenant — duplicates would
-- mean an inbound email could open a ticket on the wrong tenant.
-- The UNIQUE constraint enforces this at insert time.

CREATE TABLE IF NOT EXISTS tenant_support_domains (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    domain       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, domain)
);

-- Globally unique so a single recipient hostname always resolves to
-- exactly one tenant. We can't use a partial / case-insensitive
-- index on the PK so this is a separate UNIQUE INDEX.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_support_domains_domain_unique
    ON tenant_support_domains (lower(domain));

ALTER TABLE tenant_support_domains ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_support_domains_isolation ON tenant_support_domains;
CREATE POLICY tenant_support_domains_isolation ON tenant_support_domains
    USING (tenant_id::text = current_setting('app.tenant_id', true));

-- The inbound-email handler resolves the recipient host against this
-- table on the admin pool (control-plane lookup precedes RLS), so a
-- bypass policy lets the resolver SELECT without first SET LOCAL'ing
-- a tenant. Mirrors the bypass pattern on tenants.* used elsewhere.
DROP POLICY IF EXISTS tenant_support_domains_admin_bypass ON tenant_support_domains;
CREATE POLICY tenant_support_domains_admin_bypass ON tenant_support_domains
    FOR SELECT
    USING (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-000000000000');

COMMENT ON TABLE tenant_support_domains IS
    'Per-tenant inbound-email recipient domains. The InboundEmailHandler maps the To: header host to a tenant id via this table.';
