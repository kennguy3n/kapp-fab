-- Phase H — audit log hash chain.
--
-- SECURITY_REVIEW.md §8 flagged that an append-only audit log is not
-- enough: if an attacker compromises the DB they can DELETE or UPDATE
-- past rows without leaving evidence. We solve that by hash-chaining
-- each row to its tenant-scoped predecessor:
--
--     row_hash = SHA256(
--         prev_hash ||
--         tenant_id ||
--         target_id ||
--         action ||
--         before || after || context ||
--         created_at
--     )
--
-- `prev_hash` stores the hash of the previous row in the same tenant.
-- `row_hash` stores this row's computed hash. A verifier scans the
-- table in (tenant_id, id) order and reports the first row whose
-- recomputed hash does not match its stored value, or whose prev_hash
-- does not match the prior row's row_hash — either case is tampering.
--
-- Both columns are nullable to preserve backward compatibility with
-- pre-chain rows. The logger treats NULL prev_hash on the very first
-- row per tenant as a zero-seed.

ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS prev_hash BYTEA;
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS row_hash  BYTEA;
