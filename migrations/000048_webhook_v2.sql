-- Phase M Task 8 — Webhook v2 enhancements.
--
-- Adds three columns to the existing `webhooks` table:
--
--   * conditions JSONB — JSONB-expression filter evaluated against
--     the event payload. Lets a tenant subscribe e.g. only to
--     "krecord.updated where ktype=helpdesk.ticket and status=open".
--     The expression syntax is documented in
--     internal/notifications/webhook_conditions.go.
--   * max_retries INT DEFAULT 5 — overrides the worker-level
--     attempt ceiling so a tenant with a slow downstream can opt
--     into longer retry tails without code changes.
--   * backoff_base_seconds INT DEFAULT 10 — base for the exponential
--     backoff. Effective wait is base * 2^(attempt-1) plus a jitter
--     window so concurrent retries from many tenants don't pile
--     into the same wall-clock instant.
--
-- The existing `event_filters` JSONB column already implements the
-- "event_types" filter the spec references; we keep that name to
-- avoid breaking the existing /api/v1/webhooks contract and
-- packages/client/src/index.ts consumers. The spec name is
-- documented in the column comment for future readers.
--
-- All three additions are tenant_id-scoped via the existing RLS
-- policy on `webhooks` (no policy changes needed); the migration is
-- idempotent so a re-run is safe.

ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS conditions JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS max_retries INT NOT NULL DEFAULT 5;

ALTER TABLE webhooks
    ADD COLUMN IF NOT EXISTS backoff_base_seconds INT NOT NULL DEFAULT 10;

COMMENT ON COLUMN webhooks.event_filters IS
    'Event-type prefix filter (a.k.a. "event_types" in the v2 spec). '
    'JSONB array of strings; entries ending in "*" match by prefix. '
    'Empty array means "all events".';

COMMENT ON COLUMN webhooks.conditions IS
    'Optional JSONB-expression filter applied to the event payload '
    'BEFORE delivery. See internal/notifications/webhook_conditions.go '
    'for syntax. Empty object means "no condition; deliver all '
    'events that pass event_filters".';

COMMENT ON COLUMN webhooks.max_retries IS
    'Per-webhook attempt ceiling. Default 5, max 20.';

COMMENT ON COLUMN webhooks.backoff_base_seconds IS
    'Base seconds for the exponential backoff schedule. Effective '
    'wait between attempts is base * 2^(attempt-1) plus jitter.';
