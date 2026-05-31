-- Phase B7 — marketplace publisher identity, code-signing, and the
-- structured findings table backing the automated review pipeline.
--
-- B6 (#130) shipped the marketplace catalog HTTP surface but left the
-- per-version review state in `submitted` until a human admin manually
-- transitioned it. The `automated_checks::jsonb` column was unused.
-- B7 closes that gap with a real automated pipeline; this migration
-- adds the data model the pipeline reads + writes:
--
--   1. marketplace_publishers         — publisher identity & verification
--   2. marketplace_publisher_keys     — ed25519 public keys per publisher
--                                       used to verify bundle signatures
--   3. marketplace_extension_versions — three new columns capturing the
--      bundle's optional ed25519 signature
--   4. marketplace_review_findings    — append-once structured findings
--      the automated checks produce
--
-- Design choices anchored to docs/EXTENSION_SPEC.md §10 ("Bundle
-- Hashing & Signing"):
--
--   * Ed25519, not PGP, in v1. The spec calls signing "optional, Phase 2"
--     and leaves the algorithm open. We pick raw ed25519 because the
--     platform key manager already supports it, the public key fits in
--     a single TEXT column (32 bytes b64 → 44 chars), and there is no
--     keyring-management surface to maintain. PGP can be added as a
--     second `algorithm` value without a schema change.
--
--   * Signature is publisher-policy, not per-version policy. The
--     pipeline asks "does this publisher have any non-revoked keys?"
--     — if yes, an unsigned new version is auto-rejected; if no,
--     unsigned is accepted with an info-level finding. This lets the
--     rollout happen without breaking pre-B7 publishers. Once a
--     publisher registers their first key, the policy flips on for
--     them. The DB does NOT enforce this at the row level (a version
--     row can be inserted unsigned regardless of publisher state) —
--     the policy lives in the review pipeline so it can degrade to a
--     finding rather than a constraint violation.
--
--   * Sign-bytes are raw bundle bytes, NOT the hash. Signing the hash
--     would let an attacker who controls the publisher-key pair forge
--     a new bundle whose hash collides with the signed-over hash via
--     length extension on the surrounding store row — admittedly a
--     theoretical attack today, but with ed25519 the raw-bytes shape
--     is the documented best practice (signed-data is what you intend
--     to authenticate; in our case that's the .tar.gz body that
--     bundle.HTTPResolver streams off the CDN).
--
--   * marketplace_publishers is backfilled from the distinct
--     `publisher` column on marketplace_extensions so existing
--     catalog rows are not orphaned at deploy time. The backfill
--     creates a row with display_name = publisher slug and a
--     synthesised noreply@<publisher>.invalid contact email; the
--     operator-side verify endpoint lets admins replace those with
--     real values during the first verification action.
--
--   * marketplace_review_findings uses (extension_version_id,
--     check_name, code, location) as the natural key so a re-scan
--     of the same version replaces rather than duplicates findings.
--     We model it as a real UNIQUE constraint + ON CONFLICT DO UPDATE
--     in the Go layer; the alternative (DELETE-then-INSERT) would
--     race with the per-version FOR UPDATE on the review_state row
--     and risks dropping a finding the publisher is currently
--     reading.

-- 1) marketplace_publishers ------------------------------------------------

CREATE TABLE IF NOT EXISTS marketplace_publishers (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                 TEXT NOT NULL UNIQUE,
    display_name         TEXT NOT NULL,
    contact_email        TEXT NOT NULL,
    verified_at          TIMESTAMPTZ,
    verified_by          TEXT,
    verification_notes   TEXT,
    auto_approve_patch   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT marketplace_publishers_slug_format
        CHECK (slug ~ '^[a-z][a-z0-9_]{2,31}$'),
    CONSTRAINT marketplace_publishers_email_format
        CHECK (contact_email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
    CONSTRAINT marketplace_publishers_verified_metadata
        CHECK (
            (verified_at IS NULL AND verified_by IS NULL)
            OR (verified_at IS NOT NULL AND verified_by IS NOT NULL)
        ),
    CONSTRAINT marketplace_publishers_auto_approve_requires_verified
        CHECK (auto_approve_patch = FALSE OR verified_at IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS marketplace_publishers_verified_idx
    ON marketplace_publishers (verified_at)
    WHERE verified_at IS NOT NULL;

COMMENT ON TABLE marketplace_publishers IS
    'Publisher identity, verification status, and the gate for the verified badge on listings. One row per distinct publisher slug; backfilled at migration time from the distinct publisher values already present on marketplace_extensions so legacy rows are not orphaned.';
COMMENT ON COLUMN marketplace_publishers.auto_approve_patch IS
    'Future fast-path for verified publishers with a track record: when true, patch-version bumps (z in x.y.z) skip the manual_review step if every automated check passes. B7 ships the column but always sets it false; B7.1 wires the fast-path logic.';

-- Backfill: every distinct publisher already in the catalog whose
-- slug satisfies the new strict regex gets a row so the FK from
-- marketplace_extension_versions.bundle_signature_key_id (which
-- joins through this table) has somewhere to land.
--
-- The pre-B7 marketplace_extensions.publisher CHECK enforces only
-- '^[a-z][a-z0-9_]*$' (any length ≥ 1); this new table tightens to
-- length 3-32. Any pre-existing publisher slug shorter than 3
-- chars or longer than 32 chars would fail the new CHECK at INSERT
-- time and break the migration. We WHERE-filter the backfill so
-- non-conforming legacy slugs are skipped — silently rewriting the
-- slug here would orphan the publisher's existing catalog rows
-- (marketplace_extensions.publisher is the de-facto identifier
-- and is immutable post-publish per the 000068 trigger), so the
-- correct migration shape is to leave legacy non-conforming rows
-- without a marketplace_publishers row. There is no FK from
-- marketplace_extensions.publisher → marketplace_publishers.slug
-- so existing catalog rows remain functional.
--
-- Devin Review ANALYSIS_0002 on commit 6783035.
--
-- Operators can audit which legacy slugs were skipped with:
--   SELECT DISTINCT publisher FROM marketplace_extensions
--    WHERE publisher !~ '^[a-z][a-z0-9_]{2,31}$';
-- and create publisher rows for them via the admin /publishers
-- POST endpoint with a normalised slug.
INSERT INTO marketplace_publishers (slug, display_name, contact_email)
SELECT DISTINCT
    e.publisher,
    e.publisher,
    'noreply@' || e.publisher || '.invalid'
FROM marketplace_extensions e
WHERE e.publisher ~ '^[a-z][a-z0-9_]{2,31}$'
ON CONFLICT (slug) DO NOTHING;

-- 2) marketplace_publisher_keys --------------------------------------------

CREATE TABLE IF NOT EXISTS marketplace_publisher_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id    UUID NOT NULL REFERENCES marketplace_publishers(id) ON DELETE CASCADE,
    key_id          TEXT NOT NULL,
    algorithm       TEXT NOT NULL DEFAULT 'ed25519',
    public_key_b64  TEXT NOT NULL,
    label           TEXT,
    revoked_at      TIMESTAMPTZ,
    revoked_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT marketplace_publisher_keys_key_id_format
        CHECK (key_id ~ '^[A-Za-z0-9_\-]{4,64}$'),
    CONSTRAINT marketplace_publisher_keys_algorithm_valid
        CHECK (algorithm IN ('ed25519')),
    -- ed25519 public key is 32 bytes → standard b64 encoding is 44
    -- chars (with one '=' padding). Tighten to the exact shape to
    -- catch typos / wrong-format pastes early.
    CONSTRAINT marketplace_publisher_keys_pubkey_format
        CHECK (
            algorithm <> 'ed25519'
            OR public_key_b64 ~ '^[A-Za-z0-9+/]{43}=$'
        ),
    CONSTRAINT marketplace_publisher_keys_revoked_metadata
        CHECK (
            (revoked_at IS NULL AND revoked_reason IS NULL)
            OR (revoked_at IS NOT NULL AND revoked_reason IS NOT NULL)
        ),
    CONSTRAINT marketplace_publisher_keys_unique_per_publisher
        UNIQUE (publisher_id, key_id)
);

CREATE INDEX IF NOT EXISTS marketplace_publisher_keys_publisher_active_idx
    ON marketplace_publisher_keys (publisher_id)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE marketplace_publisher_keys IS
    'Ed25519 public keys registered by publishers for bundle signing. Multiple keys per publisher supports rotation: register the new key, sign new uploads with it, then revoke the old key. The pipeline considers any non-revoked key valid; the revoked_at column is the cutoff (signatures over bundles uploaded BEFORE revocation remain valid forever, since the signed bytes are immutable and the version row is immutable).';

-- 3) marketplace_extension_versions — signature columns --------------------

-- Add nullable columns rather than NOT NULL with default so the
-- immutability trigger in 000068 does not have to special-case them.
-- The trigger compares NEW vs OLD via IS DISTINCT FROM, which is NULL-safe.
ALTER TABLE marketplace_extension_versions
    ADD COLUMN IF NOT EXISTS bundle_signature        TEXT,
    ADD COLUMN IF NOT EXISTS bundle_signature_key_id TEXT,
    ADD COLUMN IF NOT EXISTS signed_at               TIMESTAMPTZ;

-- Signature payload is base64 of a 64-byte ed25519 signature → 88
-- chars without padding (signatures aren't padded with =). Match a
-- liberal b64 shape since the publisher-keys.key_id we reference is
-- a TEXT identifier we already format-check independently.
ALTER TABLE marketplace_extension_versions
    DROP CONSTRAINT IF EXISTS marketplace_extension_versions_signature_format;
ALTER TABLE marketplace_extension_versions
    ADD CONSTRAINT marketplace_extension_versions_signature_format
    CHECK (
        bundle_signature IS NULL
        OR bundle_signature ~ '^[A-Za-z0-9+/]{86}==$'
    );

-- All three signature columns are either all-null or all-set. A row
-- with only signed_at populated would be meaningless (we couldn't tell
-- which key signed); a row with only bundle_signature populated breaks
-- the signature-verify lookup because we'd have no key_id to resolve
-- against marketplace_publisher_keys.
ALTER TABLE marketplace_extension_versions
    DROP CONSTRAINT IF EXISTS marketplace_extension_versions_signature_consistent;
ALTER TABLE marketplace_extension_versions
    ADD CONSTRAINT marketplace_extension_versions_signature_consistent
    CHECK (
        (bundle_signature IS NULL AND bundle_signature_key_id IS NULL AND signed_at IS NULL)
        OR (bundle_signature IS NOT NULL AND bundle_signature_key_id IS NOT NULL AND signed_at IS NOT NULL)
    );

CREATE INDEX IF NOT EXISTS marketplace_extension_versions_signed_idx
    ON marketplace_extension_versions (signed_at)
    WHERE signed_at IS NOT NULL;

-- Patch the existing immutability trigger to also lock the signature
-- columns. A signed version cannot be re-signed; if a publisher loses
-- their signing key they must publish a new version. (The trigger
-- function is REPLACEd here, picking up the new column checks.)
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
    -- Signature is write-once: once set, a publisher cannot re-sign or
    -- un-sign the version. Setting NULL→value is allowed (e.g. a
    -- legacy unsigned row that the publisher later attests to via a
    -- separate signed attestation endpoint). Going value→NULL or
    -- value→different-value is not.
    IF OLD.bundle_signature IS NOT NULL
       AND NEW.bundle_signature IS DISTINCT FROM OLD.bundle_signature THEN
        RAISE EXCEPTION 'marketplace_extension_versions.bundle_signature is immutable once set';
    END IF;
    IF OLD.bundle_signature_key_id IS NOT NULL
       AND NEW.bundle_signature_key_id IS DISTINCT FROM OLD.bundle_signature_key_id THEN
        RAISE EXCEPTION 'marketplace_extension_versions.bundle_signature_key_id is immutable once set';
    END IF;
    IF OLD.signed_at IS NOT NULL
       AND NEW.signed_at IS DISTINCT FROM OLD.signed_at THEN
        RAISE EXCEPTION 'marketplace_extension_versions.signed_at is immutable once set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- 4) marketplace_review_findings -------------------------------------------

CREATE TABLE IF NOT EXISTS marketplace_review_findings (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    extension_version_id     UUID NOT NULL REFERENCES marketplace_extension_versions(id) ON DELETE CASCADE,
    check_name               TEXT NOT NULL,
    severity                 TEXT NOT NULL,
    code                     TEXT NOT NULL,
    message                  TEXT NOT NULL,
    location                 TEXT NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT marketplace_review_findings_severity_valid
        CHECK (severity IN ('error','warn','info')),
    CONSTRAINT marketplace_review_findings_check_name_format
        CHECK (check_name ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
    CONSTRAINT marketplace_review_findings_code_format
        CHECK (code ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$'),
    -- Natural key for ON CONFLICT idempotent re-scans. location is
    -- defaulted to '' rather than NULL so the UNIQUE actually works
    -- (NULL ≠ NULL in PostgreSQL b-tree dedup pre-15, and we want
    -- "no location" to be a real value the pipeline can deduplicate
    -- against).
    CONSTRAINT marketplace_review_findings_natural_key
        UNIQUE (extension_version_id, check_name, code, location)
);

CREATE INDEX IF NOT EXISTS marketplace_review_findings_version_severity_idx
    ON marketplace_review_findings (extension_version_id, severity);

COMMENT ON TABLE marketplace_review_findings IS
    'Structured findings produced by B7''s automated review pipeline. One row per (version, check, code, location). The pipeline upserts via the natural-key UNIQUE on re-scan so existing finding ids remain stable for the publisher UI. severity=error blocks the version; severity=warn/info surfaces as advisory output on the listing detail page.';
COMMENT ON COLUMN marketplace_review_findings.check_name IS
    'Name of the check that produced this finding — e.g. manifest_schema, bundle_size, permission_scope, ui_static, signature. Matches the Check.Name() return value in internal/marketplace/review.';
COMMENT ON COLUMN marketplace_review_findings.code IS
    'Machine-parsable identifier for the specific finding within a check — e.g. permission.unused, ui.eval, ktype.namespace_mismatch. Stable across pipeline runs so publishers can write CI assertions against specific codes.';
COMMENT ON COLUMN marketplace_review_findings.location IS
    'Best-effort location pointer for the finding: file path inside the bundle when applicable, otherwise '''' (empty string). Used in the natural-key UNIQUE so two findings with the same code but different locations remain distinct rows.';

-- 5) marketplace_extension_review_state.claim_* columns -------------------
--
-- B7's review worker drains the queue by SELECTing rows in
-- `submitted` and processing them. Without a transactional lease
-- column the SELECT FOR UPDATE SKIP LOCKED would run inside the
-- pool's per-statement transaction and the row locks would release
-- at end-of-statement, leaving a leader-handover or rapid-retick
-- race where two workers process the same version concurrently.
--
-- We add claimed_at + claimed_by to convert the SELECT-then-process
-- pattern into an atomic UPDATE...RETURNING (the canonical Postgres
-- job-queue pattern):
--
--   UPDATE ... SET claimed_at = now(), claimed_by = $worker
--    WHERE id IN (
--        SELECT ... FROM ... WHERE status='submitted'
--          AND (claimed_at IS NULL OR claimed_at < now()-interval)
--        FOR UPDATE SKIP LOCKED
--        LIMIT $batch
--    )
--   RETURNING ...
--
-- The atomic UPDATE inside the SKIP LOCKED scope is what gives
-- exactly-one-claimer semantics: the row's claimed_at flips
-- non-NULL inside the same statement that's holding the lock, so
-- a concurrent worker's SKIP LOCKED scan sees either the locked
-- row (skipped) or the post-update row (filtered out by
-- claimed_at IS NULL guard).
--
-- Lease expiry: a worker that claims a row then crashes mid-pipeline
-- leaves the row with claimed_at set and status still `submitted`.
-- The expiry clause (`claimed_at < now() - interval`) means the
-- next worker tick re-claims it after the lease lapses. The lease
-- is set to 10 minutes in code: longer than the 90s per-version
-- pipeline timeout (so a healthy worker never races itself) and
-- short enough that operator restarts don't strand work for hours.
--
-- claimed_by stores the worker's hostname for forensic debugging
-- (which replica was handling this version when it stalled). Not
-- used for locking decisions — SKIP LOCKED handles that.
ALTER TABLE marketplace_extension_review_state
    ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claimed_by TEXT;

-- Partial index: only rows that have been claimed are interesting
-- for the lease-expiry scan; the vast majority of rows have
-- claimed_at = NULL and we don't want to bloat the index with
-- them.
CREATE INDEX IF NOT EXISTS marketplace_extension_review_state_claimed_at_idx
    ON marketplace_extension_review_state (claimed_at)
    WHERE claimed_at IS NOT NULL;

COMMENT ON COLUMN marketplace_extension_review_state.claimed_at IS
    'Set to now() when the review worker atomically claims this version for processing (status=''submitted'' + claimed_at IS NULL guard). Cleared back to NULL by the worker''s state-transition write at the end of pipeline.Persist. A claimed_at older than the lease (10 minutes) means the claiming worker crashed; the next tick re-claims the row.';
COMMENT ON COLUMN marketplace_extension_review_state.claimed_by IS
    'Hostname of the worker currently holding the claim. Forensic-only — SKIP LOCKED + the claimed_at lease enforce exactly-one-claimer; claimed_by tells the operator which replica was running the pipeline when the row went stale.';
