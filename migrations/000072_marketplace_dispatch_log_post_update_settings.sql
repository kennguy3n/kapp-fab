-- Migration 000072 — Extend dispatch_log kind enum for the
-- post_update_settings lifecycle hook (Phase 2a B6).
--
-- B3 shipped the install / uninstall lifecycle pair; B6 wires the
-- settings-update flow which fires its own best-effort lifecycle
-- hook so an extension can rotate operator-supplied secrets (e.g.
-- an EasyPost API key change) without a full uninstall/reinstall
-- cycle. The dispatch_log row is captured for forensic parity with
-- the install / uninstall lifecycle hooks.
--
-- This is an idempotent CHECK rewrite. CHECK constraints cannot
-- be ALTERed in place; we drop and re-create with the extended
-- enum. Existing rows already in the table are unaffected (the
-- new enum is a superset of the old).

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
        'event_delivery',
        'health_check'
    ));
