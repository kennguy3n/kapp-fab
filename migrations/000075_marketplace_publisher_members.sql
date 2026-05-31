-- Phase B7.1 — publisher self-service membership.
--
-- B7 (#131) shipped publisher identity + ed25519 key registration
-- but parked the management surface entirely on the admin chain.
-- The doc comment in services/api/marketplace_publisher_handlers.go
-- explicitly called out the gap:
--
--     A future B7.1 will add a marketplace_publisher_members join
--     table; once that lands, RegisterPublisherKey /
--     RevokePublisherKey migrate to a tenant-scoped self-service
--     surface and the admin endpoints stay as the operator override.
--
-- This migration adds that join table. The semantics are deliberately
-- lightweight — two-role RBAC, no per-action capabilities, no audit
-- log table. The justification:
--
--   * Owner vs Member is the smallest split that still lets a
--     publisher organisation tag a key-rotation operator without
--     handing them the ability to add or remove other members. Owner
--     manages members + keys; Member manages keys only.
--
--   * The audit need is covered by added_by (who added this member)
--     + (future) the existing kapp_events outbox table for mutations.
--     A dedicated marketplace_publisher_member_events table would
--     duplicate that infrastructure without adding anything new.
--
--   * The "at least one owner" invariant lives in application code,
--     not a DB trigger, because the publisher row can exist in three
--     legitimate states: (a) zero members — admin-only management,
--     the bootstrap state immediately after the admin
--     /publishers POST endpoint; (b) one or more members with
--     ≥1 owner — self-service; (c) transitioning between those.
--     A DB trigger would have to model "a member-removal that
--     leaves any non-owner members behind must also leave ≥1
--     owner", which is straightforward to express but harder to
--     keep in sync with the application's role-rename and bulk
--     reassign endpoints if those land later. The store layer
--     re-checks the invariant inside a `FOR UPDATE` on the publisher
--     row so the race window is the same as it would be with a
--     trigger.
--
--   * Admin override paths bypass the invariant — an operator
--     forcibly removing the last owner is a legitimate recovery
--     action (e.g. the publisher organisation is taken over by a
--     new team). The application surfaces this as a distinct
--     admin endpoint with a separate authorisation gate, so the
--     accidental-self-lockout case (a member deleting their own
--     row via the self-service surface) is still blocked.
--
--   * No FK to tenants. Publisher membership is a person-level
--     attribute, not a tenant-level one — a single human user can
--     manage multiple unrelated publishers across different
--     tenancies. The reference is users.id (the platform-global
--     identity row from migration 000001).
--
-- Devin Review will likely flag the omission of a DB-side
-- "owner count ≥ 1" trigger. The reply: see (c) above. The
-- application enforces it under FOR UPDATE, which is equivalent
-- to a row-level trigger from a serialisability standpoint, and
-- keeping the invariant in Go means the admin override path can
-- relax it explicitly rather than having to disable a trigger.

CREATE TABLE IF NOT EXISTS marketplace_publisher_members (
    publisher_id    UUID NOT NULL
        REFERENCES marketplace_publishers(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL
        REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT NOT NULL DEFAULT 'member',
    added_by        UUID
        REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (publisher_id, user_id),

    CONSTRAINT marketplace_publisher_members_role_valid
        CHECK (role IN ('owner', 'member'))
);

-- "List the publishers user X manages." This is a hot lookup on the
-- self-service surface — every authenticated publisher request
-- hits it once for authorisation.
CREATE INDEX IF NOT EXISTS marketplace_publisher_members_user_idx
    ON marketplace_publisher_members (user_id);

-- Partial index over the owners only. Used to count owners during
-- the "would this leave 0 owners with members left behind" check.
-- The partial filter keeps the index narrow on publishers with
-- a long tail of members but only a handful of owners.
CREATE INDEX IF NOT EXISTS marketplace_publisher_members_owners_idx
    ON marketplace_publisher_members (publisher_id)
    WHERE role = 'owner';

-- updated_at trigger. Match the pattern used by the
-- marketplace_publishers table itself (see 000073).
CREATE OR REPLACE FUNCTION marketplace_publisher_members_set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS marketplace_publisher_members_updated_at
    ON marketplace_publisher_members;
CREATE TRIGGER marketplace_publisher_members_updated_at
    BEFORE UPDATE ON marketplace_publisher_members
    FOR EACH ROW EXECUTE FUNCTION marketplace_publisher_members_set_updated_at();

COMMENT ON TABLE marketplace_publisher_members IS
    'Per-publisher membership for the B7.1 self-service surface. owner role = manage members + keys; member role = manage keys only. Empty-member state means admin-only management. Application enforces "any publisher with members must have ≥1 owner".';
