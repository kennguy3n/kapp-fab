-- Session 17 — LMS Deep Enhancement.
--
-- Adds the typed tables backing the new LMS surfaces: Learning Paths,
-- xAPI (Tin Can) statements, Gamification (badges + awards), and
-- per-course Discussion Forums. SCORM runtime state reuses the
-- existing lesson_progress projection plus the lms.progress KRecord
-- (no new table); the lesson `content_type` enum is widened via the
-- KType schema version bump in internal/lms/lms.go, not here.
--
-- Every table follows the platform-wide conventions used by budgets
-- (migrations/000062) / lesson_progress (migrations/000007):
--
--   * Composite PK (tenant_id, id) so the RLS predicate falls on the
--     leading tenant_id index column and partition-pruning works once
--     the platform pivots to LIST-partition by tenant_id.
--   * RLS enabled with the `tenant_isolation` policy reading the
--     `app.tenant_id` GUC — identical to every other tenant-scoped
--     table.
--   * Composite foreign keys carry tenant_id so a child row can never
--     reference a parent in another tenant, and ON DELETE CASCADE keeps
--     the graph consistent when a parent is removed.
--   * GRANT SELECT/INSERT/UPDATE/DELETE to kapp_app; the data plane
--     never touches these as kapp_admin.

-- ===========================================================================
-- SCORM runtime state (extends the existing lesson_progress projection).
-- ===========================================================================
--
-- SCORM/xAPI runtime carries two fields the original lesson_progress
-- (migrations/000007) has no home for: accumulated session time and an
-- opaque resume blob (cmi.suspend_data). Rather than a parallel table
-- we widen lesson_progress so SCORM state lives next to the status/score
-- it already tracks and the enrollment_progress rollup keeps working
-- unchanged. Both columns are nullable/defaulted so existing rows and
-- the non-SCORM write path are untouched.
ALTER TABLE lesson_progress
    ADD COLUMN IF NOT EXISTS time_spent_seconds BIGINT NOT NULL DEFAULT 0
        CHECK (time_spent_seconds >= 0);
ALTER TABLE lesson_progress
    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

-- ===========================================================================
-- Learning Paths.
-- ===========================================================================

-- A learning_path is an ordered curriculum of courses targeted at one
-- or more roles. `target_roles` drives event-driven auto-enrollment:
-- when a user is assigned a role in `target_roles`, the worker enrolls
-- them into every published path that targets that role.
CREATE TABLE IF NOT EXISTS learning_paths (
    tenant_id                UUID NOT NULL,
    id                       UUID NOT NULL,
    title                    TEXT NOT NULL,
    description              TEXT NOT NULL DEFAULT '',
    -- draft → published → archived. Only `published` paths are
    -- auto-enrollment targets and learner-visible; `archived` paths
    -- are retained for historical enrollments but never enroll new
    -- learners.
    status                   TEXT NOT NULL DEFAULT 'draft'
                             CHECK (status IN ('draft', 'published', 'archived')),
    -- Roles that trigger auto-enrollment. Empty = no auto-enrollment
    -- (manual enroll only). Stored as a text[] so the worker can do a
    -- containment match (target_roles && assigned_roles) cheaply.
    target_roles             TEXT[] NOT NULL DEFAULT '{}',
    estimated_duration_hours INT NOT NULL DEFAULT 0
                             CHECK (estimated_duration_hours >= 0),
    difficulty               TEXT NOT NULL DEFAULT 'beginner'
                             CHECK (difficulty IN ('beginner', 'intermediate', 'advanced')),
    created_by               UUID,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- Lookup published paths for a tenant (learner catalog + auto-enroll
-- candidate scan).
CREATE INDEX IF NOT EXISTS learning_paths_tenant_status_idx
    ON learning_paths (tenant_id, status);

-- GIN index so the auto-enroller's `target_roles && $assigned` overlap
-- query is index-assisted instead of a full per-tenant scan.
CREATE INDEX IF NOT EXISTS learning_paths_target_roles_gin
    ON learning_paths USING GIN (target_roles);

ALTER TABLE learning_paths ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON learning_paths;
CREATE POLICY tenant_isolation ON learning_paths
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON learning_paths TO kapp_app;

-- A learning_path_course pins one course into a path at a sequence
-- position. `is_mandatory` controls completion gating: a path is
-- complete when all mandatory courses are complete; non-mandatory
-- courses are tracked but never block. `prerequisite_course_ids` lists
-- courses (by KRecord id) that must complete before this one unlocks.
CREATE TABLE IF NOT EXISTS learning_path_courses (
    tenant_id               UUID NOT NULL,
    id                      UUID NOT NULL,
    learning_path_id        UUID NOT NULL,
    -- course_id references an lms.course KRecord id. Courses remain
    -- KRecords (internal/lms/lms.go), so this is a soft reference, not
    -- a FK into krecords.
    course_id               UUID NOT NULL,
    sequence_order          INT NOT NULL DEFAULT 0,
    is_mandatory            BOOLEAN NOT NULL DEFAULT true,
    prerequisite_course_ids UUID[] NOT NULL DEFAULT '{}',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    -- A course appears at most once per path.
    UNIQUE (tenant_id, learning_path_id, course_id),
    FOREIGN KEY (tenant_id, learning_path_id)
        REFERENCES learning_paths (tenant_id, id) ON DELETE CASCADE
);

-- Ordered fetch of a path's courses (the curriculum view + completion
-- rollup both walk this in sequence_order).
CREATE INDEX IF NOT EXISTS learning_path_courses_path_seq_idx
    ON learning_path_courses (tenant_id, learning_path_id, sequence_order);

