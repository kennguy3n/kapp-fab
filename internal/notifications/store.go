// Package notifications persists in-app notices for the bell/inbox
// surface. External transports (KChat DM, webhook, email) are served
// by services/worker/notifications.go; this package is the durable
// record that survives transport failures and drives the web UI inbox.
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Notification mirrors one row of the notifications table. Payload
// is the raw event envelope the notice was derived from; the inbox UI
// renders the title/body and links back through payload.
type Notification struct {
	ID        uuid.UUID       `json:"id"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	UserID    *uuid.UUID      `json:"user_id,omitempty"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Read      bool            `json:"read"`
	CreatedAt time.Time       `json:"created_at"`
	ReadAt    *time.Time      `json:"read_at,omitempty"`
}

// CreateInput is the payload accepted by Store.Create. UserID may be
// nil for tenant-wide notices (e.g. "period closed"), which the inbox
// renders for every user in the tenant.
type CreateInput struct {
	TenantID uuid.UUID       `json:"tenant_id"`
	UserID   *uuid.UUID      `json:"user_id,omitempty"`
	Type     string          `json:"type"`
	Title    string          `json:"title"`
	Body     string          `json:"body"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// ListFilter is the option bag accepted by Store.List.
type ListFilter struct {
	UserID     *uuid.UUID
	UnreadOnly bool
	Limit      int
}

// Store is the Postgres-backed notifications persistence. All reads
// and writes go through SET LOCAL app.tenant_id so RLS enforces the
// tenant boundary just like every other kernel table.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a store backed by the supplied pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ErrNotFound is returned by MarkRead when the row doesn't exist in
// the current tenant's RLS scope.
var ErrNotFound = errors.New("notification: not found")

// Create persists a notification. Returns the inserted row (with id
// and created_at populated by the database).
func (s *Store) Create(ctx context.Context, in CreateInput) (*Notification, error) {
	if in.TenantID == uuid.Nil {
		return nil, errors.New("notification: tenant_id is required")
	}
	if in.Type == "" {
		return nil, errors.New("notification: type is required")
	}
	payload := in.Payload
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	id := uuid.New()
	var n Notification
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO notifications
                (id, tenant_id, user_id, type, title, body, payload)
             VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
             RETURNING id, tenant_id, user_id, type, title, body, payload, read, created_at, read_at`,
			id, in.TenantID, in.UserID, in.Type, in.Title, in.Body, payload,
		).Scan(&n.ID, &n.TenantID, &n.UserID, &n.Type, &n.Title, &n.Body, &n.Payload, &n.Read, &n.CreatedAt, &n.ReadAt)
	})
	if err != nil {
		return nil, fmt.Errorf("notification: insert: %w", err)
	}
	return &n, nil
}

// List returns the latest notifications visible to the caller. When
// filter.UserID is set, rows are restricted to either that user or
// tenant-wide notices (user_id IS NULL). Default limit is 50.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID, filter ListFilter) ([]Notification, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	out := make([]Notification, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT id, tenant_id, user_id, type, title, body, payload, read, created_at, read_at
              FROM notifications
              WHERE tenant_id = $1`
		args := []any{tenantID}
		if filter.UserID != nil {
			q += " AND (user_id = $2 OR user_id IS NULL)"
			args = append(args, *filter.UserID)
		}
		if filter.UnreadOnly {
			q += " AND read = FALSE"
		}
		q += " ORDER BY created_at DESC LIMIT " + fmt.Sprintf("%d", filter.Limit)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n Notification
			if err := rows.Scan(&n.ID, &n.TenantID, &n.UserID, &n.Type, &n.Title, &n.Body, &n.Payload, &n.Read, &n.CreatedAt, &n.ReadAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("notification: list: %w", err)
	}
	return out, nil
}

// MarkRead flips the read flag and stamps read_at for the row scoped
// to (tenant, id). Returns ErrNotFound if RLS + id did not match a row.
func (s *Store) MarkRead(ctx context.Context, tenantID, id uuid.UUID) error {
	var rowsAffected int64
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE notifications
                SET read = TRUE, read_at = COALESCE(read_at, now())
             WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		rowsAffected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("notification: mark read: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead marks every row for the supplied (tenant, user) as
// read. Passing user=nil only flips the tenant-wide notices.
func (s *Store) MarkAllRead(ctx context.Context, tenantID uuid.UUID, userID *uuid.UUID) (int64, error) {
	var n int64
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := `UPDATE notifications
                SET read = TRUE, read_at = COALESCE(read_at, now())
             WHERE tenant_id = $1 AND read = FALSE`
		args := []any{tenantID}
		if userID != nil {
			q += " AND (user_id = $2 OR user_id IS NULL)"
			args = append(args, *userID)
		}
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("notification: mark all read: %w", err)
	}
	return n, nil
}
