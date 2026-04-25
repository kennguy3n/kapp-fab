-- Phase J cont. — per-tenant ZK Object Fabric credentials.
--
-- Each tenant's file attachments live in a per-tenant bucket on the
-- ZK Object Fabric gateway, encrypted under per-tenant DEKs that the
-- gateway derives from the tenant's HMAC credentials. Storing the
-- credentials on the `tenants` row keeps the attachment layer
-- stateless: the request path looks up the row, derives an
-- `S3StoreConfig` for the request's tenant, and delegates the put /
-- get exactly the same way the global MinIO-backed store does — see
-- internal/files/zk_fabric.go.
--
-- Backward compatibility: when all three columns are NULL the
-- attachment store falls back to the global S3_BUCKET / S3_ENDPOINT
-- env vars (i.e. MinIO in dev, the old per-cell bucket in legacy
-- deploys). Tenants provisioned by the wizard after this migration
-- get the fields populated by `wizard.RunSetupWizard` so newly
-- created tenants are ZK-encrypted by default.

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS zk_access_key TEXT,
    ADD COLUMN IF NOT EXISTS zk_secret_key TEXT,
    ADD COLUMN IF NOT EXISTS zk_bucket     TEXT;

-- The credentials carry HMAC secrets so they MUST stay readable
-- only by the kapp_app role (RLS does not apply to the tenants
-- table — it is control-plane). The default GRANT ON tenants
-- inherits SELECT/UPDATE so no extra privilege change is needed
-- here; the comment is informational.
COMMENT ON COLUMN tenants.zk_access_key IS
    'ZK Object Fabric HMAC access key (per-tenant). NULL = fall back to global S3_BUCKET.';
COMMENT ON COLUMN tenants.zk_secret_key IS
    'ZK Object Fabric HMAC secret key (per-tenant). NULL = fall back to global S3_BUCKET.';
COMMENT ON COLUMN tenants.zk_bucket IS
    'ZK Object Fabric bucket name (per-tenant). NULL = fall back to global S3_BUCKET.';
