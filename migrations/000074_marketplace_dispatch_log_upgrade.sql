-- Migration 000074 — Extend dispatch_log kind enum for the
-- pre_upgrade / post_upgrade lifecycle hooks (Phase 2a B6.1).
--
-- B6 shipped install + uninstall + update_settings; B6.1 adds
-- Engine.Upgrade — an in-place version swap that re-registers the
-- runtime tables (ktypes / workflows / agent_tools / webhook
-- subscriptions) inside a single transaction so the (tenant,
-- extension) install row never observes a half-upgraded state.
--
-- The upgrade flow fires two lifecycle hooks for parity with
-- install / uninstall:
--
--   pre_upgrade   — BLOCKING. The extension can reject the
--                   upgrade (e.g. data migration not ready,
--                   refuse downgrade) and the engine returns
--                   ErrPreUpgradeRejected without touching
--                   the DB. dispatch_log captures the attempt.
--   post_upgrade  — BEST-EFFORT. Fires after the in-tx
--                   re-registration commits. A failure is
--                   logged but does NOT roll back the upgrade
--                   (the runtime tables are already swapped).
--
-- This is the same CHECK-rewrite shape as migration 000072 — the
-- enum is a strict superset of the old one, so existing rows
-- remain valid.

ALTER TABLE marketplace_dispatch_log
    DROP CONSTRAINT IF EXISTS marketplace_dispatch_log_kind_chk;

ALTER TABLE marketplace_dispatch_log
    ADD CONSTRAINT marketplace_dispatch_log_kind_chk
    CHECK (kind IN (
        'tool_invoke',
        'lifecycle_pre_install',
        'lifecycle_post_install',
        'lifecycle_pre_uninstall',
        'lifecycle_post_uninstall',
        'lifecycle_post_update_settings',
        'lifecycle_pre_upgrade',
        'lifecycle_post_upgrade',
        'event_delivery',
        'health_check'
    ));
