-- 000105_tenant_keys.sql
-- Phase 2 P2-2c: tenant_keys table + envelope encryption.
--
-- The current encryption model derives per-tenant keys via HKDF from a
-- single master key (KAPP_MASTER_KEY). This works for standard SME
-- tiers but has two limitations for business/regulated tiers:
--
--   1. Key rotation requires re-encrypting every field in every tenant
--      because the derived key changes when the master key changes.
--   2. There is no per-tenant key versioning — a rotation is all-or-
--      nothing across the entire platform.
--
-- Envelope encryption solves both:
--
--   - Each tenant gets a random 256-bit DEK (data encryption key)
--     generated once and stored in this table, wrapped by a KEK (key
--     encryption key) derived from the master key (standard tier) or
--     fetched from a KMS (business/regulated tier).
--   - Rotation creates a new DEK version; the old version remains
--     available for decrypting existing ciphertext. Re-encryption is
--     a background job that migrates rows from old to new version.
--   - The KEK never touches the database; only the wrapped DEK is
--     stored. Compromising the database does not reveal plaintext.
--
-- This migration creates the table; the runtime EnvelopeKeyManager
-- (internal/tenant/envelope.go) populates and reads it. The existing
-- HKDF-based KeyManager remains the default for standard tiers; the
-- envelope manager is opt-in via KAPP_ENVELOPE_ENCRYPTION=1.

CREATE TABLE IF NOT EXISTS tenant_keys (
    tenant_id    UUID    NOT NULL,
    key_version  INT     NOT NULL,
    -- wrapped_dek is the DEK encrypted by the KEK. Format:
    -- "kapp:wrap:v1:<base64(nonce+ciphertext)>". The KEK is derived
    -- from KAPP_MASTER_KEY via HKDF under the kapp.kek.v1 label (for
    -- standard tier) or fetched from a KMS (for business tier, future).
    wrapped_dek  TEXT    NOT NULL,
    -- kek_source identifies how the KEK was derived: "hkdf" (standard)
    -- or "kms" (business/regulated, future). The runtime uses this to
    -- select the correct unwrapping path.
    kek_source   TEXT    NOT NULL DEFAULT 'hkdf',
    -- status is "active" or "retired". Only one "active" version per
    -- tenant at a time; "retired" versions remain for decryption of
    -- existing ciphertext until re-encryption is complete. The CHECK
    -- constraint prevents invalid status values that would bypass the
    -- partial unique index below.
    status       TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'retired')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at   TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, key_version)
);

-- One active key per tenant. A partial unique index enforces this
-- without blocking retired rows.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_keys_active_uniq
    ON tenant_keys (tenant_id)
    WHERE status = 'active';

-- RLS: tenant_keys is control-plane data. The tenant-scoped app role
-- must NOT have direct access — the EnvelopeKeyManager reads via the
-- admin pool (kapp_admin) or a dedicated role. We revoke from PUBLIC
-- and grant only to admin roles.
REVOKE ALL ON tenant_keys FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON tenant_keys TO kapp_admin;
GRANT SELECT ON tenant_keys TO kapp_admin_readonly;
GRANT SELECT ON tenant_keys TO kapp_breakglass;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_keys TO kapp_admin_maintenance;
ALTER TABLE tenant_keys OWNER TO kapp_admin_maintenance;
