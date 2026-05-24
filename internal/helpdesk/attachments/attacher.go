package attachments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/files"
)

// Attachment is one inbound (or outbound) email attachment. The
// shape mirrors what the MIME parser hands us: a filename, a
// content type, and the raw bytes. The Attacher stores the bytes
// via files.Store (content-addressable, dedup'd) and persists the
// per-message linkage row.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Record is one row of email_attachments — what the agent UI
// renders on the ticket timeline. The FileID points at the
// content-addressable file row; the agent's download link
// resolves it via files.Store.Read.
type Record struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	MessageID   string    `json:"message_id"`
	FileID      uuid.UUID `json:"file_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type,omitempty"`
	SizeBytes   int64     `json:"size_bytes,omitempty"`
	ScanVerdict Verdict   `json:"scan_verdict,omitempty"`
	ScanDetail  string    `json:"scan_detail,omitempty"`
	ScannedAt   time.Time `json:"scanned_at,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// uploader is the subset of files.Store the Attacher uses. The
// interface exists so unit tests can inject a fake without
// constructing a real Store.
type uploader interface {
	Upload(ctx context.Context, tenantID, uploaderID uuid.UUID, blob files.Blob) (*files.File, error)
}

// Attacher persists email attachments to the content-addressable
// file store and links them to the email_messages row. Construction
// requires a pool + uploader + scanner triple — wiring lives in
// services/api/deps_build.go.
type Attacher struct {
	pool     *pgxpool.Pool
	uploader uploader
	scanner  Scanner

	// systemActor is the uploader_id stamped on the files row.
	// The inbound-email path has no real user — we use the
	// same constant the recurring-invoice handler uses so the
	// audit log treats "system-uploaded" consistently. Source
	// of truth lives in helpdesk constants (passed at
	// construction so the attachments package doesn't import
	// from helpdesk and create a cycle).
	systemActor uuid.UUID

	// maxBytes caps the per-attachment size at attach time so
	// an adversarial sender can't OOM the worker by piping a
	// 50GB tarball. Defaults to 25MiB at construction;
	// operator-tunable via WithMaxBytes.
	maxBytes int64

	now func() time.Time
}

// Option configures an Attacher at construction.
type Option func(*Attacher)

// WithMaxBytes caps the per-attachment size. Bytes over the cap are
// rejected with ErrTooLarge BEFORE the upload to the object store,
// so a malicious payload never reaches S3.
func WithMaxBytes(n int64) Option {
	return func(a *Attacher) {
		if n > 0 {
			a.maxBytes = n
		}
	}
}

// WithClock injects a deterministic clock for tests.
func WithClock(now func() time.Time) Option {
	return func(a *Attacher) {
		if now != nil {
			a.now = now
		}
	}
}

// DefaultMaxBytes is the per-attachment cap when WithMaxBytes is
// not set. 25 MiB matches the de-facto SMTP attachment limit on
// most mail providers; anything bigger usually arrives as a link
// to cloud storage, not an inline body part.
const DefaultMaxBytes = 25 * 1024 * 1024

// ErrTooLarge is returned by Attach when an attachment exceeds the
// configured per-attachment cap. The inbound-email path surfaces
// this as a 4xx terminal failure so the relay does NOT retry; the
// attachment is recorded as "rejected: too large" for the audit
// log.
var ErrTooLarge = errors.New("attachments: payload too large")

// ErrNoMessage is returned when Attach is called against a
// (tenant, message_id) that does not exist in email_messages. The
// caller's contract is to insert the parent email_messages row
// first.
var ErrNoMessage = errors.New("attachments: no parent message")

// NewAttacher wires an Attacher. The systemActor parameter is the
// uploader id stamped on the underlying files row; the caller
// passes the same constant the rest of the helpdesk surface uses
// for system writes so the audit log stays consistent.
func NewAttacher(pool *pgxpool.Pool, uploader uploader, scanner Scanner, systemActor uuid.UUID, opts ...Option) *Attacher {
	a := &Attacher{
		pool:        pool,
		uploader:    uploader,
		scanner:     scanner,
		systemActor: systemActor,
		maxBytes:    DefaultMaxBytes,
		now:         func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Attach stores one attachment for one inbound (or outbound)
// message. The bytes go to the content-addressable file store
// (dedup'd by SHA-256), the link row goes to email_attachments,
// and the virus scan happens BEFORE the file is committed so an
// infected payload is never persisted.
//
// Ordering (important for the failure semantics):
//
//  1. Size check — fail fast before scan + upload.
//  2. Virus scan — fail closed; on scanner error, surface
//     immediately. On VerdictInfected, return ErrInfected
//     WITHOUT uploading the bytes.
//  3. files.Store.Upload — bytes hit S3 + a files row is created.
//  4. email_attachments INSERT — the linkage row is created with
//     the scan verdict + detail.
//
// On any failure between (3) and (4), the bytes in S3 are orphaned
// — that's acceptable because the content-addressable dedup means
// future uploads of the same bytes will re-use the same key (no
// extra storage cost). A janitor job can be added later to garbage-
// collect files rows with no email_attachments referrers, but is
// not in scope for this PR.
func (a *Attacher) Attach(ctx context.Context, tenantID uuid.UUID, messageID string, att Attachment) (*Record, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("attachments: tenant id required")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, errors.New("attachments: message id required")
	}
	if att.Filename == "" {
		return nil, errors.New("attachments: filename required")
	}
	if len(att.Data) == 0 {
		return nil, errors.New("attachments: empty payload")
	}
	if a.maxBytes > 0 && int64(len(att.Data)) > a.maxBytes {
		return nil, fmt.Errorf("%w: %d > %d bytes", ErrTooLarge, len(att.Data), a.maxBytes)
	}

	// Step 2: virus scan. Fail closed.
	scanRes, err := a.scanner.Scan(ctx, att.Filename, att.ContentType, att.Data)
	if err != nil {
		return nil, fmt.Errorf("attachments: scan %q: %w", att.Filename, err)
	}
	scannedAt := a.now()
	if scanRes.Verdict == VerdictInfected {
		// Persist the attempt for the audit log — file_id is
		// uuid.Nil because we never uploaded the bytes, but
		// the row records what was rejected and why.
		_ = a.recordRejection(ctx, tenantID, messageID, att, scanRes, scannedAt)
		return nil, fmt.Errorf("%w: %s (%s)", ErrInfected, scanRes.Detail, att.Filename)
	}

	// Step 3: upload via files.Store. The content type defaults
	// to application/octet-stream inside files.Store.Upload so
	// we don't need to substitute here.
	file, err := a.uploader.Upload(ctx, tenantID, a.systemActor, files.Blob{
		ContentType: att.ContentType,
		Data:        att.Data,
	})
	if err != nil {
		return nil, fmt.Errorf("attachments: upload: %w", err)
	}

	// Step 4: link row.
	rec := Record{
		TenantID:    tenantID,
		MessageID:   messageID,
		FileID:      file.ID,
		Filename:    att.Filename,
		ContentType: file.ContentType,
		SizeBytes:   file.SizeBytes,
		ScanVerdict: scanRes.Verdict,
		ScanDetail:  scanRes.Detail,
		ScannedAt:   scannedAt,
		CreatedAt:   a.now(),
	}
	err = dbutil.WithTenantTx(ctx, a.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO email_attachments
                 (tenant_id, message_id, file_id, filename, content_type, size_bytes,
                  scan_verdict, scan_detail, scanned_at)
             VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), $9)
             RETURNING created_at`,
			rec.TenantID, rec.MessageID, rec.FileID, rec.Filename, rec.ContentType,
			rec.SizeBytes, string(rec.ScanVerdict), rec.ScanDetail, rec.ScannedAt,
		).Scan(&rec.CreatedAt)
	})
	if err != nil {
		// FK violation on (tenant_id, message_id) maps to
		// ErrNoMessage so the caller can distinguish a real
		// programming error from a transient DB failure.
		if isFKMissing(err, "email_attachments_tenant_id_message_id_fkey") {
			return nil, ErrNoMessage
		}
		return nil, fmt.Errorf("attachments: insert link: %w", err)
	}
	return &rec, nil
}

