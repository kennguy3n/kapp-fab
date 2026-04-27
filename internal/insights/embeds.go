package insights

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Embed mirrors a row of insights_embeds. Token is only ever populated
// at create time — subsequent reads expose only TokenDigest because
// the secret is one-way hashed at rest.
type Embed struct {
	TenantID      uuid.UUID       `json:"tenant_id"`
	ID            uuid.UUID       `json:"id"`
	DashboardID   uuid.UUID       `json:"dashboard_id"`
	Token         string          `json:"token,omitempty"`
	TokenDigest   string          `json:"token_digest,omitempty"`
	ScopedFilters json.RawMessage `json:"scoped_filters,omitempty"`
	MaxViews      int             `json:"max_views,omitempty"`
	ViewCount     int             `json:"view_count"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	RevokedAt     *time.Time      `json:"revoked_at,omitempty"`
	CreatedBy     *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Sentinel errors surfaced to API callers.
var (
	ErrEmbedNotFound = errors.New("insights: embed not found")
	ErrEmbedRevoked  = errors.New("insights: embed revoked")
	ErrEmbedExpired  = errors.New("insights: embed expired")
	ErrEmbedExceeded = errors.New("insights: embed view limit reached")
)

// EmbedStore persists insights_embeds rows. Most operations run under
// tenant RLS; the unauth lookup path uses an admin pool to bypass RLS
// for the digest fetch only — it returns the row + tenant id so the
// caller can switch context before running the dashboard.
type EmbedStore struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
}

// NewEmbedStore wires the store with the tenant-scoped pool and a
// separate admin pool (typically the kapp_admin pool) used only for
// the public lookup path.
func NewEmbedStore(pool, adminPool *pgxpool.Pool) *EmbedStore {
	return &EmbedStore{pool: pool, adminPool: adminPool}
}

// generateEmbedToken returns a cryptographically random 256-bit token
// encoded as 43-char base64url (padding stripped). Used at create
// time; the digest column stores the SHA-256 of the secret.
func generateEmbedToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashEmbedToken returns the hex-encoded SHA-256 of a token. The
// digest column uses hex (not base64) so equality comparisons are
// case-insensitive in queries.
func hashEmbedToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create issues a new embed token for the given dashboard. The
// returned Embed includes the plaintext Token — callers must
// surface it to the user immediately because the column stores
// only the digest.
func (s *EmbedStore) Create(ctx context.Context, e Embed) (*Embed, error) {
	if e.TenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if e.DashboardID == uuid.Nil {
		return nil, validationErr("dashboard id required")
	}
	if e.MaxViews < 0 {
		return nil, validationErr("max_views must be >= 0")
	}
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	token, err := generateEmbedToken()
	if err != nil {
		return nil, fmt.Errorf("insights: generate embed token: %w", err)
	}
	digest := hashEmbedToken(token)
	if len(e.ScopedFilters) == 0 {
		e.ScopedFilters = json.RawMessage(`{}`)
	}
	out := e
	err = dbutil.WithTenantTx(ctx, s.pool, e.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if e.CreatedBy != nil {
			createdBy = *e.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO insights_embeds
			   (tenant_id, id, dashboard_id, token_digest,
			    scoped_filters, max_views, expires_at, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING created_at`,
			e.TenantID, e.ID, e.DashboardID, digest,
			[]byte(e.ScopedFilters), e.MaxViews, e.ExpiresAt, createdBy,
		).Scan(&out.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insights: create embed: %w", err)
	}
	out.Token = token
	out.TokenDigest = digest
	return &out, nil
}

