-- Phase J/K — Per-tenant ZK Object Fabric placement policy.
--
-- The ZK fabric supports per-tenant placement policies (provider
-- allow-list, country residency, encryption mode, cache hint). Kapp
-- mirrors the active policy on the tenants row so:
--
--   * the wizard can persist the policy it computed from the plan
--     tier + tenant locale alongside the credentials it minted, in
--     one transaction, even before the fabric console PUT lands.
--   * `GET /api/v1/tenants/{id}/placement` can serve the local copy
--     without round-tripping to the fabric on every read.
--   * `PUT /api/v1/tenants/{id}/placement` validates against the
--     fabric schema, then forwards to the console — Kapp keeps the
--     local row in lock-step.
--
-- `zk_fabric_endpoint` lets paid tiers route to dedicated cells
-- (e.g. enterprise tenants on regional fabric clusters) without
-- changing the global `ZK_FABRIC_ENDPOINT` default. NULL means
-- "use the platform default".
--
-- Backward compatibility: both columns default to NULL so existing
-- tenants keep using the platform-wide pooled policy until the
-- wizard re-runs (idempotent) or the operator sets the policy via
-- the API.

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS placement_policy   JSONB DEFAULT NULL,
    ADD COLUMN IF NOT EXISTS zk_fabric_endpoint TEXT  DEFAULT NULL;

COMMENT ON COLUMN tenants.placement_policy IS
    'Active ZK Object Fabric placement policy (JSONB). NULL = platform default pooled policy.';
COMMENT ON COLUMN tenants.zk_fabric_endpoint IS
    'Per-tenant ZK fabric endpoint override. NULL = global ZK_FABRIC_ENDPOINT.';
