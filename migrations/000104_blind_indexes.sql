-- 000104_blind_indexes.sql
-- Phase 2 P2-2b: Blind indexes for encrypted lookup fields.
--
-- Encrypted fields cannot be queried via direct JSONB access because
-- the stored value is ciphertext. A blind index stores
-- HMAC(tenant_search_key, canonical(value)) in a separate JSONB
-- column so ListByField can match on the digest without decrypting.
--
-- The HMAC key is derived from the master key under a separate HKDF
-- label (kapp.blind.index.v1) so it is cryptographically independent
-- of the field-encryption key and the audit HMAC key. The digest is
-- truncated to 16 bytes (128 bits) and base64-encoded for storage.
--
-- Security properties:
--   * The index is deterministic for a given (tenant, field, value)
--     triple, so equality lookups work.
--   * The index does NOT reveal the plaintext value; it only reveals
--     equality/inequality. A rainbow-table attack against the index
--     is infeasible because the HMAC key is per-tenant and derived
--     from the master key.
--   * The index does NOT support range queries or prefix matching;
--     it is equality-only by design.

-- Add a JSONB column to krecords for blind index storage. Each key
-- in the JSON object is the field name; each value is the base64
-- HMAC digest. The column is nullable so existing rows (written
-- before blind indexes were introduced) remain valid — they simply
-- have no index entries and won't match blind-index queries.
ALTER TABLE krecords ADD COLUMN IF NOT EXISTS blind_indexes jsonb;

-- Partial index on the blind_indexes column to speed up equality
-- lookups for rows that actually have index entries. The index covers
-- (tenant_id, ktype, status) so the existing ListByField query shape
-- can push the blind-index predicate into an index scan.
CREATE INDEX IF NOT EXISTS krecords_blind_idx
    ON krecords (tenant_id, ktype, status)
    WHERE blind_indexes IS NOT NULL;

-- The kapp_admin and kapp_admin_maintenance roles need to read the
-- column for cross-tenant operations; the standard tenant-scoped role
-- (the one the app pool connects as) needs both read and write.
-- kapp_breakglass gets read for break-glass investigations.
GRANT SELECT (blind_indexes) ON krecords TO kapp_admin_readonly;
GRANT SELECT (blind_indexes) ON krecords TO kapp_breakglass;