ALTER TABLE learning_path_courses ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON learning_path_courses;
CREATE POLICY tenant_isolation ON learning_path_courses
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON learning_path_courses TO kapp_app;

-- One row per (path, user). The UNIQUE constraint makes enrollment
-- idempotent: a repeated enroll (manual or auto) is a no-op via
-- ON CONFLICT DO NOTHING rather than a duplicate row.
CREATE TABLE IF NOT EXISTS learning_path_enrollments (
    tenant_id        UUID NOT NULL,
    id               UUID NOT NULL,
    learning_path_id UUID NOT NULL,
    user_id          UUID NOT NULL,
    status           TEXT NOT NULL DEFAULT 'enrolled'
                     CHECK (status IN ('enrolled', 'in_progress', 'completed')),
    -- 'manual' (explicit enroll) or 'auto' (role-assignment driven).
    -- Retained so reporting can distinguish self/admin enrollment from
    -- policy-driven enrollment.
    source           TEXT NOT NULL DEFAULT 'manual'
                     CHECK (source IN ('manual', 'auto')),
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, learning_path_id, user_id),
    FOREIGN KEY (tenant_id, learning_path_id)
        REFERENCES learning_paths (tenant_id, id) ON DELETE CASCADE
);

-- "What paths is this user on?" — the learner dashboard + auto-enroll
-- dedupe both query by user.
CREATE INDEX IF NOT EXISTS learning_path_enrollments_user_idx
    ON learning_path_enrollments (tenant_id, user_id, status);

ALTER TABLE learning_path_enrollments ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON learning_path_enrollments;
CREATE POLICY tenant_isolation ON learning_path_enrollments
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON learning_path_enrollments TO kapp_app;

-- ===========================================================================
-- xAPI (Tin Can) statements.
-- ===========================================================================

-- Append-only store of validated xAPI statements ingested via
-- POST /api/v1/lms/xapi/statements. The full statement is retained in
-- `raw` (the xAPI spec requires verbatim retention for the statement
-- query API); the projected columns drive actor resolution and the
-- verb→progress mapping without re-parsing JSON.
CREATE TABLE IF NOT EXISTS lms_xapi_statements (
    tenant_id      UUID NOT NULL,
    -- Statement id. xAPI lets the Learning Record Provider supply the
    -- id; we honor it when present (so PUT/POST of the same id is
    -- idempotent per the spec) and generate one otherwise.
    id             UUID NOT NULL,
    -- Resolved Kapp user for the statement actor, NULL when the actor
    -- (email / account) maps to no known user.
    actor_user_id  UUID,
    -- Raw actor identifier used for resolution (mbox sans "mailto:",
    -- or account homePage|name) — kept for audit + re-resolution.
    actor_ident    TEXT NOT NULL DEFAULT '',
    verb_id        TEXT NOT NULL,
    -- Canonical short verb (completed/passed/failed/attempted/
    -- experienced/…) extracted from verb_id's IRI tail.
    verb           TEXT NOT NULL DEFAULT '',
    object_id      TEXT NOT NULL DEFAULT '',
    -- Soft references to the lesson / enrollment the statement mapped
    -- to (when the object IRI carried a Kapp lesson id). NULL when the
    -- statement is informational only and produced no progress write.
    lesson_id      UUID,
    enrollment_id  UUID,
    raw            JSONB NOT NULL,
    stored_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- Statement query API filters: by actor and by recency.
CREATE INDEX IF NOT EXISTS lms_xapi_statements_actor_idx
    ON lms_xapi_statements (tenant_id, actor_user_id, stored_at DESC);
CREATE INDEX IF NOT EXISTS lms_xapi_statements_stored_idx
    ON lms_xapi_statements (tenant_id, stored_at DESC);

ALTER TABLE lms_xapi_statements ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON lms_xapi_statements;
CREATE POLICY tenant_isolation ON lms_xapi_statements
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON lms_xapi_statements TO kapp_app;

-- ===========================================================================
-- Gamification — badges + awards.
-- ===========================================================================

-- A badge is an award definition. `criteria_type` selects the rule the
-- award engine evaluates; `criteria_value` is the rule's parameter
-- (e.g. minimum quiz score, streak length). Kept as JSONB so new
-- criteria shapes don't require a schema change.
CREATE TABLE IF NOT EXISTS lms_badges (
    tenant_id      UUID NOT NULL,
    id             UUID NOT NULL,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    icon           TEXT NOT NULL DEFAULT '',
    criteria_type  TEXT NOT NULL
                   CHECK (criteria_type IN ('course_complete', 'path_complete', 'quiz_score', 'streak')),
    criteria_value JSONB NOT NULL DEFAULT '{}'::jsonb,
    active         BOOLEAN NOT NULL DEFAULT true,
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    -- Badge names are unique per tenant so the award engine can refer
    -- to a badge by name idempotently.
    UNIQUE (tenant_id, name)
);

-- The award engine scans active badges by criteria_type on each
-- milestone event.
CREATE INDEX IF NOT EXISTS lms_badges_tenant_criteria_idx
    ON lms_badges (tenant_id, criteria_type, active);

ALTER TABLE lms_badges ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON lms_badges;
CREATE POLICY tenant_isolation ON lms_badges
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON lms_badges TO kapp_app;

-- One row per (user, badge). The UNIQUE constraint enforces
-- at-most-once awarding: the engine inserts with ON CONFLICT DO NOTHING
-- so re-evaluating a milestone never double-awards.
CREATE TABLE IF NOT EXISTS lms_user_badges (
    tenant_id  UUID NOT NULL,
    id         UUID NOT NULL,
    user_id    UUID NOT NULL,
    badge_id   UUID NOT NULL,
    earned_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The milestone payload that earned the badge (course_id, score,
    -- streak length, …) for audit + UI tooltips.
    context    JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, user_id, badge_id),
    FOREIGN KEY (tenant_id, badge_id)
        REFERENCES lms_badges (tenant_id, id) ON DELETE CASCADE
);

