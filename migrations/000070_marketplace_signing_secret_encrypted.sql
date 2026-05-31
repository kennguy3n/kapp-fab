-- Loosen marketplace_installations_signing_secret_format_chk to
-- accept the *.KeyManager AES-256-GCM envelope shape produced by
-- internal/tenant/encryption.go: `kapp:enc:v1:` + base64-std-encoded
-- (nonce ‖ ciphertext ‖ GCM tag).
--
-- Motivation: Devin Review ANALYSIS_0004 on PR #128 flagged that
-- signing_secret was stored in plaintext on the row, which meant
-- kapp-backup's `SELECT row_to_json(t)` dump leaked the per-install
-- HMAC key in plaintext into every backup file. The architecturally
-- correct fix is column-level encryption at rest using the existing
-- per-tenant *tenant.KeyManager — the same machinery the record
-- store uses for {"encrypted": true} fields. With that wired:
--
--   * Engine.Install EncryptString(secret) before INSERT.
--   * Engine.Uninstall / Dispatcher.Invoke DecryptString on read.
--   * kapp-backup row_to_json inherits ciphertext automatically —
--     no kapp-backup-side column allowlist needed.
--
-- The original constraint (migration 000069) only accepted:
--   * '' (legacy fixtures predating B3 that INSERT directly)
--   * 43-char base64url (the raw secret format, dev mode without
--     KAPP_MASTER_KEY).
--
-- The new constraint additionally accepts the encrypted envelope:
--   * '^kapp:enc:v1:[A-Za-z0-9+/=]+$'
--
-- We keep the legacy plaintext branches so dev/test environments
-- without KAPP_MASTER_KEY (the noopEncryptor path in
-- internal/marketplace/runtime/encryptor.go) keep working without a
-- separate "encrypted-vs-plaintext" toggle. Production deploys MUST
-- set KAPP_MASTER_KEY (already required by the record store), and
-- the engine then writes ciphertext that matches the envelope
-- branch. Any future migration that wants to enforce encrypted-only
-- can drop the 43-char base64url branch once all rows are confirmed
-- migrated; for now we keep both shapes valid because there is no
-- backfill path needed (the column is brand-new in 000069 with
-- DEFAULT '').
--
-- DROP + ADD is wrapped in a single DO block so a re-run is a
-- no-op. ADD CONSTRAINT does not have an IF NOT EXISTS form, so we
-- pg_constraint-check before re-creating to mirror the idempotency
-- pattern in migration 000069.

ALTER TABLE marketplace_extension_installations
    DROP CONSTRAINT IF EXISTS marketplace_installations_signing_secret_format_chk;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'marketplace_installations_signing_secret_format_chk'
    ) THEN
        ALTER TABLE marketplace_extension_installations
            ADD CONSTRAINT marketplace_installations_signing_secret_format_chk
            CHECK (
                signing_secret = ''
                OR signing_secret ~ '^[A-Za-z0-9_-]{43}$'
                OR signing_secret ~ '^kapp:enc:v1:[A-Za-z0-9+/=]+$'
            );
    END IF;
END $$;
