-- Session 16 — Recruitment module.
--
-- Four tables form the recruitment surface, mirroring the manufacturing
-- module's typed-table-plus-Go-state-machine shape (migrations/000063):
--
--   * job_openings    — A requisition / open position. status walks
--                       draft → open → on_hold → closed → filled. The
--                       Go layer (internal/hr/recruitment_store.go)
--                       owns the legal transitions; positions_filled is
--                       bumped when a linked application reaches 'hired'
--                       and is capped at max_positions there.
--
--   * job_applications— A candidate applying to a job_opening. status is
--                       the load-bearing field
--                       (applied → screening → shortlisted → interview →
--                       offered → hired, with rejected / withdrawn
--                       terminal states). AdvanceApplication validates
--                       the move, audits it, and on 'hired' auto-creates
--                       a draft hr.employee KRecord whose id is recorded
--                       in hired_employee_id so the hire is idempotent on
--                       retry.
--
--   * interviews      — One interview round against an application.
--                       status walks scheduled → completed, with
--                       cancelled / no_show terminal states.
--
--   * offer_letters   — An offer extended to an application's candidate.
--                       status walks draft → sent → accepted, with
--                       rejected / expired / withdrawn terminal states.
--                       The draft→sent move requires hiring-manager
--                       approval (workflow.approvals); approval_id links
--                       the gating approval and the applicant email is
--                       only dispatched once it is granted.
--
-- All four tables follow the canonical tenant-scoped pattern: composite
-- (tenant_id, id) primary key, ENABLE ROW LEVEL SECURITY, a
-- tenant_isolation policy keyed off app.tenant_id, and GRANT to
-- kapp_app. Employee / interviewer / referrer references point at
-- hr.employee KRecords (krecords table) and resume_file_id at a file
-- record, so those columns are bare UUIDs with no SQL foreign key — the
-- same way the rest of the HR module links to employees. Intra-module
-- links (application → opening, interview → application, offer →
-- application) use composite foreign keys so cross-tenant linkage is
-- impossible.

-- ---------------------------------------------------------------------------
-- Job openings
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS job_openings (
    tenant_id         UUID    NOT NULL REFERENCES tenants(id),
    id                UUID    NOT NULL,
    title             TEXT    NOT NULL,
    department        TEXT,
    description       TEXT,
    requirements      TEXT,
    employment_type   TEXT    NOT NULL DEFAULT 'full_time'
                      CHECK (employment_type IN ('full_time', 'part_time', 'contract', 'intern')),
    location          TEXT,
    salary_range_min  NUMERIC(20, 2) CHECK (salary_range_min IS NULL OR salary_range_min >= 0),
    salary_range_max  NUMERIC(20, 2) CHECK (salary_range_max IS NULL OR salary_range_max >= 0),
    currency          TEXT    NOT NULL DEFAULT 'USD'
                      CHECK (currency ~ '^[A-Z]{3}$'),
    status            TEXT    NOT NULL DEFAULT 'draft'
                      CHECK (status IN ('draft', 'open', 'on_hold', 'closed', 'filled')),
    hiring_manager_id UUID,
    max_positions     INTEGER NOT NULL DEFAULT 1 CHECK (max_positions >= 1),
    positions_filled  INTEGER NOT NULL DEFAULT 0 CHECK (positions_filled >= 0),
    published_at      TIMESTAMPTZ,
    closes_at         TIMESTAMPTZ,
    created_by        UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    CHECK (salary_range_max IS NULL OR salary_range_min IS NULL OR salary_range_max >= salary_range_min)
);

CREATE INDEX IF NOT EXISTS job_openings_status_idx
    ON job_openings (tenant_id, status);
CREATE INDEX IF NOT EXISTS job_openings_department_idx
    ON job_openings (tenant_id, department);

ALTER TABLE job_openings ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON job_openings;
CREATE POLICY tenant_isolation ON job_openings
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON job_openings TO kapp_app;

