-- Phase L (PR-7) — Helpdesk email message persistence + threading.
--
-- The Phase J inbound-email handler (000031) creates one ticket per
-- inbound message. PR-7 extends this so subsequent emails on the
-- same thread (replies, follow-ups, customer comments) attach to
-- the original ticket instead of opening duplicates.
--
-- Threading is driven by RFC-822 In-Reply-To and References
-- headers. The handler looks up the parent message in email_messages
-- (this migration's table) and, if found, threads onto the parent's
-- ticket. The lookup is bounded by lookback window — by default
-- ThreadingResolver only considers messages received in the last
-- 30 days, so a hijacked-thread attempt (stale message-id reused on
-- an unrelated ticket months later) is rejected and a new ticket
-- opens instead.
--
-- email_messages also seeds the outbound reply path (PR-7 Surface C):
-- each outbound reply persists a row with direction='outbound', so a
-- customer's reply lands on the same ticket as the agent's prior
-- response via the customer's In-Reply-To pointing at the outbound
-- Message-ID.

CREATE TABLE IF NOT EXISTS email_messages (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    message_id   TEXT NOT NULL,
    ticket_id    UUID NOT NULL,
    direction    TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    in_reply_to  TEXT,
    -- References is the full RFC-822 thread chain stored as a JSON
    -- array of strings so the threading resolver can walk the chain
    -- without parsing per-row whitespace-delimited header values.
    -- Stored as JSONB rather than TEXT[] for parity with the rest
    -- of the platform's JSON-first persistence style.
    "references" JSONB NOT NULL DEFAULT '[]'::jsonb,
    subject      TEXT,
    from_addr    TEXT,
    to_addr      TEXT,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, message_id)
);

-- Lookup by (tenant, in_reply_to) — the threading resolver's hot
-- path. When an inbound email's In-Reply-To references a known
-- Message-ID, this index satisfies the parent-lookup in O(log n)
-- without scanning the whole tenant's message history.
CREATE INDEX IF NOT EXISTS email_messages_tenant_in_reply_to_idx
    ON email_messages (tenant_id, in_reply_to)
    WHERE in_reply_to IS NOT NULL;

-- Lookup by (tenant, ticket_id, received_at DESC) — the agent UI's
-- "show all messages on this ticket, newest first" rendering. The
-- DESC order matches the dominant access pattern and keeps the
-- index leaf reads sequential.
CREATE INDEX IF NOT EXISTS email_messages_tenant_ticket_received_idx
    ON email_messages (tenant_id, ticket_id, received_at DESC);

-- Lookback-window prune support. The threading resolver only
-- considers messages within a recency window (default 30d), so a
-- partial index restricted to `received_at > now() - interval`
-- would help — but partial-index predicates can't reference
-- now() (it's not IMMUTABLE), and a static cutoff would go stale.
-- The (tenant_id, received_at) index supports prune-job sweeps
-- (DELETE WHERE received_at < $cutoff) without locking the
-- threading hot path.
CREATE INDEX IF NOT EXISTS email_messages_tenant_received_idx
    ON email_messages (tenant_id, received_at);

ALTER TABLE email_messages ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS email_messages_isolation ON email_messages;
CREATE POLICY email_messages_isolation ON email_messages
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON email_messages TO kapp_app;

COMMENT ON TABLE email_messages IS
    'Per-message persistence for helpdesk email threading. One row per inbound or outbound message; direction=inbound rows seed parent-lookup, direction=outbound rows seed agent-reply threading.';
COMMENT ON COLUMN email_messages.message_id IS
    'RFC-822 Message-ID header value, angle-brackets stripped. Idempotency key alongside tenant_id; a retry by the upstream relay yields the same row via ON CONFLICT (tenant_id, message_id) DO NOTHING.';
COMMENT ON COLUMN email_messages."references" IS
    'Full RFC-822 References header as a JSON array of message-ids (oldest first). The threading resolver walks this chain when In-Reply-To miss-matches but a deeper ancestor matches.';
