package lms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// CertificateNumberPrefix is prepended to every generated certificate
// number so a quick visual scan distinguishes certificate IDs from
// other identifiers in the system. Tenant operators can override the
// prefix by passing CertificateOptions.NumberPrefix.
const CertificateNumberPrefix = "LMS-CERT-"

// DefaultCertificateTemplateID is the template id IssueCertificate
// stamps on the certificate KRecord when the caller does not pass
// one. Resolved by the print service; absence falls back to
// internal/print/templates/certificate.html.
const DefaultCertificateTemplateID = "lms.certificate.default"

// Sentinel errors the API / agent layer translates into 4xx.
var (
	ErrEnrollmentNotFound        = errors.New("lms: enrollment not found")
	ErrEnrollmentNotComplete     = errors.New("lms: enrollment is not in completed status")
	ErrCertificateAlreadyIssued  = errors.New("lms: certificate already issued for this enrollment")
	ErrCertificateInvalidPayload = errors.New("lms: certificate payload invalid")
)

// CertificateOptions allows overriding generated values during issue.
// Both fields are optional; zero values fall back to defaults.
type CertificateOptions struct {
	NumberPrefix string
	TemplateID   string
	IssuedAt     time.Time
}

// CertificateIssuer issues lms.certificate KRecords against completed
// lms.enrollment KRecords. Pulled out into its own type so the
// auto-issue worker, the agent tool, and the KChat command share one
// implementation — they only differ in how they discover which
// enrollments to issue against.
type CertificateIssuer struct {
	records *record.PGStore
	pool    *pgxpool.Pool
}

// NewCertificateIssuer wires an issuer from the shared record store
// and pool. The pool is used only for the duplicate-lookup fallback
// (see findExisting); writes go through the record store.
func NewCertificateIssuer(records *record.PGStore, pool *pgxpool.Pool) *CertificateIssuer {
	return &CertificateIssuer{records: records, pool: pool}
}

// IssueCertificate generates and persists an lms.certificate KRecord
// for a completed enrollment. Idempotent: if a certificate already
// exists for the enrollment, the existing row is returned with
// ErrCertificateAlreadyIssued wrapped — callers can detect the
// sentinel and treat it as success in the auto-issue path.
//
// Validation:
//   - the enrollment must exist
//   - the enrollment.status must be "completed"
//
// Number generation: deterministic on the enrollment id so re-runs
// generate the same certificate_number (paired with the partial
// unique index in 000037_lms_certificates.sql to make duplicates
// hard-fail at the DB layer).
func (c *CertificateIssuer) IssueCertificate(ctx context.Context, tenantID, enrollmentID, actorID uuid.UUID, opt CertificateOptions) (*record.KRecord, error) {
	if c.records == nil {
		return nil, errors.New("lms: certificate issuer requires a record store")
	}
	if tenantID == uuid.Nil || enrollmentID == uuid.Nil {
		return nil, errors.New("lms: tenant_id and enrollment_id required")
	}
	enrollment, err := c.records.Get(ctx, tenantID, enrollmentID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnrollmentNotFound, err)
	}
	if enrollment.KType != KTypeEnrollment {
		return nil, fmt.Errorf("%w: record %s is not an enrollment", ErrCertificateInvalidPayload, enrollmentID)
	}
	var enrollmentBody map[string]any
	if err := json.Unmarshal(enrollment.Data, &enrollmentBody); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCertificateInvalidPayload, err)
	}
	status, _ := enrollmentBody["status"].(string)
	if status != "completed" {
		return nil, ErrEnrollmentNotComplete
	}
	courseID, _ := enrollmentBody["course_id"].(string)
	learnerID, _ := enrollmentBody["user_id"].(string)
	if courseID == "" || learnerID == "" {
		return nil, fmt.Errorf("%w: enrollment missing course_id or user_id", ErrCertificateInvalidPayload)
	}

	prefix := opt.NumberPrefix
	if prefix == "" {
		prefix = CertificateNumberPrefix
	}
	templateID := opt.TemplateID
	if templateID == "" {
		templateID = DefaultCertificateTemplateID
	}
	issuedAt := opt.IssuedAt
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	}
	certNumber := generateCertificateNumber(prefix, enrollmentID)

	body := map[string]any{
		"enrollment_id":      enrollmentID,
		"course_id":          courseID,
		"learner_id":         learnerID,
		"certificate_number": certNumber,
		"issued_at":          issuedAt.UTC().Format(time.RFC3339Nano),
		"template_id":        templateID,
	}
	dataJSON, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrCertificateInvalidPayload, err)
	}
	cert, err := c.records.Create(ctx, record.KRecord{
		TenantID:     tenantID,
		KType:        KTypeCertificate,
		KTypeVersion: 1,
		Data:         dataJSON,
		Status:       "active",
		CreatedBy:    actorID,
	})
	if err != nil {
		// The partial unique index from 000037 surfaces 23505 when an
		// active certificate already exists for the enrollment.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			existing, getErr := c.findExisting(ctx, tenantID, enrollmentID)
			if getErr != nil {
				return nil, fmt.Errorf("%w (and lookup failed: %v)", ErrCertificateAlreadyIssued, getErr)
			}
			return existing, ErrCertificateAlreadyIssued
		}
		// Wrapped in case the underlying store also returned the
		// duplicate via a wrapped error (defence in depth).
		if errors.Is(err, pgx.ErrTxClosed) {
			return nil, fmt.Errorf("lms: persist certificate: %w", err)
		}
		return nil, fmt.Errorf("lms: persist certificate: %w", err)
	}
	return cert, nil
}

// findExisting locates the in-place certificate when IssueCertificate
// races a concurrent issuer and the partial unique index trips. Walks
// active certificates for the tenant via dbutil.WithTenantTx so RLS
// applies; returns ErrCertificateAlreadyIssued if no row matches
// (which would indicate an inconsistency, not a race).
func (c *CertificateIssuer) findExisting(ctx context.Context, tenantID, enrollmentID uuid.UUID) (*record.KRecord, error) {
	var out record.KRecord
	if c.pool == nil {
		return nil, ErrCertificateAlreadyIssued
	}
	err := dbutil.WithTenantTx(ctx, c.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, ktype, ktype_version, data, status, version,
			        created_by, created_at, updated_by, updated_at, deleted_at
			   FROM krecords
			  WHERE tenant_id = $1
			    AND ktype = $2
			    AND status = 'active'
			    AND data->>'enrollment_id' = $3::text
			  LIMIT 1`,
			tenantID, KTypeCertificate, enrollmentID.String(),
		).Scan(
			&out.ID, &out.TenantID, &out.KType, &out.KTypeVersion,
			&out.Data, &out.Status, &out.Version,
			&out.CreatedBy, &out.CreatedAt,
			&out.UpdatedBy, &out.UpdatedAt, &out.DeletedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCertificateAlreadyIssued
		}
		return nil, err
	}
	return &out, nil
}

// generateCertificateNumber returns a deterministic certificate
// number derived from the enrollment id. Same enrollment → same
// number, so retries do not invent fresh ids.
func generateCertificateNumber(prefix string, enrollmentID uuid.UUID) string {
	idStr := strings.ReplaceAll(strings.ToUpper(enrollmentID.String()), "-", "")
	if len(idStr) > 12 {
		idStr = idStr[:12]
	}
	return prefix + idStr
}
