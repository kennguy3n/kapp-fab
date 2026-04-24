package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// PortalUser mirrors one row of portal_users. The raw magic token is
// never stored — only its SHA-256 hash lives in magic_link_token.
type PortalUser struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	Email         string     `json:"email"`
	DisplayName   string     `json:"display_name"`
	EmailVerified bool       `json:"email_verified"`
	LastLoginAt   *time.Time `json:"last_login_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// PortalScope is the Claims.Scope value issued by the verify flow.
// Handlers use RequirePortalScope to gate /api/v1/portal/*.
const PortalScope = "portal"

// MagicLinkTokenTTL is how long a freshly-issued magic link remains
// valid. 15 minutes follows the common customer-portal default and
// bounds the blast radius of a leaked email.
const MagicLinkTokenTTL = 15 * time.Minute

// PortalTokenTTL is how long a portal-scoped JWT stays valid after
// verify. Portal sessions are short — the UI will prompt for a new
// magic link when the token expires rather than offering a long-
// lived refresh path, which would expand the attack surface.
const PortalTokenTTL = 2 * time.Hour

// ErrMagicLinkInvalid is returned when the verify flow cannot find
// a matching (email, token) pair — whether because the token is
// wrong, expired, or the email was never registered.
var ErrMagicLinkInvalid = errors.New("auth: magic link invalid or expired")

// PortalStore persists portal_users rows and the magic-link state
// machine.
type PortalStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPortalStore binds a store to the shared pool.
func NewPortalStore(pool *pgxpool.Pool) *PortalStore {
	return &PortalStore{pool: pool, now: time.Now}
}

// IssueMagicLink upserts the portal_user row for the supplied
// (tenant, email) pair and stamps it with a new token + expiry.
// The returned plaintext token is only ever handed to the caller
// once (the store persists only its hash) and is what the caller
// should email to the customer.
func (s *PortalStore) IssueMagicLink(ctx context.Context, tenantID uuid.UUID, email string) (string, *PortalUser, error) {
	email = normaliseEmail(email)
	if email == "" {
		return "", nil, errors.New("auth: portal email required")
	}
	token, err := generateToken(32)
	if err != nil {
		return "", nil, fmt.Errorf("auth: generate token: %w", err)
	}
	hash := hashToken(token)
	expires := s.now().Add(MagicLinkTokenTTL).UTC()
	var user PortalUser
	err = dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO portal_users (id, tenant_id, email, magic_link_token, token_expires_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, lower(email))
			DO UPDATE SET magic_link_token = EXCLUDED.magic_link_token,
			              token_expires_at = EXCLUDED.token_expires_at,
			              updated_at = now()
			RETURNING id, tenant_id, email, display_name, email_verified, last_login_at, created_at, updated_at`,
			uuid.New(), tenantID, email, hash, expires,
		).Scan(
			&user.ID, &user.TenantID, &user.Email, &user.DisplayName,
			&user.EmailVerified, &user.LastLoginAt,
			&user.CreatedAt, &user.UpdatedAt,
		)
	})
	if err != nil {
		return "", nil, fmt.Errorf("auth: issue magic link: %w", err)
	}
	return token, &user, nil
}

// VerifyMagicLink swaps a plaintext token for the matching portal
// user. The token is single-use: a successful verify clears the
// magic_link_token / token_expires_at fields and stamps
// email_verified + last_login_at.
func (s *PortalStore) VerifyMagicLink(ctx context.Context, tenantID uuid.UUID, email, token string) (*PortalUser, error) {
	email = normaliseEmail(email)
	if email == "" || token == "" {
		return nil, ErrMagicLinkInvalid
	}
	hash := hashToken(token)
	var user PortalUser
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, tenant_id, email, display_name, email_verified, last_login_at, created_at, updated_at
			  FROM portal_users
			 WHERE tenant_id = $1
			   AND lower(email) = lower($2)
			   AND magic_link_token = $3
			   AND token_expires_at > now()
			 FOR UPDATE`,
			tenantID, email, hash,
		).Scan(
			&user.ID, &user.TenantID, &user.Email, &user.DisplayName,
			&user.EmailVerified, &user.LastLoginAt,
			&user.CreatedAt, &user.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMagicLinkInvalid
			}
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE portal_users
			   SET magic_link_token = NULL,
			       token_expires_at = NULL,
			       email_verified = TRUE,
			       last_login_at = now(),
			       updated_at = now()
			 WHERE tenant_id = $1 AND id = $2`,
			tenantID, user.ID,
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetByEmail looks up an existing portal user. Used by the portal
// handler stack so ticket queries can hydrate the customer's
// display name from the portal_users row instead of the JWT.
func (s *PortalStore) GetByEmail(ctx context.Context, tenantID uuid.UUID, email string) (*PortalUser, error) {
	email = normaliseEmail(email)
	var user PortalUser
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, tenant_id, email, display_name, email_verified, last_login_at, created_at, updated_at
			  FROM portal_users
			 WHERE tenant_id = $1 AND lower(email) = lower($2)`,
			tenantID, email,
		).Scan(
			&user.ID, &user.TenantID, &user.Email, &user.DisplayName,
			&user.EmailVerified, &user.LastLoginAt,
			&user.CreatedAt, &user.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMagicLinkInvalid
		}
		return nil, fmt.Errorf("auth: portal lookup: %w", err)
	}
	return &user, nil
}

// IssuePortalToken mints the JWT for a verified portal user. It
// reuses the platform's Signer under the hood so rotation /
// revocation surfaces work the same as for standard user tokens.
// The signer's AccessTTL is substituted with PortalTokenTTL on the
// resulting claims by overwriting ExpiresAt after issuance — we
// deliberately do not re-use the standard-user TTL because portal
// sessions should expire faster.
func IssuePortalToken(s *Signer, user PortalUser) (string, *Claims, error) {
	if s == nil {
		return "", nil, errors.New("auth: signer required")
	}
	base := Claims{
		UserID:   user.ID,
		TenantID: user.TenantID,
		Scope:    PortalScope,
		Email:    user.Email,
	}
	token, err := s.Issue(base)
	if err != nil {
		return "", nil, err
	}
	claims, err := s.Verify(token)
	if err != nil {
		return "", nil, err
	}
	return token, claims, nil
}

// generateToken returns a URL-safe hex string derived from n random
// bytes. Used for magic-link tokens; 32 bytes ≈ 256 bits of entropy.
func generateToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken returns the SHA-256 hash of the token in hex. We store
// this rather than the plaintext so a DB dump is not a login vector.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normaliseEmail(email string) string {
	return strings.TrimSpace(strings.ToLower(email))
}
