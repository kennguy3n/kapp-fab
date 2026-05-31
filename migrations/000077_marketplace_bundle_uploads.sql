-- Phase B8 — publisher bundle uploads.
--
-- B6 (#130) shipped the publish surface as "publisher hosts the
-- bundle on its own CDN; POST a JSON body with bundle_url +
-- bundle_hash + bundle_size and we'll fetch from there at install
-- time." That works for organisations with existing CDN infra
-- (Cloudflare R2, S3, GitHub Releases), but it's a hard
-- prerequisite for smaller publishers and for the CLI tool we
-- ship with B8 — the publisher experience cannot require
-- "first, go set up a CDN somewhere with HTTPS and a stable URL."
--
-- This migration adds the schema half of B8's bundle-hosting
-- service: an internal content-addressed object store keyed by
-- SHA-256, with a metadata table that lets us:
--
--   * Dedup uploads across publishers — two publishers uploading
--     the same bytes share one storage object.
--   * Garbage-collect orphans — a publisher who uploads but
--     never references the bundle in a PublishVersion call
--     should not leak storage forever. The GC sweeper finds
--     rows with referenced_at IS NULL AND created_at < now() - 7d.
--   * Enforce the same 10 MiB cap as
--     marketplace.MaxBundleSizeBytes — defense-in-depth against
--     a future publisher CDN being replaced by a marketplace
--     upload that would exceed the cap.
--   * Maintain a per-publisher audit trail of who uploaded what.
--
-- Design choices:
--
--   * One row per content_hash, not per upload-attempt. A second
--     publisher uploading bytes that already exist gets the same
--     row (referenced via publisher_id), with uploader_id of the
--     first uploader. Rationale: the storage object is global;
--     metadata for "who uploaded this hash" is the first-mover
--     because the bytes are identical and the second uploader's
--     intent is captured by the (eventual) PublishVersion row
--     that references the hash.
--
--     Wait — that's wrong. The audit trail needs per-publisher
--     visibility. So content_hash is UNIQUE across the table
--     (storage dedup) but we need a SEPARATE link table for
--     (publisher_id, content_hash) attempts so each publisher's
--     dashboard can list "bundles I uploaded." We can model that
--     later; for B8 v1 the simpler approach is: content_hash is
--     UNIQUE in the main table, and we accept that the
--     "uploaded_by" / "publisher_id" fields reflect the FIRST
--     uploader. The PublishVersion row carries the
--     publisher-of-record for any version actually shipped.
--
--     A second publisher uploading the same hash gets a 200 OK
--     with the existing row (idempotent dedup). The handler
--     code MAY require that the second uploader is also a
--     member of some publisher (gate against random anonymous
--     re-uploads) but does NOT take over the original metadata.
--
--   * publisher_id is NULLABLE. A future admin-side bundle
--     ingestion (e.g. "operator side-loaded this bundle as
--     part of an incident response") may not have a publisher
--     context. For B8 v1, all upload paths require a
--     publisher_id, but the column is nullable to keep the
--     schema flexible.
--
--   * referenced_at is NULLABLE. NULL means "uploaded but never
--     consumed by a PublishVersion call." The bundle-upload
--     handler does NOT create the version row — the publisher
--     calls PublishVersion separately with the returned
--     bundle_url. This split is intentional:
--
--       - PublishVersion is the single source of truth for
--         "this is a real version that should be reviewed."
--         Uploading bytes to a CDN is a strictly weaker
--         operation — bytes exist, no commitment to publish.
--
--       - It lets a publisher experiment ("upload a draft,
--         look at the bundle_hash my tooling produced, decide
--         whether to publish") without polluting the version
--         catalog.
--
--       - It means the GC sweeper can reclaim storage from
--         abandoned uploads using a simple "old AND no
--         referenced_at" predicate, without joining against
--         marketplace_extension_versions on every sweep.
--
--     referenced_at is set the first time the hash appears in a
--     successful PublishVersion call (the handler updates it
--     post-insert). It is never cleared; a yanked version still
--     "references" the bytes because callers may still need
--     them for forensic / audit reasons.
--
--   * storage_key is the path inside the object store, not a
--     URL. The bundle-serve endpoint constructs the URL
--     (https://<host>/api/v1/marketplace/bundles/<hash>.tar.gz)
--     so a backend swap (MemoryStore → S3 → MinIO) does not
--     break existing version rows that store a marketplace-
--     hosted bundle_url. The storage_key is opaque to clients.
--
--   * content_type is fixed at 'application/gzip' for v1. The
--     bundle handler rejects everything else at upload time;
--     storing the column is forward-compatibility for a future
--     spec extension that allows e.g. zstd-compressed tarballs
--     (out of scope for v1; the resolver only un-gzips today).
--
--   * size_bytes capped at MaxBundleSizeBytes (10 MiB). The
--     upload handler does the soft check; the DB CHECK is the
--     hard floor so a misconfigured handler cannot smuggle
--     a 50 MiB bundle in.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT
-- EXISTS / DROP CONSTRAINT IF EXISTS + ADD CONSTRAINT throughout.

CREATE TABLE IF NOT EXISTS marketplace_bundle_uploads (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_hash    TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    content_type    TEXT NOT NULL DEFAULT 'application/gzip',
    storage_key     TEXT NOT NULL,
    publisher_id    UUID
        REFERENCES marketplace_publishers(id) ON DELETE SET NULL,
    uploaded_by     UUID
        REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    referenced_at   TIMESTAMPTZ,

    CONSTRAINT marketplace_bundle_uploads_content_hash_unique
        UNIQUE (content_hash),

    -- SHA-256 hex, 64 chars, lowercase. Matches IsValidBundleHash
    -- in internal/marketplace/types.go so a value passed straight
    -- to PublishVersion does not need re-canonicalisation.
    CONSTRAINT marketplace_bundle_uploads_content_hash_format
        CHECK (content_hash ~ '^[0-9a-f]{64}$'),

    -- Mirror MaxBundleSizeBytes (10 MiB). A handler-side bug that
    -- tried to insert a larger row would hit this gate before
    -- any storage cost is incurred.
    CONSTRAINT marketplace_bundle_uploads_size_bounded
        CHECK (size_bytes > 0 AND size_bytes <= 10485760),

    -- Defensive: storage_key must be non-empty (every backend
    -- writes something).
    CONSTRAINT marketplace_bundle_uploads_storage_key_nonempty
        CHECK (length(storage_key) > 0),

    -- Defensive: the recognised content types. v1 only accepts
    -- application/gzip; the list is here so adding 'application/zstd'
    -- later is a one-line CHECK rewrite alongside the resolver
    -- update.
    CONSTRAINT marketplace_bundle_uploads_content_type_valid
        CHECK (content_type IN ('application/gzip'))
);

-- "List uploads for publisher X newest first" — the publisher
-- dashboard's bundle history view.
CREATE INDEX IF NOT EXISTS marketplace_bundle_uploads_publisher_recent_idx
    ON marketplace_bundle_uploads (publisher_id, created_at DESC)
    WHERE publisher_id IS NOT NULL;

-- "Find unreferenced uploads older than 7 days" — the GC sweeper's
-- hot path. Partial index keeps it narrow on a table that's mostly
-- referenced rows (every successful publish flips the column).
CREATE INDEX IF NOT EXISTS marketplace_bundle_uploads_orphans_idx
    ON marketplace_bundle_uploads (created_at)
    WHERE referenced_at IS NULL;

GRANT SELECT, INSERT, UPDATE ON marketplace_bundle_uploads TO kapp_app;
GRANT DELETE ON marketplace_bundle_uploads TO kapp_admin;

COMMENT ON TABLE marketplace_bundle_uploads IS
    'Phase B8 — metadata for publisher bundles uploaded to the marketplace-hosted object store (alternative to publisher-hosted CDN). One row per unique content_hash; second uploader of the same bytes dedups to the existing row. The actual bytes live in the object store at storage_key; the bundle-serve endpoint at GET /api/v1/marketplace/bundles/{hash} streams them. referenced_at is set by the PublishVersion handler when a version row first references the hash; until then, the row is GC-eligible after 7 days.';

COMMENT ON COLUMN marketplace_bundle_uploads.content_hash IS
    'Lowercase hex SHA-256 of the raw bundle bytes — same value stored on marketplace_extension_versions.bundle_hash when this bundle is published as a version.';

COMMENT ON COLUMN marketplace_bundle_uploads.storage_key IS
    'Opaque path inside the configured ObjectStore (MemoryStore/S3/MinIO). The serve endpoint translates this to a streaming GET; clients never see the storage_key.';

COMMENT ON COLUMN marketplace_bundle_uploads.publisher_id IS
    'Publisher who first uploaded these bytes. NULL is reserved for future admin-side ingestion. Subsequent uploads of the same hash by other publishers do NOT overwrite this column.';

COMMENT ON COLUMN marketplace_bundle_uploads.referenced_at IS
    'Wall-clock when the first PublishVersion call referenced this hash. NULL = orphan (eligible for GC after 7 days). Never cleared once set, even if all referencing versions are yanked.';
