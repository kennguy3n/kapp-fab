package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// KChatProfile is the subset of the KChat user profile we need to
// mint a Kapp JWT. Shapes match KChat's /api/users/me response; other
// fields are ignored so KChat can extend the payload freely.
type KChatProfile struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// KChatClient is the minimum interface our SSO path needs from KChat.
// The Phase H default implementation talks to a KChat HTTP API; tests
// swap this for an in-process fake.
type KChatClient interface {
	ExchangeCode(ctx context.Context, code, redirectURI string) (*KChatProfile, error)
}

// HTTPKChatClient is the live KChat SSO client. It hits
// POST {BaseURL}/oauth/token to exchange the auth code and
// GET {BaseURL}/api/users/me to fetch the profile. The API key is a
// service-to-service credential supplied by KChat admin.
type HTTPKChatClient struct {
	BaseURL  string
	APIKey   string
	client   *http.Client
}

// NewHTTPKChatClient returns a KChat SSO client. BaseURL is the
// KChat origin (no trailing slash required). APIKey is the service
// credential. Both can be empty for tests that stub the interface.
func NewHTTPKChatClient(baseURL, apiKey string) *HTTPKChatClient {
	return &HTTPKChatClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// ExchangeCode performs the OAuth-style code exchange and fetches
// the caller's profile. Both legs must succeed; a partial failure
// returns an error so we never mint a Kapp JWT with a half-populated
// profile.
func (c *HTTPKChatClient) ExchangeCode(ctx context.Context, code, redirectURI string) (*KChatProfile, error) {
	if c.BaseURL == "" {
		return nil, errors.New("auth: kchat base url not configured")
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/oauth/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build token req: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: kchat exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("auth: kchat exchange status=%d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("auth: decode token resp: %w", err)
	}
	if body.AccessToken == "" {
		return nil, errors.New("auth: kchat exchange returned empty access token")
	}
	return c.fetchProfile(ctx, body.AccessToken)
}

func (c *HTTPKChatClient) fetchProfile(ctx context.Context, accessToken string) (*KChatProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/users/me", nil)
	if err != nil {
		return nil, fmt.Errorf("auth: build profile req: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: kchat profile: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("auth: kchat profile status=%d", resp.StatusCode)
	}
	var p KChatProfile
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("auth: decode profile: %w", err)
	}
	if p.ID == "" {
		return nil, errors.New("auth: kchat profile missing id")
	}
	return &p, nil
}

// ExchangeResult wraps the outcome of an SSO exchange: the minted
// Kapp access + refresh tokens, the resolved user row, and the list
// of tenants the user can log into. The API surface exposes tenants
// so the UI can prompt for selection when the user has >1.
type ExchangeResult struct {
	AccessToken  string       `json:"access_token"`
	RefreshToken string       `json:"refresh_token"`
	User         ResolvedUser `json:"user"`
	Tenants      []TenantRef  `json:"tenants"`
	TenantID     uuid.UUID    `json:"tenant_id"`
	SessionID    uuid.UUID    `json:"session_id"`
	ExpiresIn    int64        `json:"expires_in"`
}

// ResolvedUser is the lightweight row the SSO path produces. Mirrors
// the users table columns we persist after first login.
type ResolvedUser struct {
	ID          uuid.UUID `json:"id"`
	KChatUserID string    `json:"kchat_user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
}

// TenantRef is the membership summary returned to the UI so users
// with multi-tenancy can choose.
type TenantRef struct {
	ID   uuid.UUID `json:"id"`
	Slug string    `json:"slug"`
	Name string    `json:"name"`
	Role string    `json:"role"`
}

// SSOService wires the KChat client to the JWT signer and session
// store. A deployment instantiates one of these and calls Exchange
// from the POST /api/v1/auth/sso handler.
type SSOService struct {
	client     KChatClient
	signer     *Signer
	sessions   SessionStore
	pool       *pgxpool.Pool
	adminPool  *pgxpool.Pool
}

// NewSSOService wires an SSOService. pool is the tenant-scoped
// application pool; adminPool (optional) is the BYPASSRLS pool used
// for the cross-tenant membership read. When adminPool is nil the
// membership lookup runs under the app pool and returns nothing
// under default-deny RLS — callers are expected to configure the
// admin pool in production.
func NewSSOService(
	client KChatClient,
	signer *Signer,
	sessions SessionStore,
	pool *pgxpool.Pool,
	adminPool *pgxpool.Pool,
) *SSOService {
	return &SSOService{
		client:    client,
		signer:    signer,
		sessions:  sessions,
		pool:      pool,
		adminPool: adminPool,
	}
}

// Exchange runs the full flow: KChat code exchange, upsert the user
// row, load tenant memberships, pick the default tenant (preferredID
// when the user is a member, else the first membership), mint the
// JWT + refresh, create a session, and return the bundle.
func (s *SSOService) Exchange(
	ctx context.Context,
	code, redirectURI string,
	preferredTenant uuid.UUID,
	userAgent, ipAddr string,
) (*ExchangeResult, error) {
	profile, err := s.client.ExchangeCode(ctx, code, redirectURI)
	if err != nil {
		return nil, err
	}
	user, err := s.upsertUser(ctx, *profile)
	if err != nil {
		return nil, err
	}
	tenants, err := s.listMemberships(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if len(tenants) == 0 {
		return nil, errors.New("auth: user has no active tenant memberships")
	}
	chosen := tenants[0]
	if preferredTenant != uuid.Nil {
		for _, t := range tenants {
			if t.ID == preferredTenant {
				chosen = t
				break
			}
		}
	}
	roles := []string{chosen.Role}
	sess, err := s.sessions.Create(ctx, Session{
		TenantID:   chosen.ID,
		UserID:     user.ID,
		IssuedAt:   time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(24 * time.Hour),
		UserAgent:  userAgent,
		IPAddress:  ipAddr,
		RefreshJTI: uuid.NewString(),
	})
	if err != nil {
		return nil, err
	}
	base := Claims{
		UserID:    user.ID,
		TenantID:  chosen.ID,
		Roles:     roles,
		SessionID: sess.ID,
	}
	access, err := s.signer.Issue(base)
	if err != nil {
		return nil, err
	}
	refreshClaims := base
	refreshClaims.JWTID = sess.RefreshJTI
	refresh, err := s.signer.IssueRefresh(refreshClaims)
	if err != nil {
		return nil, err
	}
	return &ExchangeResult{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         *user,
		Tenants:      tenants,
		TenantID:     chosen.ID,
		SessionID:    sess.ID,
		ExpiresIn:    int64(s.signer.cfg.AccessTTL.Seconds()),
	}, nil
}

// Refresh mints a new access token from a presented refresh token.
// The refresh token's session must still be live — revocation is
// therefore the single lever an admin pulls to kick a user off.
func (s *SSOService) Refresh(ctx context.Context, refreshToken string) (*ExchangeResult, error) {
	claims, err := s.signer.Verify(refreshToken)
	if err != nil {
		return nil, err
	}
	if s.sessions != nil && claims.SessionID != uuid.Nil {
		if _, err := s.sessions.Get(ctx, claims.TenantID, claims.SessionID); err != nil {
			return nil, err
		}
	}
	base := Claims{
		UserID:    claims.UserID,
		TenantID:  claims.TenantID,
		Roles:     claims.Roles,
		SessionID: claims.SessionID,
	}
	access, err := s.signer.Issue(base)
	if err != nil {
		return nil, err
	}
	return &ExchangeResult{
		AccessToken:  access,
		RefreshToken: refreshToken,
		TenantID:     claims.TenantID,
		SessionID:    claims.SessionID,
		ExpiresIn:    int64(s.signer.cfg.AccessTTL.Seconds()),
	}, nil
}

func (s *SSOService) upsertUser(ctx context.Context, p KChatProfile) (*ResolvedUser, error) {
	if s.adminPool == nil {
		return nil, errors.New("auth: admin pool required for user upsert")
	}
	out := &ResolvedUser{
		KChatUserID: p.ID,
		Email:       p.Email,
		DisplayName: fallbackStr(p.DisplayName, p.Username),
	}
	err := s.adminPool.QueryRow(ctx,
		`INSERT INTO users (id, kchat_user_id, email, display_name)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (kchat_user_id) DO UPDATE
		   SET email = EXCLUDED.email,
		       display_name = EXCLUDED.display_name
		 RETURNING id`,
		uuid.New(), p.ID, p.Email, out.DisplayName,
	).Scan(&out.ID)
	if err != nil {
		return nil, fmt.Errorf("auth: upsert user: %w", err)
	}
	return out, nil
}

func (s *SSOService) listMemberships(ctx context.Context, userID uuid.UUID) ([]TenantRef, error) {
	pool := s.adminPool
	if pool == nil {
		pool = s.pool
	}
	rows, err := pool.Query(ctx,
		`SELECT t.id, t.slug, t.name, ut.role
		   FROM user_tenants ut
		   JOIN tenants t ON t.id = ut.tenant_id
		  WHERE ut.user_id = $1
		    AND ut.status = 'active'
		    AND t.status = 'active'
		  ORDER BY t.slug ASC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: list memberships: %w", err)
	}
	defer rows.Close()
	out := make([]TenantRef, 0)
	for rows.Next() {
		var tr TenantRef
		if err := rows.Scan(&tr.ID, &tr.Slug, &tr.Name, &tr.Role); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

// ForTenantTx lets callers expose the auth package's transaction
// helper without re-importing dbutil. Used by the /me endpoint so
// membership reads honour RLS when the admin pool is not configured.
func ForTenantTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	fn func(ctx context.Context, tx pgx.Tx) error,
) error {
	return dbutil.WithTenantTx(ctx, pool, tenantID, fn)
}

func fallbackStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
