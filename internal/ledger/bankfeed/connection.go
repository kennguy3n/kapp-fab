package bankfeed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Encryptor is the subset of internal/tenant.KeyManager the connection
// store needs to seal provider credentials at rest. Declaring it as a
// local interface keeps the package decoupled from the tenant package
// (no import cycle risk) and lets tests inject a deterministic fake.
type Encryptor interface {
	EncryptString(tenantID uuid.UUID, plaintext string) (string, error)
	DecryptString(tenantID uuid.UUID, value string) (string, error)
}

// ConnectionStore persists bank_feed_connections with the OAuth/API
// credentials field-encrypted. Reads decrypt transparently; the
// decrypted tokens live only in the returned *Connection in memory and
// are never logged. All writes run under WithTenantTx so RLS is the
// final guarantor of tenant isolation, and every mutation emits an
// audit entry inside the same transaction.
type ConnectionStore struct {
	pool    *pgxpool.Pool
	enc     Encryptor
	auditor audit.Logger
	now     func() time.Time
}

// NewConnectionStore wires a store. enc may be nil in development (the
// platform boot gate requires KAPP_MASTER_KEY in production), in which
// case credentials are stored verbatim — acceptable only for local dev
// against sandbox providers. auditor may be nil in tests.
func NewConnectionStore(pool *pgxpool.Pool, enc Encryptor, auditor audit.Logger) *ConnectionStore {
	return &ConnectionStore{
		pool:    pool,
		enc:     enc,
		auditor: auditor,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithClock pins the clock for deterministic tests.
func (s *ConnectionStore) WithClock(now func() time.Time) *ConnectionStore {
	if now != nil {
		s.now = now
	}
	return s
}

// seal encrypts a credential, returning the BYTEA payload to persist.
// Empty plaintext stores SQL NULL so absent tokens (e.g. the CSV
// provider) are distinguishable from an empty ciphertext.
func (s *ConnectionStore) seal(tenantID uuid.UUID, plaintext string) (any, error) {
	if plaintext == "" {
		return nil, nil
	}
	if s.enc == nil {
		return []byte(plaintext), nil
	}
	ct, err := s.enc.EncryptString(tenantID, plaintext)
	if err != nil {
		return nil, fmt.Errorf("bankfeed: seal credential: %w", err)
	}
	return []byte(ct), nil
}

// open reverses seal. A NULL column yields an empty string.
func (s *ConnectionStore) open(tenantID uuid.UUID, raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if s.enc == nil {
		return string(raw), nil
	}
	pt, err := s.enc.DecryptString(tenantID, string(raw))
	if err != nil {
		return "", fmt.Errorf("bankfeed: open credential: %w", err)
	}
	return pt, nil
}

// UpsertConnection inserts or updates a connection by (tenant_id, id),
// re-encrypting the credentials each write. A fresh connection with a
// nil ID is assigned one. The audit entry records the provider and
// account but never the token material.
func (s *ConnectionStore) UpsertConnection(ctx context.Context, c Connection) (*Connection, error) {
	if c.TenantID == uuid.Nil {
		return nil, errors.New("bankfeed: tenant id required")
	}
	if c.BankAccountID == uuid.Nil {
		return nil, errors.New("bankfeed: bank_account_id required")
	}
	if c.Provider == "" {
		return nil, errors.New("bankfeed: provider required")
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.Status == "" {
		c.Status = StatusActive
	}
	accessEnc, err := s.seal(c.TenantID, c.AccessToken)
	if err != nil {
		return nil, err
	}
	refreshEnc, err := s.seal(c.TenantID, c.RefreshToken)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := c
	err = dbutil.WithTenantTx(ctx, s.pool, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// RETURNING created_at/updated_at so the caller gets the true
		// persisted timestamps. On the ON CONFLICT update path the DB keeps
		// the original created_at (it is not in the SET clause), so reading
		// it back avoids reporting a fresh created_at for a connection that
		// was actually first established earlier.
		if err := tx.QueryRow(ctx,
			`INSERT INTO bank_feed_connections
			     (tenant_id, id, bank_account_id, provider, access_token_enc,
			      refresh_token_enc, cursor, external_id, status, last_sync_at,
			      last_error, created_at, updated_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET
			     bank_account_id   = EXCLUDED.bank_account_id,
			     provider          = EXCLUDED.provider,
			     access_token_enc  = EXCLUDED.access_token_enc,
			     refresh_token_enc = EXCLUDED.refresh_token_enc,
			     cursor            = EXCLUDED.cursor,
			     external_id       = EXCLUDED.external_id,
			     status            = EXCLUDED.status,
			     -- Preserve an established sync position across a
			     -- credential refresh / re-link: a caller updating
			     -- credentials may not repopulate last_sync_at, and
			     -- clobbering it with NULL would make the next sync
			     -- re-pull the full lookback window. COALESCE keeps the
			     -- existing timestamp when the incoming value is NULL.
			     last_sync_at      = COALESCE(EXCLUDED.last_sync_at, bank_feed_connections.last_sync_at),
			     last_error        = EXCLUDED.last_error,
			     updated_at        = EXCLUDED.updated_at
			 RETURNING created_at, updated_at, last_sync_at`,
			c.TenantID, c.ID, c.BankAccountID, c.Provider, accessEnc,
			refreshEnc, nullIfEmpty(c.Cursor), nullIfEmpty(c.ExternalID), c.Status,
			c.LastSyncAt, nullIfEmpty(c.LastError), now,
		).Scan(&out.CreatedAt, &out.UpdatedAt, &out.LastSyncAt); err != nil {
			return fmt.Errorf("bankfeed: upsert connection: %w", err)
		}
		return s.auditConnection(ctx, tx, c, "finance.bank_feed.connection.upsert")
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetConnection loads one connection by id with credentials decrypted.
func (s *ConnectionStore) GetConnection(ctx context.Context, tenantID, id uuid.UUID) (*Connection, error) {
	var c *Connection
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, connectionSelect+` WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		loaded, err := s.scanConnection(tenantID, row)
		if err != nil {
			return err
		}
		c = loaded
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("bankfeed: connection %s: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return c, nil
}

// ListConnectionsByAccount returns all connections for a bank account,
// newest first, with credentials decrypted. Used by the connection
// status indicator on the reconciliation page.
func (s *ConnectionStore) ListConnectionsByAccount(ctx context.Context, tenantID, bankAccountID uuid.UUID) ([]Connection, error) {
	return s.queryConnections(ctx, tenantID,
		connectionSelect+` WHERE tenant_id = $1 AND bank_account_id = $2 ORDER BY created_at DESC`,
		tenantID, bankAccountID)
}

// ListActiveConnections returns every active connection for the tenant.
// The sync scheduler walks this list each tick.
func (s *ConnectionStore) ListActiveConnections(ctx context.Context, tenantID uuid.UUID) ([]Connection, error) {
	return s.queryConnections(ctx, tenantID,
		connectionSelect+` WHERE tenant_id = $1 AND status = $2 ORDER BY created_at`,
		tenantID, StatusActive)
}

// AdvanceCursor records a successful sync: updates the cursor, stamps
// last_sync_at, and clears any prior last_error. Credentials are left
// untouched so this cheap write does not re-encrypt the tokens.
func (s *ConnectionStore) AdvanceCursor(ctx context.Context, tenantID, id uuid.UUID, cursor string, syncedAt time.Time) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE bank_feed_connections
			    SET cursor = $3, last_sync_at = $4, last_error = NULL, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, nullIfEmpty(cursor), syncedAt)
		if err != nil {
			return fmt.Errorf("bankfeed: advance cursor: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("bankfeed: connection %s: %w", id, ErrNotFound)
		}
		return nil
	})
}

// MarkError records a sync failure on the connection without changing
// its status (a transient failure should not disable the feed). The
// error string is operator-facing and must not contain credentials —
// callers pass a sanitized message.
func (s *ConnectionStore) MarkError(ctx context.Context, tenantID, id uuid.UUID, msg string) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE bank_feed_connections
			    SET last_error = $3, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, nullIfEmpty(msg), s.now())
		return err
	})
}

// SetStatus transitions a connection's lifecycle state (e.g. to
// 'revoked' on disconnect, 'expired' when the provider rejects the
// token). Emits an audit entry.
func (s *ConnectionStore) SetStatus(ctx context.Context, tenantID, id uuid.UUID, status string) error {
	if status != StatusActive && status != StatusExpired && status != StatusRevoked {
		return fmt.Errorf("bankfeed: invalid status %q", status)
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE bank_feed_connections
			    SET status = $3, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, status, s.now())
		if err != nil {
			return fmt.Errorf("bankfeed: set status: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("bankfeed: connection %s: %w", id, ErrNotFound)
		}
		return s.auditConnection(ctx, tx, Connection{TenantID: tenantID, ID: id, Status: status},
			"finance.bank_feed.connection.status")
	})
}

// connectionSelect is the column list shared by all connection reads.
const connectionSelect = `SELECT tenant_id, id, bank_account_id, provider,
	access_token_enc, refresh_token_enc, cursor, external_id, status,
	last_sync_at, last_error, created_at, updated_at
	FROM bank_feed_connections`

// rowScanner abstracts pgx.Row / pgx.Rows for scanConnection.
type rowScanner interface {
	Scan(dest ...any) error
}

func (s *ConnectionStore) scanConnection(tenantID uuid.UUID, row rowScanner) (*Connection, error) {
	var (
		c          Connection
		accessEnc  []byte
		refreshEnc []byte
		cursor     *string
		externalID *string
		lastErr    *string
	)
	if err := row.Scan(&c.TenantID, &c.ID, &c.BankAccountID, &c.Provider,
		&accessEnc, &refreshEnc, &cursor, &externalID, &c.Status,
		&c.LastSyncAt, &lastErr, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	access, err := s.open(tenantID, accessEnc)
	if err != nil {
		return nil, err
	}
	refresh, err := s.open(tenantID, refreshEnc)
	if err != nil {
		return nil, err
	}
	c.AccessToken = access
	c.RefreshToken = refresh
	if cursor != nil {
		c.Cursor = *cursor
	}
	if externalID != nil {
		c.ExternalID = *externalID
	}
	if lastErr != nil {
		c.LastError = *lastErr
	}
	return &c, nil
}

func (s *ConnectionStore) queryConnections(ctx context.Context, tenantID uuid.UUID, sql string, args ...any) ([]Connection, error) {
	var out []Connection
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("bankfeed: query connections: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := s.scanConnection(tenantID, rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// auditConnection writes a credential-free audit breadcrumb. The
// connection id is the audit target; the context records the provider,
// account, and status only.
func (s *ConnectionStore) auditConnection(ctx context.Context, tx pgx.Tx, c Connection, action string) error {
	if s.auditor == nil {
		return nil
	}
	id := c.ID
	return s.auditor.LogTx(ctx, tx, audit.Entry{
		TenantID:    c.TenantID,
		ActorKind:   audit.ActorSystem,
		Action:      action,
		TargetKType: "finance.bank_feed_connection",
		TargetID:    &id,
		Context:     auditContext(c),
	})
}
