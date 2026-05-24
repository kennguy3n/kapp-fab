package mailboxes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Store is the read/write seam used by the admin API (CRUD per
// tenant under RLS) and the worker's supervisor (read-only
// enumerate-all-enabled scan via the admin pool's bypass policy).
// Implementations must be safe for concurrent use.
type Store interface {
	// Create inserts a new mailbox row under the tenant's RLS
	// context. Returns the freshly-persisted row including the
	// server-assigned created_at / updated_at timestamps.
	// Validates the input via Mailbox.Validate before reaching
	// the DB.
	Create(ctx context.Context, m Mailbox) (*Mailbox, error)
	// Get loads one mailbox by primary key.
	Get(ctx context.Context, tenantID, mailboxID uuid.UUID) (*Mailbox, error)
	// List returns every mailbox for the given tenant ordered by
	// (lower(name), mailbox_id) so the admin UI gets a stable
	// listing across requests.
	List(ctx context.Context, tenantID uuid.UUID) ([]Mailbox, error)
	// Update replaces the mutable fields on the existing row.
	// The (tenant_id, mailbox_id) pair is the key; everything
	// else is overwritten. Validates via Mailbox.Validate.
	Update(ctx context.Context, m Mailbox) (*Mailbox, error)
	// Delete removes one mailbox by primary key. Returns
	// ErrNotFound when no row matched (so the admin API can
	// surface a 404 rather than silently 204'ing).
	Delete(ctx context.Context, tenantID, mailboxID uuid.UUID) error
	// ListAllEnabled enumerates every enabled mailbox across
	// every tenant, used by the worker's supervisor at boot +
	// on convergence ticks. Runs against the admin pool via the
	// admin-bypass RLS policy on helpdesk_mailboxes.
	ListAllEnabled(ctx context.Context) ([]Mailbox, error)
}

// PGStore is the Postgres implementation of Store. It carries two
// pool handles: the tenant-scoped pool for CRUD-under-RLS, and the
// admin pool for the cross-tenant supervisor scan. Callers that
// only do CRUD may pass nil for adminPool — ListAllEnabled will
// fail with a clear error in that case.
type PGStore struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
	now       func() time.Time
}

// NewPGStore wires a store with both pool handles. The adminPool may
// be nil if the caller does not need ListAllEnabled (e.g. the admin
// API only — the worker has its own constructor path).
func NewPGStore(pool, adminPool *pgxpool.Pool) *PGStore {
	return &PGStore{
		pool:      pool,
		adminPool: adminPool,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// SetClock is a test-only hook. Production callers should not change
// the clock after construction.
func (s *PGStore) SetClock(now func() time.Time) { s.now = now }

// Create inserts a row under the tenant's RLS context. The PRIMARY
// KEY conflict is treated as ErrDuplicateNameForTenant when the
// constraint that fires is the unique-name index; the generic
// duplicate-key path returns a wrapped pgx error so the admin API
// can render a 409.
func (s *PGStore) Create(ctx context.Context, m Mailbox) (*Mailbox, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	out := m
	if out.CreatedAt.IsZero() {
		out.CreatedAt = s.now()
	}
	out.UpdatedAt = out.CreatedAt
	err := dbutil.WithTenantTx(ctx, s.pool, m.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, perr := tx.Exec(ctx, `
			INSERT INTO helpdesk_mailboxes (
				tenant_id, mailbox_id, name,
				imap_host, imap_port, imap_username,
				imap_password_ref, imap_use_tls, folder,
				poll_interval_seconds, max_backoff_seconds, fetch_batch_size,
				enabled, created_at, updated_at
			) VALUES (
				$1, $2, $3,
				$4, $5, $6,
				$7, $8, $9,
				$10, $11, $12,
				$13, $14, $15
			)`,
			out.TenantID, out.MailboxID, out.Name,
			out.IMAPHost, out.IMAPPort, out.IMAPUsername,
			out.IMAPPasswordRef, out.IMAPUseTLS, out.Folder,
			out.PollIntervalSeconds, out.MaxBackoffSeconds, out.FetchBatchSize,
			out.Enabled, out.CreatedAt, out.UpdatedAt,
		)
		return perr
	})
	if err != nil {
		return nil, fmt.Errorf("mailboxes: create: %w", err)
	}
	return &out, nil
}

// Get loads one mailbox by primary key under the tenant's RLS
// context. ErrNotFound when the row is absent.
func (s *PGStore) Get(ctx context.Context, tenantID, mailboxID uuid.UUID) (*Mailbox, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenantID
	}
	if mailboxID == uuid.Nil {
		return nil, ErrInvalidMailboxID
	}
	var out Mailbox
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanMailbox(tx.QueryRow(ctx, `
			SELECT tenant_id, mailbox_id, name,
			       imap_host, imap_port, imap_username,
			       imap_password_ref, imap_use_tls, folder,
			       poll_interval_seconds, max_backoff_seconds, fetch_batch_size,
			       enabled, created_at, updated_at
			  FROM helpdesk_mailboxes
			 WHERE tenant_id = $1 AND mailbox_id = $2`,
			tenantID, mailboxID,
		), &out)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mailboxes: get: %w", err)
	}
	return &out, nil
}