-- ---------------------------------------------------------------------------
-- Job applications
-- ---------------------------------------------------------------------------
-- status is the load-bearing field; legal transitions are enforced in
-- the Go state machine (AdvanceApplication), not in SQL, so the error
-- surface is a typed error instead of a CHECK violation. hired_employee_id
-- records the draft hr.employee KRecord auto-created when the candidate is
-- hired, making the hire idempotent on retry.
CREATE TABLE IF NOT EXISTS job_applications (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   UUID NOT NULL,
    job_opening_id       UUID NOT NULL,
    applicant_name       TEXT NOT NULL,
    applicant_email      TEXT,
    phone                TEXT,
    resume_file_id       UUID,
    cover_letter         TEXT,
    source               TEXT NOT NULL DEFAULT 'website'
                         CHECK (source IN ('website', 'referral', 'linkedin', 'agency', 'other')),
    referrer_employee_id UUID,
    status               TEXT NOT NULL DEFAULT 'applied'
                         CHECK (status IN ('applied', 'screening', 'shortlisted', 'interview', 'offered', 'hired', 'rejected', 'withdrawn')),
    rating               INTEGER CHECK (rating IS NULL OR (rating >= 1 AND rating <= 5)),
    notes                TEXT,
    hired_employee_id    UUID,
    applied_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by           UUID,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, job_opening_id) REFERENCES job_openings (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS job_applications_status_idx
    ON job_applications (tenant_id, status);
CREATE INDEX IF NOT EXISTS job_applications_opening_idx
    ON job_applications (tenant_id, job_opening_id);

ALTER TABLE job_applications ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON job_applications;
CREATE POLICY tenant_isolation ON job_applications
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON job_applications TO kapp_app;

-- ---------------------------------------------------------------------------
-- Interviews
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS interviews (
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    id               UUID NOT NULL,
    application_id   UUID NOT NULL,
    interviewer_id   UUID,
    interview_type   TEXT NOT NULL DEFAULT 'video'
                     CHECK (interview_type IN ('phone', 'video', 'in_person', 'panel', 'technical', 'cultural')),
    scheduled_at     TIMESTAMPTZ,
    duration_minutes INTEGER NOT NULL DEFAULT 60 CHECK (duration_minutes >= 0),
    location         TEXT,
    meeting_link     TEXT,
    status           TEXT NOT NULL DEFAULT 'scheduled'
                     CHECK (status IN ('scheduled', 'completed', 'cancelled', 'no_show')),
    rating           INTEGER CHECK (rating IS NULL OR (rating >= 1 AND rating <= 5)),
    feedback         TEXT,
    recommendation   TEXT CHECK (recommendation IS NULL OR recommendation IN ('strong_yes', 'yes', 'neutral', 'no', 'strong_no')),
    created_by       UUID,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, application_id) REFERENCES job_applications (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS interviews_status_idx
    ON interviews (tenant_id, status);
CREATE INDEX IF NOT EXISTS interviews_application_idx
    ON interviews (tenant_id, application_id);

ALTER TABLE interviews ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON interviews;
CREATE POLICY tenant_isolation ON interviews
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON interviews TO kapp_app;

-- ---------------------------------------------------------------------------
-- Offer letters
-- ---------------------------------------------------------------------------
-- The draft→sent transition is gated by a hiring-manager approval
-- (workflow.approvals). approval_id links that approval; the applicant
-- email is dispatched only once the approval is granted (or immediately
-- when the opening has no hiring manager to approve).
CREATE TABLE IF NOT EXISTS offer_letters (
    tenant_id            UUID NOT NULL REFERENCES tenants(id),
    id                   UUID NOT NULL,
    application_id       UUID NOT NULL,
    employee_template_id UUID,
    designation          TEXT,
    department           TEXT,
    salary               NUMERIC(20, 2) CHECK (salary IS NULL OR salary >= 0),
    currency             TEXT NOT NULL DEFAULT 'USD'
                         CHECK (currency ~ '^[A-Z]{3}$'),
    joining_date         DATE,
    probation_months     INTEGER NOT NULL DEFAULT 0 CHECK (probation_months >= 0),
    benefits             JSONB NOT NULL DEFAULT '{}'::jsonb,
    status               TEXT NOT NULL DEFAULT 'draft'
                         CHECK (status IN ('draft', 'sent', 'accepted', 'rejected', 'expired', 'withdrawn')),
    approval_id          UUID,
    sent_at              TIMESTAMPTZ,
    responded_at         TIMESTAMPTZ,
    valid_until          TIMESTAMPTZ,
    created_by           UUID,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, application_id) REFERENCES job_applications (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS offer_letters_status_idx
    ON offer_letters (tenant_id, status);
CREATE INDEX IF NOT EXISTS offer_letters_application_idx
    ON offer_letters (tenant_id, application_id);

ALTER TABLE offer_letters ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON offer_letters;
CREATE POLICY tenant_isolation ON offer_letters
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON offer_letters TO kapp_app;
