package imap

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// PGUIDState persists checkpoints to helpdesk_imap_state. The
// type lives here (rather than in services/worker) so the Manager
// wiring is symmetric with the rest of the helpdesk surface.
type PGUIDState struct {
	pool *pgxpool.Pool
}

// NewPGUIDState wires the persistence backend over the shared
// pool.
func NewPGUIDState(pool *pgxpool.Pool) *PGUIDState {
	return &PGUIDState{pool: pool}
}

// Get returns (uid_validity, last_uid). A missing row returns
// (0, 0, nil) so the caller treats it as "first poll".
func (s *PGUIDState) Get(ctx context.Context, tenantID, mailboxID uuid.UUID) (uidValidity, lastUID uint32, err error) {
	var uv, lu int64
	err = dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT uid_validity, last_uid FROM helpdesk_imap_state WHERE tenant_id = $1 AND mailbox_id = $2`,
			tenantID, mailboxID,
		).Scan(&uv, &lu)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("imap: state get: %w", err)
	}
	// uv / lu are stored as BIGINT but only ever hold values
	// produced by uint32 → int64 in Set. Clamp defensively in
	// case a corrupt row carries an out-of-range value (a
	// negative or > MaxUint32 value would otherwise wrap).
	if uv < 0 || uv > 0xFFFFFFFF {
		uv = 0
	}
	if lu < 0 || lu > 0xFFFFFFFF {
		lu = 0
	}
	return uint32(uv), uint32(lu), nil //nolint:gosec // bounded above
}

// Set upserts the checkpoint + clears any error state in the
// same statement (so a successful poll always resets
// consecutive_errors atomically with the UID advance).
func (s *PGUIDState) Set(ctx context.Context, tenantID, mailboxID uuid.UUID, uidValidity, lastUID uint32) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO helpdesk_imap_state
                 (tenant_id, mailbox_id, uid_validity, last_uid, last_polled_at, consecutive_errors, last_error)
             VALUES ($1, $2, $3, $4, now(), 0, NULL)
             ON CONFLICT (tenant_id, mailbox_id) DO UPDATE
             SET uid_validity = EXCLUDED.uid_validity,
                 last_uid = EXCLUDED.last_uid,
                 last_polled_at = now(),
                 consecutive_errors = 0,
                 last_error = NULL`,
			tenantID, mailboxID, int64(uidValidity), int64(lastUID),
		)
		return err
	})
}

// RecordError increments consecutive_errors + stores the message.
// last_polled_at is updated so the dashboard reflects the most
// recent attempt regardless of outcome.
func (s *PGUIDState) RecordError(ctx context.Context, tenantID, mailboxID uuid.UUID, message string) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO helpdesk_imap_state
                 (tenant_id, mailbox_id, uid_validity, last_uid, last_polled_at, consecutive_errors, last_error)
             VALUES ($1, $2, 0, 0, now(), 1, $3)
             ON CONFLICT (tenant_id, mailbox_id) DO UPDATE
             SET last_polled_at = now(),
                 consecutive_errors = helpdesk_imap_state.consecutive_errors + 1,
                 last_error = $3`,
			tenantID, mailboxID, message,
		)
		return err
	})
}

// ClearError resets consecutive_errors to 0 without changing UID
// state. Called when the poll succeeded but processed zero new
// messages (so Set wasn't called).
func (s *PGUIDState) ClearError(ctx context.Context, tenantID, mailboxID uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE helpdesk_imap_state
             SET consecutive_errors = 0,
                 last_error = NULL,
                 last_polled_at = now()
             WHERE tenant_id = $1 AND mailbox_id = $2`,
			tenantID, mailboxID,
		)
		return err
	})
}
