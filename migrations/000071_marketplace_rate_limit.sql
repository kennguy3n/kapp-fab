-- B4 — per-extension dispatch rate limit.
--
-- The marketplace event router (internal/marketplace/eventrouter)
-- and the agent-tool dispatcher (internal/marketplace/runtime/
-- dispatcher.go) both fan out signed HTTPS POSTs to an extension's
-- webhook. Both consume from a single shared budget per
-- (tenant_id, extension_id) because the rate limit is a property
-- of the extension's webhook receiver (its ingress capacity), not
-- of the dispatcher's call path.
--
-- Default 100 RPM matches the spec sketch in the B4 plan; operators
-- can raise/lower per extension via the publisher API once B6
-- ships. The value is on the catalog row (marketplace_extensions),
-- not the per-version row, because rate limits are typically a
-- function of the extension's hosting tier rather than its
-- semantic version.
--
-- Bounds: 1..10000. Below 1 the extension can never be invoked;
-- above 10000 is the ceiling we want for hot agent-tool paths
-- without letting a malicious publisher disable the limiter
-- entirely. Operators with a verified high-volume extension can
-- have the value raised by a marketplace admin.

ALTER TABLE marketplace_extensions
    ADD COLUMN IF NOT EXISTS rate_limit_rpm INTEGER NOT NULL DEFAULT 100;

ALTER TABLE marketplace_extensions
    DROP CONSTRAINT IF EXISTS marketplace_extensions_rate_limit_rpm_chk;

ALTER TABLE marketplace_extensions
    ADD CONSTRAINT marketplace_extensions_rate_limit_rpm_chk
        CHECK (rate_limit_rpm BETWEEN 1 AND 10000);
