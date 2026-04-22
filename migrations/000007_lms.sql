-- Phase E — LMS lesson-progress table.
--
-- Courses, modules, lessons, enrollments, quizzes, and assignments are
-- all stored as KRecords (schemas in internal/lms/lms.go). The only
-- projection that benefits from a dedicated table is per-user
-- per-lesson progress: it is queried heavily (progress pane, course
-- dashboards) and is naturally keyed by (enrollment_id, lesson_id),
-- which is expensive to express over the JSON KRecord store.
--
-- `lesson_progress` carries a small fixed set of columns; the rest of
-- the lesson/quiz metadata stays in the KRecord.

CREATE TABLE IF NOT EXISTS lesson_progress (
    tenant_id       UUID NOT NULL,
    enrollment_id   UUID NOT NULL,
    lesson_id       UUID NOT NULL,
    status          TEXT NOT NULL DEFAULT 'not_started'
        CHECK (status IN ('not_started','in_progress','completed')),
    score           NUMERIC(6,2),
    attempts        INT NOT NULL DEFAULT 0,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, enrollment_id, lesson_id)
);

CREATE INDEX IF NOT EXISTS lesson_progress_tenant_enrollment_idx
    ON lesson_progress (tenant_id, enrollment_id, status);

ALTER TABLE lesson_progress ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON lesson_progress;
CREATE POLICY tenant_isolation ON lesson_progress
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON lesson_progress TO kapp_app;

-- Course-level completion rollup: count of lessons completed vs total
-- per enrollment. Consumers (progress pane, /learn command) use this
-- to render simple progress bars without loading every row.
CREATE OR REPLACE VIEW enrollment_progress AS
    SELECT tenant_id,
           enrollment_id,
           COUNT(*) FILTER (WHERE status = 'completed') AS completed_lessons,
           COUNT(*) AS total_lessons
      FROM lesson_progress
     GROUP BY tenant_id, enrollment_id;

GRANT SELECT ON enrollment_progress TO kapp_app;