-- "What badges does this user hold?" — leaderboard + profile.
CREATE INDEX IF NOT EXISTS lms_user_badges_user_idx
    ON lms_user_badges (tenant_id, user_id, earned_at DESC);

ALTER TABLE lms_user_badges ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON lms_user_badges;
CREATE POLICY tenant_isolation ON lms_user_badges
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON lms_user_badges TO kapp_app;

-- ===========================================================================
-- Discussion forums (per course).
-- ===========================================================================

-- A discussion_thread is a question/topic scoped to a course (and
-- optionally a specific lesson). `reply_count` is denormalized so the
-- thread list renders counts without an aggregate per row; the store
-- keeps it in sync inside the same transaction as the reply insert.
CREATE TABLE IF NOT EXISTS lms_discussion_threads (
    tenant_id   UUID NOT NULL,
    id          UUID NOT NULL,
    -- Soft references to lms.course / lms.lesson KRecords.
    course_id   UUID NOT NULL,
    lesson_id   UUID,
    author_id   UUID NOT NULL,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open', 'resolved', 'closed')),
    pinned      BOOLEAN NOT NULL DEFAULT false,
    reply_count INT NOT NULL DEFAULT 0 CHECK (reply_count >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

-- Thread list for a course, pinned first then most-recently-active.
CREATE INDEX IF NOT EXISTS lms_discussion_threads_course_idx
    ON lms_discussion_threads (tenant_id, course_id, pinned DESC, updated_at DESC);

ALTER TABLE lms_discussion_threads ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON lms_discussion_threads;
CREATE POLICY tenant_isolation ON lms_discussion_threads
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON lms_discussion_threads TO kapp_app;

-- A reply belongs to a thread. `is_answer` marks the accepted answer
-- (a thread can have at most one, enforced in the store, not the
-- schema, so an instructor can move the accepted flag between replies).
CREATE TABLE IF NOT EXISTS lms_discussion_replies (
    tenant_id  UUID NOT NULL,
    id         UUID NOT NULL,
    thread_id  UUID NOT NULL,
    author_id  UUID NOT NULL,
    body       TEXT NOT NULL,
    is_answer  BOOLEAN NOT NULL DEFAULT false,
    -- 'web' (posted in-app) or 'kchat' (synced from a KChat reply) so
    -- the UI can badge externally-sourced replies.
    source     TEXT NOT NULL DEFAULT 'web'
               CHECK (source IN ('web', 'kchat')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, thread_id)
        REFERENCES lms_discussion_threads (tenant_id, id) ON DELETE CASCADE
);

-- Replies for a thread in post order.
CREATE INDEX IF NOT EXISTS lms_discussion_replies_thread_idx
    ON lms_discussion_replies (tenant_id, thread_id, created_at);

ALTER TABLE lms_discussion_replies ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON lms_discussion_replies;
CREATE POLICY tenant_isolation ON lms_discussion_replies
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON lms_discussion_replies TO kapp_app;
