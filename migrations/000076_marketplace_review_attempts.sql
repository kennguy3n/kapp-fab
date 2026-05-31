-- Phase B7.2 — review worker hardening: retry counter + dead-letter state.
--
-- B7 (#131) shipped the automated review pipeline as a singleton on
-- the elected leader, with sequential per-version drain and infinite
-- implicit retries — a pipeline failure (CDN 5xx, bundle parser
-- exception, etc.) leaves the row in `submitted` and the next tick
-- re-claims it forever. There's no upper bound on attempt count and
-- no terminal "we tried, we gave up" state.
--
-- This migration adds the schema half of B7.2:
--
--   * attempt_count: monotonically incremented every time the
--     pipeline starts a run and fails. Reset to 0 by admin Rescan
--     and on successful state transitions.
--
--   * last_attempt_error: free-form text recording the failure
--     class from the most recent attempt. Surfaced in the admin
--     review queue UI so the operator can decide whether to rescan
--     (e.g. CDN was temporarily down) or investigate (e.g. bundle
--     is corrupt and must be re-uploaded).
--
--   * last_attempt_at: wall-clock at the most recent failure. Lets
--     the operator see whether a dead-lettered version has been
--     stuck for hours vs minutes.
--
--   * Adds `dead_letter` to the status CHECK. The pipeline
--     transitions a row to `dead_letter` once attempt_count reaches
--     the worker-side MaxReviewAttempts (default 5). The DB CHECK
--     accepts the new value but doesn't enforce the attempt-count
--     gate — that policy lives in the worker so it can change
--     without a migration.
--
-- Design choices:
--
--   * Status flip vs separate dead-letter table. A separate
--     marketplace_review_dead_letter would duplicate version_id,
--     attempt_count, and last_attempt_error and require a JOIN on
--     every queue-list query. A status value + the three new
--     columns keeps every review row in one place and lets the
--     existing reviewQueue handler filter by status=dead_letter
--     with a query string.
--
--   * attempt_count starts at 0 and counts FAILED attempts (not
--     started attempts). A successful run never increments it. This
--     matches the operator mental model "how many times has the
--     pipeline tried and failed?" — incrementing on start would
--     surface 1 even for healthy rows that pass on first try.
--
--   * attempt_count is NOT cleared on the normal terminal
--     transition out of submitted (automated_passed, manual_review,
--     rejected). It IS cleared by admin Rescan (which sets status
--     back to submitted from scratch). The rationale: once a row
--     successfully transitioned away from submitted, the
--     attempt_count is forensic history; if rescan brings it back
--     it deserves a fresh 5-attempt budget.
--
--   * last_attempt_error is TEXT (not JSONB). The pipeline writes a
--     short human-readable string (the error message + class). If
--     the operator needs structured detail they look at logs;
--     dumping the full pipeline error tree into the review row
--     would bloat the queue-list response unnecessarily.
--
--   * dead_letter is terminal in the worker's transition graph. The
--     only way out is admin Rescan, which is the same recovery
--     path for an admin-stuck `submitted` row already.
--
-- Idempotent: the ALTER TABLE uses IF NOT EXISTS so re-running this
-- migration is safe (the runtime check enforces transitions; the DB
-- only validates the value belongs to the enum).

ALTER TABLE marketplace_extension_review_state
    ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_attempt_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ;

-- Relax the status CHECK to include dead_letter. ALTER CONSTRAINT
-- isn't supported on CHECK constraints in Postgres, so we DROP+ADD.
-- IF EXISTS keeps the migration idempotent across fresh and
-- previously-migrated databases.
ALTER TABLE marketplace_extension_review_state
    DROP CONSTRAINT IF EXISTS marketplace_extension_review_state_status_valid;
ALTER TABLE marketplace_extension_review_state
    ADD CONSTRAINT marketplace_extension_review_state_status_valid
        CHECK (status IN ('submitted','automated_passed','manual_review','approved','rejected','withdrawn','dead_letter'));

-- Bound the counter so a runaway worker can't insert millions of
-- attempts. Five is the worker's MaxReviewAttempts; the cap of 100
-- in the DB is defense-in-depth in case the constant changes without
-- a migration.
ALTER TABLE marketplace_extension_review_state
    DROP CONSTRAINT IF EXISTS marketplace_extension_review_state_attempt_count_bounded;
ALTER TABLE marketplace_extension_review_state
    ADD CONSTRAINT marketplace_extension_review_state_attempt_count_bounded
        CHECK (attempt_count >= 0 AND attempt_count <= 100);

COMMENT ON COLUMN marketplace_extension_review_state.attempt_count IS
    'Number of consecutive failed pipeline runs since the row was last in a non-submitted state. Incremented by the worker via RecordReviewAttemptFailure when pipeline.Run or pipeline.Persist returns an unrecoverable error (not ErrClaimLost / ErrNotFound). Reset to 0 by ResetReviewStateForRescan. When >= MaxReviewAttempts the worker transitions the row to dead_letter.';
COMMENT ON COLUMN marketplace_extension_review_state.last_attempt_error IS
    'Human-readable error message from the most recent failed pipeline run. Cleared by admin Rescan. Surfaced in the admin review queue UI so the operator can decide whether the failure is transient (rescan) or persistent (investigate the bundle).';
COMMENT ON COLUMN marketplace_extension_review_state.last_attempt_at IS
    'Wall-clock timestamp of the most recent failed pipeline run. NULL until the first failure. Lets the operator distinguish a dead-lettered row that just failed (worth rescanning) from one stuck for hours (worth investigating).';
