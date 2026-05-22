-- Phase 2.4: Add content_hash column to support lazy KType registration.
-- RegisterIfChanged computes a deterministic SHA-256 of the serialized
-- KType and compares it against the stored hash before writing. Unchanged
-- KTypes skip the upsert entirely, eliminating 50+ unnecessary writes on
-- every cold-start replica boot.
ALTER TABLE ktypes ADD COLUMN IF NOT EXISTS content_hash TEXT;
