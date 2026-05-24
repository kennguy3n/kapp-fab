-- Phase L (PR-7) — IMAP poller checkpoint state.
--
-- One row per (tenant, mailbox_id) recording the highest UID
-- already processed. The Poller fetches strictly above this on
-- every cycle so re-runs after a worker restart don't re-deliver
-- already-processed messages.
--
-- UIDVALIDITY semantics (RFC 9051 §2.3.1.1): if the IMAP server
-- changes UIDVALIDITY on the mailbox (mailbox renamed, recreated,
-- restored from backup), every UID is potentially reassigned and
-- our checkpoint is meaningless. The Poller compares the stored
-- uid_validity against the SELECT response on each run; mismatch
-- forces a full re-scan (with Message-ID dedup catching the
-- duplicate inserts at the email_messages PRIMARY KEY layer).

CREATE TABLE IF NOT EXISTS helpdesk_imap_state (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    mailbox_id   UUID NOT NULL,
    -- The IMAP UIDVALIDITY for the SELECTed folder. When this
    -- changes server-side, last_uid is invalidated and we re-scan.
    uid_validity BIGINT NOT NULL DEFAULT 0,
    -- The highest UID we've successfully processed. Fetch range
    -- is strictly (last_uid, *].
    last_uid     BIGINT NOT NULL DEFAULT 0,
    -- last_polled_at lets the dashboard show "30s ago" without an
    -- IMAP round-trip.
    last_polled_at TIMESTAMPTZ,
    -- consecutive_errors tracks the run of failed polls. Exposed
    -- on the dashboard + used by the Manager to apply exponential
    -- backoff before declaring the mailbox unhealthy.
    consecutive_errors INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT,
    PRIMARY KEY (tenant_id, mailbox_id)
);

CREATE INDEX IF NOT EXISTS helpdesk_imap_state_polled_idx
    ON helpdesk_imap_state (tenant_id, last_polled_at);

ALTER TABLE helpdesk_imap_state ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS helpdesk_imap_state_isolation ON helpdesk_imap_state;
CREATE POLICY helpdesk_imap_state_isolation ON helpdesk_imap_state
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON helpdesk_imap_state TO kapp_app;

COMMENT ON TABLE helpdesk_imap_state IS
    'IMAP poller checkpoint. One row per (tenant, mailbox). The Poller fetches strictly above last_uid; UIDVALIDITY mismatch forces a full re-scan with Message-ID dedup catching the duplicate inserts at the email_messages PRIMARY KEY.';
COMMENT ON COLUMN helpdesk_imap_state.uid_validity IS
    'IMAP UIDVALIDITY for the SELECTed folder. Set on first successful poll. Mismatch on subsequent polls invalidates last_uid.';
COMMENT ON COLUMN helpdesk_imap_state.last_uid IS
    'Highest UID processed in the current uid_validity epoch. Reset to 0 when uid_validity changes.';
