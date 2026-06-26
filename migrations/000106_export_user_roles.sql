-- 000106_export_user_roles.sql
-- Phase 2 P2-3a: Store the requesting user's roles on the export job
-- so the worker can apply field_permissions redaction at export time.
--
-- The export pipeline is asynchronous: the API handler enqueues a job,
-- the worker picks it up later. The user's roles at enqueue time are
-- the authoritative set for the export — if roles change between
-- enqueue and processing, the export reflects the permissions the
-- user had when they requested it, which is the correct audit posture.
--
-- Stored as a JSONB array of role strings. NULL when the job was
-- created by a system actor (no roles) — the worker treats NULL as
-- "no field_permissions filtering" which is the legacy behaviour.

ALTER TABLE export_jobs ADD COLUMN IF NOT EXISTS user_roles jsonb;

-- No additional grants needed: the existing export_jobs grants cover
-- the new column. kapp_admin already has SELECT on export_jobs for
-- the cross-tenant download path.
