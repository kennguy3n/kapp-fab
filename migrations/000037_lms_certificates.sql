-- Phase K — LMS certificate indexes.
--
-- lms.certificate KRecords are stored in the generic `krecords`
-- table so the bulk of the migration is just an index that makes
-- the lookup IssueCertificate runs (does this enrollment already
-- have a certificate?) cheap on every issuance attempt.
--
-- Idempotency: the partial unique index makes a duplicate insert
-- collide at 23505. The Go layer translates that into ErrCertificateAlreadyIssued
-- so the auto-issuer worker is safe to retry on transient failures.

CREATE UNIQUE INDEX IF NOT EXISTS lms_certificate_enrollment_uniq
    ON krecords (tenant_id, (data->>'enrollment_id'))
    WHERE ktype = 'lms.certificate' AND status = 'active';

CREATE INDEX IF NOT EXISTS lms_certificate_learner_idx
    ON krecords (tenant_id, (data->>'learner_id'))
    WHERE ktype = 'lms.certificate' AND status = 'active';