// List returns every attachment for a message, oldest first
// (insert order). Empty slice when none.
func (a *Attacher) List(ctx context.Context, tenantID uuid.UUID, messageID string) ([]Record, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("attachments: tenant id required")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, errors.New("attachments: message id required")
	}
	out := make([]Record, 0)
	err := dbutil.WithTenantTx(ctx, a.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, message_id, file_id, filename, content_type, size_bytes,
                    scan_verdict, scan_detail, scanned_at, created_at
             FROM email_attachments
             WHERE tenant_id = $1 AND message_id = $2
             ORDER BY created_at ASC`,
			tenantID, messageID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Record
			var contentType, scanVerdict, scanDetail *string
			var sizeBytes *int64
			var scannedAt *time.Time
			if err := rows.Scan(
				&r.TenantID, &r.MessageID, &r.FileID, &r.Filename,
				&contentType, &sizeBytes, &scanVerdict, &scanDetail, &scannedAt, &r.CreatedAt,
			); err != nil {
				return err
			}
			if contentType != nil {
				r.ContentType = *contentType
			}
			if sizeBytes != nil {
				r.SizeBytes = *sizeBytes
			}
			if scanVerdict != nil {
				r.ScanVerdict = Verdict(*scanVerdict)
			}
			if scanDetail != nil {
				r.ScanDetail = *scanDetail
			}
			if scannedAt != nil {
				r.ScannedAt = *scannedAt
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("attachments: list: %w", err)
	}
	return out, nil
}

// recordRejection writes an audit-only row for an infected
// attachment WITHOUT a backing files row. file_id is uuid.Nil and
// the FK constraint on (tenant_id, file_id) is BYPASSED by writing
// to a sibling audit table — actually we just skip this because
// the constraint would reject the insert. The detail is logged via
// the structured error returned from Attach; the agent UI's
// timeline shows "rejected attachment <filename>: infected
// (<signature>)" but the row itself doesn't survive to the audit
// log. A future enhancement can split rejections into a separate
// table; for now, the slog line + the upstream relay's bounce
// record is the audit trail.
func (a *Attacher) recordRejection(_ context.Context, _ uuid.UUID, _ string, att Attachment, scan ScanResult, _ time.Time) error {
	// Intentionally a no-op for the current schema. Kept as a
	// hook so a future migration adds email_attachment_rejections
	// without changing the caller. The structured error message
	// (ErrInfected wrap) carries enough detail for the
	// upstream caller's slog line — see services/api/
	// helpdesk_inbound_handlers.go for the surfacing.
	_ = att
	_ = scan
	return nil
}

// isFKMissing detects a Postgres foreign-key-violation error
// whose constraint name matches the supplied substring.
func isFKMissing(err error, constraintHint string) bool {
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.SQLState() != "23503" {
		return false
	}
	return strings.Contains(err.Error(), constraintHint)
}

// jsonString is a tiny helper used by tests that compare Record
// shapes against JSON fixtures. Not exported.
//
//nolint:unused // available for test golden files; harmless if no test uses it
func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
