-- Rollback for 000083_lms_deep.sql. Drops in reverse dependency order
-- so child tables (with composite FKs into their parents) go first.
-- CASCADE on the FKs would also handle this, but explicit ordering
-- keeps the intent obvious and avoids surprise drops.

DROP TABLE IF EXISTS lms_discussion_replies;
DROP TABLE IF EXISTS lms_discussion_threads;

DROP TABLE IF EXISTS lms_user_badges;
DROP TABLE IF EXISTS lms_badges;

DROP TABLE IF EXISTS lms_xapi_statements;

DROP TABLE IF EXISTS learning_path_enrollments;
DROP TABLE IF EXISTS learning_path_courses;
DROP TABLE IF EXISTS learning_paths;

-- SCORM runtime columns added onto lesson_progress.
ALTER TABLE lesson_progress DROP COLUMN IF EXISTS metadata;
ALTER TABLE lesson_progress DROP COLUMN IF EXISTS time_spent_seconds;
