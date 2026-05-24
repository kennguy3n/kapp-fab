-- Phase L (PR-7) — Helpdesk email attachment linkage.
--
-- The actual attachment bytes live in the platform's
-- content-addressable file store (`files` table + S3 / MinIO backend
-- via internal/files). This table is the linkage from an inbound
-- (or outbound) email message to one or more files, plus the
-- per-attachment metadata that doesn't fit in the shared `files`
-- shape — original filename (vs the SHA-256 storage key), the virus
-- scan verdict, and the verdict timestamp.
--
-- Storage strategy:
--   email_messages 1 ← N email_attachments → 1 files
--                                            (dedup'd globally by hash)
--
-- A 5MB customer-uploaded screenshot sent to ten tickets stores its
-- bytes ONCE in S3 and creates ten email_attachments rows + ten
-- files metadata rows. The dedup happens automatically via the
-- content_hash → storage_key map in files.

CREATE TABLE IF NOT EXISTS email_attachments (
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    -- (tenant_id, message_id) references email_messages so a delete
    -- on the message cascades. The FK target on email_messages is
    -- (tenant_id, message_id) per migration 000055.
    message_id   TEXT NOT NULL,
    file_id      UUID NOT NULL,
    filename     TEXT NOT NULL,
    content_type TEXT,
    size_bytes   BIGINT,
    -- Virus-scan verdict at attach time. NULL until scanned;
    -- 'clean', 'infected', or 'skipped' once a scanner has run
    -- (skipped = no scanner configured).
    scan_verdict TEXT CHECK (scan_verdict IN ('clean', 'infected', 'skipped')),
    scan_detail  TEXT,
    scanned_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, message_id, file_id),
    FOREIGN KEY (tenant_id, message_id) REFERENCES email_messages (tenant_id, message_id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, file_id)    REFERENCES files (tenant_id, id)
);

-- "All attachments for this message in insert order" — the agent
-- UI's primary view. created_at ASC mirrors the order the parser
-- enumerated parts.
CREATE INDEX IF NOT EXISTS email_attachments_tenant_message_created_idx
    ON email_attachments (tenant_id, message_id, created_at);

-- "Find every message that links to this file" — used by the file-
-- deletion path to refuse deleting a file that's still referenced
-- (RESTRICT-on-delete semantics handled at the application layer
-- since CHECK + trigger would be overkill).
CREATE INDEX IF NOT EXISTS email_attachments_tenant_file_idx
    ON email_attachments (tenant_id, file_id);

ALTER TABLE email_attachments ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS email_attachments_isolation ON email_attachments;
CREATE POLICY email_attachments_isolation ON email_attachments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON email_attachments TO kapp_app;

COMMENT ON TABLE email_attachments IS
    'Per-(message, file) linkage with virus scan verdict. Attachment bytes live in the platform-wide files table; this row records the per-message metadata that doesn''t fit there (original filename, scan verdict, scan timestamp).';
COMMENT ON COLUMN email_attachments.scan_verdict IS
    'clean / infected / skipped. NULL during the brief window between row insert and scan completion. The inbound path scans synchronously so a row with NULL scan_verdict that survives more than one second indicates a crash mid-scan and is candidate for replay.';
