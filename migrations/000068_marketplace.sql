-- Phase B2 — marketplace extension registry.
--
-- This migration models the contracts pinned by docs/EXTENSION_SPEC.md
-- (B1, PR #125) so the downstream phases have a typed data layer to
-- build on:
--
--   * B6 (marketplace API endpoints) reads / writes through the Go
--     repository in internal/marketplace.
--   * B7 (review & security pipeline) populates the per-version
--     review_state row and gates listing.status transitions.
--   * B4 (webhook dispatch) reads webhook_base + permissions_required
--     when signing outbound requests.
--   * B5 (UI extensions) reads ui_extensions_count + manifest to render
--     installed iframe slots in the workspace shell.
--
-- Four tables, three global + one tenant-scoped.
--
--   1. marketplace_extensions ............ publisher-level listing
--   2. marketplace_extension_versions .... immutable (extension, version)
--                                          rows, one per upload
--   3. marketplace_extension_review_state. per-version operator review
--   4. marketplace_extension_installations tenant-scoped install (RLS)
--
-- Design choices anchored to the EXTENSION_SPEC:
--
--   * Namespace boundary. `name` is CHECK-constrained to
--     `<publisher>.<slug>` lower-case slug form, and `publisher` /
--     `slug` are denormalised back out of `name` with a CHECK that
--     they reconcile. This is the same DB-CHECK belt-and-braces
--     pattern as tenant_ktypes (#000061) — the application validator
--     is the first line of defence, the constraint is the last.
--
--   * Bundle immutability. (extension_id, version) is UNIQUE so a
--     second upload of the same version returns a constraint
--     violation that the repository layer translates to 409. The
--     bundle_hash is the integrity anchor — install-time re-fetch
--     verifies it (per spec §10), so a CHECK pins the SHA-256 hex
--     shape (`[a-f0-9]{64}`) to prevent garbage entries from getting
--     persisted.
--
--   * Size + count anchors. The hard limits from spec §2 ("Total
--     bundle size 10 MiB", per-file 2 MiB, ≤32 KTypes / ≤16
--     workflows / etc.) are enforced by the marketplace upload
--     pipeline in the manifest validator (internal/marketplace).
--     The DB only re-asserts the bundle-size cap (10 MiB) as a
--     last-line CHECK so corrupt or out-of-band inserts can't poison
--     the registry. The per-file and per-kind limits are not
--     re-asserted in the DB because they would require structural
--     manifest parsing the validator already does — duplicating the
--     parser in pl/pgsql would drift.
--
--   * Tenant isolation for installations. Installations are
--     per-tenant — each tenant has its own settings, webhook_base
--     (so multi-tenant SaaS installs of the same extension can point
--     at different vendor environments), and lifecycle state. RLS
--     uses the same `app.tenant_id` GUC scheme every other
--     tenant-scoped table uses, and the policy applies to all CRUD
--     (USING + WITH CHECK).
--
--   * Global tables (`marketplace_extensions`,
--     `marketplace_extension_versions`,
--     `marketplace_extension_review_state`) are intentionally NOT
--     tenant-scoped. The marketplace catalog is a shared product
--     surface — every tenant sees the same listings. Per-tenant
--     visibility (e.g. private extensions) is a Phase 2 concern and
--     will arrive via a separate `marketplace_extension_visibility`
--     join table; the schema here doesn't need to model it yet.
--
--   * Webhook base is HTTPS-only. CHECK constraint blocks `http://`
--     and any other scheme so a compromised admin UI can't downgrade
--     a tenant's extension to plaintext POST. The same check applies
--     in the manifest validator on upload; the DB constraint backs
--     it up for direct SQL writes.
--
--   * Yanking. A published version can be soft-removed (`yanked = true`)
--     so existing installations stay running but new installs are
--     refused. Hard-delete is intentionally not supported — installs
--     reference versions by id, and dropping a version would break
--     compatibility-checking for already-installed tenants.

CREATE TABLE IF NOT EXISTS marketplace_extensions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL UNIQUE,
    publisher       TEXT NOT NULL,
    slug            TEXT NOT NULL,
    display_name    TEXT NOT NULL,
    description     TEXT NOT NULL,
    author          TEXT NOT NULL,
    license         TEXT NOT NULL,
    homepage        TEXT,
    support_email   TEXT,
    icon_url        TEXT,
    status          TEXT NOT NULL DEFAULT 'unpublished',
    listed_version  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT marketplace_extensions_name_format
        CHECK (name ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$'),
    CONSTRAINT marketplace_extensions_publisher_format
        CHECK (publisher ~ '^[a-z][a-z0-9_]*$'),
    CONSTRAINT marketplace_extensions_slug_format
        CHECK (slug ~ '^[a-z][a-z0-9_]*$'),
    CONSTRAINT marketplace_extensions_name_matches_parts
        CHECK (name = publisher || '.' || slug),
    CONSTRAINT marketplace_extensions_status_valid
        CHECK (status IN ('unpublished','listed','deprecated','removed')),
    CONSTRAINT marketplace_extensions_publisher_slug_unique
        UNIQUE (publisher, slug)
);

CREATE INDEX IF NOT EXISTS marketplace_extensions_status_idx
    ON marketplace_extensions (status, name);

CREATE TABLE IF NOT EXISTS marketplace_extension_versions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    extension_id        UUID NOT NULL REFERENCES marketplace_extensions(id) ON DELETE RESTRICT,
    version             TEXT NOT NULL,
    bundle_hash         TEXT NOT NULL,
    bundle_size_bytes   BIGINT NOT NULL,
    bundle_url          TEXT NOT NULL,
    manifest            JSONB NOT NULL,
    min_kapp_version    TEXT NOT NULL,
    max_kapp_version    TEXT,
    features_required   TEXT[] NOT NULL DEFAULT '{}'::text[],
    permissions_required TEXT[] NOT NULL DEFAULT '{}'::text[],
    ktypes_count        INTEGER NOT NULL DEFAULT 0,
    workflows_count     INTEGER NOT NULL DEFAULT 0,
    agent_tools_count   INTEGER NOT NULL DEFAULT 0,
    ui_extensions_count INTEGER NOT NULL DEFAULT 0,
    webhooks_count      INTEGER NOT NULL DEFAULT 0,
    yanked              BOOLEAN NOT NULL DEFAULT FALSE,
    yanked_reason       TEXT,
    published_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT marketplace_extension_versions_unique
        UNIQUE (extension_id, version),
    CONSTRAINT marketplace_extension_versions_hash_format
        CHECK (bundle_hash ~ '^[a-f0-9]{64}$'),
    CONSTRAINT marketplace_extension_versions_version_format
        CHECK (version ~ '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'),
    CONSTRAINT marketplace_extension_versions_size_limit
        CHECK (bundle_size_bytes > 0 AND bundle_size_bytes <= 10485760),
    CONSTRAINT marketplace_extension_versions_counts_nonneg
        CHECK (
            ktypes_count >= 0 AND ktypes_count <= 32
            AND workflows_count >= 0 AND workflows_count <= 16
            AND agent_tools_count >= 0 AND agent_tools_count <= 32
            AND ui_extensions_count >= 0 AND ui_extensions_count <= 16
            AND webhooks_count >= 0 AND webhooks_count <= 16
        ),
    CONSTRAINT marketplace_extension_versions_yanked_reason_required
        CHECK ((yanked = FALSE AND yanked_reason IS NULL) OR (yanked = TRUE AND yanked_reason IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS marketplace_extension_versions_extension_idx
    ON marketplace_extension_versions (extension_id, published_at DESC);

CREATE INDEX IF NOT EXISTS marketplace_extension_versions_active_idx
    ON marketplace_extension_versions (extension_id, version)
    WHERE yanked = FALSE;

-- Immutability trigger: (extension_id, version, bundle_hash, manifest,
-- min_kapp_version, max_kapp_version, features_required,
-- permissions_required, ktypes_count, workflows_count,
-- agent_tools_count, ui_extensions_count, webhooks_count,
-- bundle_size_bytes, bundle_url, published_at) are write-once on
-- insert. The marketplace upload pipeline is the only writer; updates
-- are restricted to the yank fields (yanked, yanked_reason) so
-- operators can soft-remove without losing the row. Per spec §3.2
-- "(name, version) is immutable" — a re-upload of the same version is
-- a 409 from the repository layer; here we make sure even a misrouted
-- UPDATE statement cannot mutate the integrity-anchored columns.
CREATE OR REPLACE FUNCTION marketplace_extension_versions_immutable()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.extension_id IS DISTINCT FROM OLD.extension_id THEN
        RAISE EXCEPTION 'marketplace_extension_versions.extension_id is immutable';
    END IF;
    IF NEW.version IS DISTINCT FROM OLD.version THEN
        RAISE EXCEPTION 'marketplace_extension_versions.version is immutable';
    END IF;
    IF NEW.bundle_hash IS DISTINCT FROM OLD.bundle_hash THEN
        RAISE EXCEPTION 'marketplace_extension_versions.bundle_hash is immutable';
    END IF;
    IF NEW.bundle_size_bytes IS DISTINCT FROM OLD.bundle_size_bytes THEN
        RAISE EXCEPTION 'marketplace_extension_versions.bundle_size_bytes is immutable';
    END IF;
    IF NEW.bundle_url IS DISTINCT FROM OLD.bundle_url THEN
        RAISE EXCEPTION 'marketplace_extension_versions.bundle_url is immutable';
    END IF;
    IF NEW.manifest IS DISTINCT FROM OLD.manifest THEN
        RAISE EXCEPTION 'marketplace_extension_versions.manifest is immutable';
    END IF;
    IF NEW.min_kapp_version IS DISTINCT FROM OLD.min_kapp_version
       OR NEW.max_kapp_version IS DISTINCT FROM OLD.max_kapp_version
       OR NEW.features_required IS DISTINCT FROM OLD.features_required
       OR NEW.permissions_required IS DISTINCT FROM OLD.permissions_required
       OR NEW.ktypes_count IS DISTINCT FROM OLD.ktypes_count
       OR NEW.workflows_count IS DISTINCT FROM OLD.workflows_count
       OR NEW.agent_tools_count IS DISTINCT FROM OLD.agent_tools_count
       OR NEW.ui_extensions_count IS DISTINCT FROM OLD.ui_extensions_count
       OR NEW.webhooks_count IS DISTINCT FROM OLD.webhooks_count
       OR NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'marketplace_extension_versions schema/compat fields are immutable; create a new version row instead';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS marketplace_extension_versions_immutable_trg
    ON marketplace_extension_versions;
CREATE TRIGGER marketplace_extension_versions_immutable_trg
    BEFORE UPDATE ON marketplace_extension_versions
    FOR EACH ROW
    EXECUTE FUNCTION marketplace_extension_versions_immutable();

CREATE TABLE IF NOT EXISTS marketplace_extension_review_state (
    extension_version_id    UUID PRIMARY KEY REFERENCES marketplace_extension_versions(id) ON DELETE CASCADE,
    status                  TEXT NOT NULL DEFAULT 'submitted',
    automated_checks        JSONB NOT NULL DEFAULT '{}'::jsonb,
    manual_review_notes     TEXT,
    reviewer                TEXT,
    reviewed_at             TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT marketplace_extension_review_state_status_valid
        CHECK (status IN ('submitted','automated_passed','manual_review','approved','rejected','withdrawn')),
    CONSTRAINT marketplace_extension_review_state_terminal_review_metadata
        CHECK (
            status NOT IN ('approved','rejected')
            OR (reviewer IS NOT NULL AND reviewed_at IS NOT NULL)
        )
);

CREATE INDEX IF NOT EXISTS marketplace_extension_review_state_status_idx
    ON marketplace_extension_review_state (status);

CREATE TABLE IF NOT EXISTS marketplace_extension_installations (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    extension_id             UUID NOT NULL REFERENCES marketplace_extensions(id) ON DELETE RESTRICT,
    extension_version_id     UUID NOT NULL REFERENCES marketplace_extension_versions(id) ON DELETE RESTRICT,
    status                   TEXT NOT NULL DEFAULT 'pending',
    settings                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    webhook_base             TEXT NOT NULL,
    installed_by             UUID REFERENCES users(id),
    installed_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_health_check_at     TIMESTAMPTZ,
    last_health_check_status TEXT,
    failure_reason           TEXT,

    CONSTRAINT marketplace_installations_tenant_extension_unique
        UNIQUE (tenant_id, extension_id),
    CONSTRAINT marketplace_installations_status_valid
        CHECK (status IN ('pending','installing','active','disabled','failed','uninstalled')),
    CONSTRAINT marketplace_installations_webhook_base_https
        CHECK (webhook_base ~ '^https://'),
    CONSTRAINT marketplace_installations_failure_reason_only_when_failed
        CHECK (
            (status <> 'failed' AND failure_reason IS NULL)
            OR (status = 'failed' AND failure_reason IS NOT NULL)
        )
);

CREATE INDEX IF NOT EXISTS marketplace_installations_tenant_status_idx
    ON marketplace_extension_installations (tenant_id, status);

CREATE INDEX IF NOT EXISTS marketplace_installations_version_idx
    ON marketplace_extension_installations (extension_version_id);

ALTER TABLE marketplace_extension_installations ENABLE ROW LEVEL SECURITY;
-- FORCE makes the RLS policy apply even when the connection role is
-- the table owner (e.g. the migration runner role or a DBA performing
-- ad-hoc maintenance). Without FORCE, owner-role queries silently
-- bypass the USING/WITH CHECK clauses — fine for the application
-- pool (kapp_app, neither owner nor BYPASSRLS) but a defence-in-depth
-- gap if a future ops procedure or a backup-restore tool issues
-- cross-tenant reads under the owner role. The marketplace
-- installations table is the first tenant-scoped table in this
-- codebase to opt into FORCE; per-spec installations are the
-- write-amplification surface (one row per tenant per installed
-- extension, with operator-supplied webhook_base credentials in the
-- column), so we apply the stricter posture here and audit the rest
-- of the tenant-scoped tables for the same change in a separate
-- migration once we have RLS-aware tooling for kapp-backup verified
-- under the owner role.
ALTER TABLE marketplace_extension_installations FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS marketplace_extension_installations_isolation
    ON marketplace_extension_installations;
CREATE POLICY marketplace_extension_installations_isolation
    ON marketplace_extension_installations
    USING      (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON marketplace_extension_installations TO kapp_app;

-- Global catalog tables — kapp_app needs SELECT for listing endpoints,
-- and INSERT/UPDATE for the marketplace control surface (publisher
-- uploads land via the same role since publishers are tenants). The
-- review_state table is operator-write but tenant-read (every tenant
-- can see whether a version is approved before installing), so we
-- mirror the same grant pattern. Cross-tenant publishing is gated by
-- application-level authz (B6 endpoints), not RLS — these tables are
-- intentionally global.
GRANT SELECT, INSERT, UPDATE ON marketplace_extensions               TO kapp_app;
GRANT SELECT, INSERT, UPDATE ON marketplace_extension_versions       TO kapp_app;
GRANT SELECT, INSERT, UPDATE ON marketplace_extension_review_state   TO kapp_app;

COMMENT ON TABLE marketplace_extensions IS
    'Publisher-level extension listing. One row per (publisher, slug). Status drives marketplace visibility (unpublished/listed/deprecated/removed). The default install version is named in listed_version; specific versions live in marketplace_extension_versions.';
COMMENT ON TABLE marketplace_extension_versions IS
    'Immutable per-version bundle record. (extension_id, version) is UNIQUE; a re-upload returns 409. bundle_hash anchors install-time integrity verification (spec §10). Schema/compat columns are write-once via the BEFORE UPDATE trigger; only the yank fields can be updated post-publish.';
COMMENT ON TABLE marketplace_extension_review_state IS
    'Per-version operator review state. Populated by the B7 review pipeline: automated_checks captures the SAST / schema / size check results; manual_review_notes + reviewer + reviewed_at capture the human decision. status=approved is the gating signal for marketplace_extensions.status=listed.';
COMMENT ON TABLE marketplace_extension_installations IS
    'Tenant-scoped install state. RLS isolates rows per tenant. settings holds the per-tenant config (per spec §9 — JSON Schema-validated by the install API); webhook_base is the EXTENSION_WEBHOOK_BASE placeholder operators supply at install (spec §3.1) and B4 dispatches webhooks against. failure_reason is required iff status=failed.';

COMMENT ON COLUMN marketplace_extension_versions.manifest IS
    'Parsed kapp-extension.yaml as JSON. Source of truth for declarative fields the marketplace surfaces (description, license, capability declarations). The counts columns are denormalised hints maintained by the upload pipeline so listing queries do not have to JSON-parse on every render.';
COMMENT ON COLUMN marketplace_extension_versions.bundle_hash IS
    'SHA-256 hex of the raw .tar.gz upload. Install-time runtime re-fetches the bundle and verifies this hash before extracting (per spec §10) — tampering with bundle storage surfaces as an integrity error, not silent corruption.';
COMMENT ON COLUMN marketplace_extension_installations.webhook_base IS
    'Per-tenant EXTENSION_WEBHOOK_BASE supplied at install. Substituted for ${EXTENSION_WEBHOOK_BASE} placeholders in the manifest (spec §3.1) when B4 dispatches signed webhooks. HTTPS-only — enforced by both the install API validator and a DB CHECK as defence-in-depth.';