// List returns every embed for the dashboard. Token / Digest are
// returned for inventory; the secret has been gone since Create.
func (s *EmbedStore) List(ctx context.Context, tenantID, dashboardID uuid.UUID) ([]Embed, error) {
	if tenantID == uuid.Nil || dashboardID == uuid.Nil {
		return nil, validationErr("tenant id and dashboard id required")
	}
	out := []Embed{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, dashboard_id, token_digest,
			        scoped_filters, max_views, view_count,
			        expires_at, revoked_at, created_by, created_at
			   FROM insights_embeds
			  WHERE tenant_id = $1 AND dashboard_id = $2
			  ORDER BY created_at DESC`,
			tenantID, dashboardID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Embed
			var scoped []byte
			if err := rows.Scan(
				&e.TenantID, &e.ID, &e.DashboardID, &e.TokenDigest,
				&scoped, &e.MaxViews, &e.ViewCount,
				&e.ExpiresAt, &e.RevokedAt, &e.CreatedBy, &e.CreatedAt,
			); err != nil {
				return err
			}
			e.ScopedFilters = scoped
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("insights: list embeds: %w", err)
	}
	return out, nil
}

// Revoke marks the embed as revoked and returns ErrEmbedNotFound if
// no row was updated. Idempotent: revoking an already-revoked embed
// is a no-op (no error). The lookup path checks revoked_at IS NOT
// NULL and returns ErrEmbedRevoked.
func (s *EmbedStore) Revoke(ctx context.Context, tenantID, embedID uuid.UUID) error {
	if tenantID == uuid.Nil || embedID == uuid.Nil {
		return validationErr("tenant id and embed id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE insights_embeds
			    SET revoked_at = COALESCE(revoked_at, now())
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, embedID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrEmbedNotFound
		}
		return nil
	})
}

// LookupByToken is the unauthenticated path. It uses the admin pool
// to fetch the row by digest (RLS doesn't apply because the admin
// role bypasses it), checks expiry / revocation / view-limit, then
// increments view_count atomically. Returns the embed (with
// TenantID populated) so the caller can switch context downstream.
//
// Returns sentinel errors so the HTTP layer maps to the right status:
//
//   - ErrEmbedNotFound     -> 404
//   - ErrEmbedRevoked      -> 410 Gone
//   - ErrEmbedExpired      -> 410 Gone
//   - ErrEmbedExceeded     -> 410 Gone
func (s *EmbedStore) LookupByToken(ctx context.Context, token string) (*Embed, error) {
	if token == "" {
		return nil, ErrEmbedNotFound
	}
	if s.adminPool == nil {
		return nil, errors.New("insights: embed store missing admin pool")
	}
	digest := hashEmbedToken(token)
	var e Embed
	var scoped []byte
	now := time.Now()
	row := s.adminPool.QueryRow(ctx,
		`SELECT tenant_id, id, dashboard_id, token_digest,
		        scoped_filters, max_views, view_count,
		        expires_at, revoked_at, created_by, created_at
		   FROM insights_embeds
		  WHERE token_digest = $1`,
		digest,
	)
	if err := row.Scan(
		&e.TenantID, &e.ID, &e.DashboardID, &e.TokenDigest,
		&scoped, &e.MaxViews, &e.ViewCount,
		&e.ExpiresAt, &e.RevokedAt, &e.CreatedBy, &e.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEmbedNotFound
		}
		return nil, fmt.Errorf("insights: embed lookup: %w", err)
	}
	// Constant-time digest comparison to defeat timing attacks at
	// the hashing layer (the DB's own equality comparison already
	// happens in constant time, but this is cheap insurance for
	// any future migration to a different lookup strategy).
	if subtle.ConstantTimeCompare([]byte(digest), []byte(e.TokenDigest)) != 1 {
		return nil, ErrEmbedNotFound
	}
	if e.RevokedAt != nil {
		return nil, ErrEmbedRevoked
	}
	if e.ExpiresAt != nil && now.After(*e.ExpiresAt) {
		return nil, ErrEmbedExpired
	}
	if e.MaxViews > 0 && e.ViewCount >= e.MaxViews {
		return nil, ErrEmbedExceeded
	}
	e.ScopedFilters = scoped
	// Best-effort view-count bump. A failed update doesn't fail the
	// fetch — the rate limit + audit trail still detect abuse, and
	// the caller may simply observe a stale view_count by one.
	_, _ = s.adminPool.Exec(ctx,
		`UPDATE insights_embeds
		    SET view_count = view_count + 1
		  WHERE token_digest = $1
		    AND (max_views = 0 OR view_count < max_views)
		    AND revoked_at IS NULL
		    AND (expires_at IS NULL OR expires_at > now())`,
		digest,
	)
	return &e, nil
}