// List returns every mailbox for one tenant. The ORDER BY
// (lower(name), mailbox_id) matches the helpdesk_mailboxes_tenant_name_idx
// index so the scan stays index-only.
func (s *PGStore) List(ctx context.Context, tenantID uuid.UUID) ([]Mailbox, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenantID
	}
	var out []Mailbox
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, qerr := tx.Query(ctx, `
			SELECT tenant_id, mailbox_id, name,
			       imap_host, imap_port, imap_username,
			       imap_password_ref, imap_use_tls, folder,
			       poll_interval_seconds, max_backoff_seconds, fetch_batch_size,
			       enabled, created_at, updated_at
			  FROM helpdesk_mailboxes
			 WHERE tenant_id = $1
			 ORDER BY lower(name) ASC, mailbox_id ASC`,
			tenantID,
		)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var m Mailbox
			if serr := scanMailbox(rows, &m); serr != nil {
				return serr
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("mailboxes: list: %w", err)
	}
	return out, nil
}

// Update overwrites the mutable fields of an existing row. Returns
// ErrNotFound when no row matched the PK.
func (s *PGStore) Update(ctx context.Context, m Mailbox) (*Mailbox, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	out := m
	out.UpdatedAt = s.now()
	err := dbutil.WithTenantTx(ctx, s.pool, m.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, perr := tx.Exec(ctx, `
			UPDATE helpdesk_mailboxes
			   SET name = $3,
			       imap_host = $4,
			       imap_port = $5,
			       imap_username = $6,
			       imap_password_ref = $7,
			       imap_use_tls = $8,
			       folder = $9,
			       poll_interval_seconds = $10,
			       max_backoff_seconds = $11,
			       fetch_batch_size = $12,
			       enabled = $13,
			       updated_at = $14
			 WHERE tenant_id = $1 AND mailbox_id = $2`,
			out.TenantID, out.MailboxID, out.Name,
			out.IMAPHost, out.IMAPPort, out.IMAPUsername,
			out.IMAPPasswordRef, out.IMAPUseTLS, out.Folder,
			out.PollIntervalSeconds, out.MaxBackoffSeconds, out.FetchBatchSize,
			out.Enabled, out.UpdatedAt,
		)
		if perr != nil {
			return perr
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("mailboxes: update: %w", err)
	}
	// Re-read so CreatedAt is preserved on the response. Cheap;
	// the row is hot in the WAL cache.
	return s.Get(ctx, out.TenantID, out.MailboxID)
}

// Delete removes one mailbox. Returns ErrNotFound when no row matched.
func (s *PGStore) Delete(ctx context.Context, tenantID, mailboxID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenantID
	}
	if mailboxID == uuid.Nil {
		return ErrInvalidMailboxID
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, perr := tx.Exec(ctx,
			`DELETE FROM helpdesk_mailboxes WHERE tenant_id = $1 AND mailbox_id = $2`,
			tenantID, mailboxID,
		)
		if perr != nil {
			return perr
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("mailboxes: delete: %w", err)
	}
	return nil
}

// ListAllEnabled scans the admin pool for every enabled mailbox
// across every tenant. The query runs WITHOUT setting app.tenant_id
// so the admin-bypass RLS policy on helpdesk_mailboxes (000058) is
// what permits the read. Returns an error if adminPool was nil.
func (s *PGStore) ListAllEnabled(ctx context.Context) ([]Mailbox, error) {
	if s.adminPool == nil {
		return nil, errors.New("mailboxes: admin pool not configured (constructor called with nil adminPool)")
	}
	rows, err := s.adminPool.Query(ctx, `
		SELECT tenant_id, mailbox_id, name,
		       imap_host, imap_port, imap_username,
		       imap_password_ref, imap_use_tls, folder,
		       poll_interval_seconds, max_backoff_seconds, fetch_batch_size,
		       enabled, created_at, updated_at
		  FROM helpdesk_mailboxes
		 WHERE enabled = TRUE
		 ORDER BY tenant_id ASC, mailbox_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("mailboxes: list-all-enabled: %w", err)
	}
	defer rows.Close()
	var out []Mailbox
	for rows.Next() {
		var m Mailbox
		if serr := scanMailbox(rows, &m); serr != nil {
			return nil, fmt.Errorf("mailboxes: scan: %w", serr)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mailboxes: rows iteration: %w", err)
	}
	return out, nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows so the same
// scan helper serves Get / List / ListAllEnabled without duplication.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanMailbox unmarshals one row into m. Nullable INTEGER columns
// decode into *int via pgx's standard nullable handling.
func scanMailbox(r rowScanner, m *Mailbox) error {
	var pollInterval, maxBackoff, fetchBatch *int
	err := r.Scan(
		&m.TenantID, &m.MailboxID, &m.Name,
		&m.IMAPHost, &m.IMAPPort, &m.IMAPUsername,
		&m.IMAPPasswordRef, &m.IMAPUseTLS, &m.Folder,
		&pollInterval, &maxBackoff, &fetchBatch,
		&m.Enabled, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		return err
	}
	m.PollIntervalSeconds = pollInterval
	m.MaxBackoffSeconds = maxBackoff
	m.FetchBatchSize = fetchBatch
	// Trim any whitespace at the boundary; the DB stores TEXT so
	// trailing whitespace would survive otherwise and break
	// host-comparison code paths downstream.
	m.IMAPHost = strings.TrimSpace(m.IMAPHost)
	m.IMAPUsername = strings.TrimSpace(m.IMAPUsername)
	m.Folder = strings.TrimSpace(m.Folder)
	return nil
}
