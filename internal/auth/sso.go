package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
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
	BaseURL string
	APIKey  string
	client  *http.Client
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
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
	ID              uuid.UUID `json:"id"`
	KChatUserID     string    `json:"kchat_user_id"`
	Email           string    `json:"email"`
	DisplayName     string    `json:"display_name"`
	IsPlatformAdmin bool      `json:"is_platform_admin,omitempty"`
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
	client    KChatClient
	signer    *Signer
	sessions  SessionStore
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
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
		ExpiresAt:  time.Now().UTC().Add(s.signer.cfg.RefreshTTL),
		UserAgent:  userAgent,
		IPAddress:  ipAddr,
		RefreshJTI: uuid.NewString(),
	})
	if err != nil {
		return nil, err
	}
	base := Claims{
		UserID:          user.ID,
		TenantID:        chosen.ID,
		Roles:           roles,
		SessionID:       sess.ID,
		IsPlatformAdmin: user.IsPlatformAdmin,
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
// the operator's primary lever to kick a user off — and the
// IsPlatformAdmin claim is re-derived from users.is_platform_admin on
// every refresh.
//
// We re-query the admin flag (rather than passing it through from the
// refresh-token claim) because the security surface of platform admin
// is qualitatively larger than per-tenant roles: it grants
// suspend/archive/delete on every tenant. If an operator demotes a
// compromised admin via `UPDATE users SET is_platform_admin = FALSE`,
// the next refresh — typically within the access-token TTL (15 min)
// — must drop the privilege without requiring manual session
// revocation. Tenant `Roles` are NOT re-queried here because doing so
// would require resolving the chosen tenant + role row on every
// refresh; that lookup is acceptable for SSO but not for refresh's
// hot path. Operators wanting to revoke per-tenant grants in real time
// must continue to revoke the session.
//
// When the admin pool is not configured (local dev), the refresh
// claim is honoured as-is and a debug-level fall-through path keeps
// the previous behaviour. Production deployments MUST configure
// ADMIN_DB_URL.
//
// Availability vs. freshness: when the admin pool IS configured but
// the lookup itself fails (timeout, connection reset, pgbouncer
// failover), Refresh fails OPEN to the refresh-token's claimed value
// rather than rejecting every refresh for every user during the
// transient outage. The reasoning is that admin-pool unavailability
// is a control-plane infrastructure event whose blast radius would
// otherwise extend to every user-facing API call (since access
// tokens are short-lived and clients refresh constantly). The
// staleness window introduced by fail-open is bounded by the
// AccessTTL of the resulting access token — the same window the
// docs already acknowledge for the demote-then-wait-15-min path. A
// WARN log on the fail-open path keeps the event visible to
// observability so operators can correlate against admin-pool
// health.
func (s *SSOService) Refresh(ctx context.Context, refreshToken string) (*ExchangeResult, error) {
	claims, err := s.signer.VerifyRefresh(refreshToken)
	if err != nil {
		return nil, err
	}
	if s.sessions != nil && claims.SessionID != uuid.Nil {
		if _, err := s.sessions.Get(ctx, claims.TenantID, claims.SessionID); err != nil {
			return nil, err
		}
	}
	// Default to whatever the refresh token claimed; the DB lookup
	// below overrides this for production deployments. The fallback
	// path matters for local dev (no admin pool), for the brief
	// window during which migration 000051 has not been applied,
	// AND for transient admin-pool unavailability — see the function
	// doc above for the fail-open rationale.
	isPlatformAdmin := claims.IsPlatformAdmin
	if s.adminPool != nil && claims.UserID != uuid.Nil {
		current, err := s.lookupPlatformAdmin(ctx, claims.UserID)
		if err != nil {
			// Fail open to the cached claim value rather than
			// rejecting the refresh. See the function doc for
			// the availability vs. freshness tradeoff.
			log.Printf("auth: refresh platform-admin lookup failed for user=%s; honouring refresh-token claim (%t): %v", claims.UserID, isPlatformAdmin, err)
		} else {
			isPlatformAdmin = current
		}
	}
	base := Claims{
		UserID:          claims.UserID,
		TenantID:        claims.TenantID,
		Roles:           claims.Roles,
		SessionID:       claims.SessionID,
		IsPlatformAdmin: isPlatformAdmin,
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

// lookupPlatformAdmin returns the current value of
// users.is_platform_admin for the supplied user id. Returns false when
// the row is missing (e.g. user was deleted between SSO and refresh);
// in that case the caller will mint a non-admin access token and the
// next request will fail under whatever middleware enforces the
// session.
func (s *SSOService) lookupPlatformAdmin(ctx context.Context, userID uuid.UUID) (bool, error) {
	var isAdmin bool
	err := s.adminPool.QueryRow(ctx,
		`SELECT is_platform_admin FROM users WHERE id = $1`,
		userID,
	).Scan(&isAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isAdmin, nil
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
	// RETURNING is_platform_admin so the SSO flow can stamp the
	// claim on the issued JWT without a second round trip. The
	// column lands in migration 000051; deployments that have not
	// applied it yet must do so before the API will start.
	//
	// `xmax = 0` is Postgres's way of saying "this row was just
	// INSERTed, not updated by the ON CONFLICT DO UPDATE branch."
	// Combined with the kchat_user_id–keyed bootstrap check below,
	// the discriminator distinguishes two real flows:
	//
	//   wasInsert == true  → fresh first login of a listed admin.
	//                        Expected install path: KAPP_PLATFORM_
	//                        ADMIN_USERS held the user's KChat ID
	//                        before they ever logged into Kapp, the
	//                        upsert just created their row, and we
	//                        promote it in the same transaction
	//                        scope. Logged at INFO as a positive
	//                        signal that the bootstrap worked.
	//   wasInsert == false → existing row had is_platform_admin =
	//                        FALSE and the env var still lists
	//                        this user. Either (a) the operator
	//                        demoted them via the DB / promote
	//                        endpoint but did not retire the env
	//                        var, or (b) the env var was added
	//                        AFTER the user's first login (an
	//                        operator who set the var to a Kapp
	//                        UUID under the old design will have
	//                        seen this; that legacy mode is no
	//                        longer supported — see
	//                        bootstrapAdmin's doc comment). The
	//                        promotion still happens to honour the
	//                        operator's stated intent, but we log
	//                        WARN with a concrete remediation hint.
	//
	// The discriminator only matters when the env var matches.
	// Both branches share the same UPDATE so the persisted state
	// converges regardless of which path the install took.
	var wasInsert bool
	err := s.adminPool.QueryRow(ctx,
		`INSERT INTO users (id, kchat_user_id, email, display_name)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (kchat_user_id) DO UPDATE
		   SET email = EXCLUDED.email,
		       display_name = EXCLUDED.display_name
		 RETURNING id, is_platform_admin, (xmax = 0)`,
		uuid.New(), p.ID, p.Email, out.DisplayName,
	).Scan(&out.ID, &out.IsPlatformAdmin, &wasInsert)
	if err != nil {
		return nil, fmt.Errorf("auth: upsert user: %w", err)
	}
	// Bootstrap check is keyed on p.ID — the KChat-provider-stable
	// user identifier the operator already knows from KChat itself
	// BEFORE the user has ever logged into Kapp. Keying on out.ID
	// (the Kapp UUID) would not work: the UUID is generated by
	// uuid.New() at INSERT time, so on a fresh install the operator
	// has no way to enumerate it in advance — the bootstrap would
	// only ever fire on the *second* SSO login, after the operator
	// queried the DB for the new row's UUID and appended it to the
	// env var. The KChat-ID keyed design lets the very first login
	// of a listed admin take the wasInsert=true branch, which is
	// the expected install path.
	if !out.IsPlatformAdmin && s.bootstrapAdmin(p.ID) {
		// Persist the flag so future sessions read it from the
		// column even after the env var is retired (which is the
		// recommended steady-state once at least one row is TRUE).
		if _, err := s.adminPool.Exec(ctx,
			`UPDATE users SET is_platform_admin = TRUE WHERE id = $1`,
			out.ID,
		); err != nil {
			return nil, fmt.Errorf("auth: bootstrap platform admin: %w", err)
		}
		out.IsPlatformAdmin = true
		if wasInsert {
			log.Printf("auth: INFO platform admin bootstrap promoted new user=%s (kchat=%s) from KAPP_PLATFORM_ADMIN_USERS", out.ID, p.ID)
		} else {
			log.Printf("auth: WARN platform admin bootstrap re-promoted existing user=%s (kchat=%s) from KAPP_PLATFORM_ADMIN_USERS — if this was a deliberate demote, remove the kchat_user_id from the env var so it does not re-promote on next SSO login", out.ID, p.ID)
		}
	}
	return out, nil
}

// bootstrapAdmin reports whether the supplied KChat user id is
// enumerated in the KAPP_PLATFORM_ADMIN_USERS env var. Operators set
// this on a fresh install so the very first SSO login of a listed
// user auto-promotes them to platform admin; subsequent installs
// leave it unset and rely on the persisted users.is_platform_admin
// column.
//
// IMPORTANT: entries are KChat user identifiers (the `id` field of
// the KChat profile — opaque, provider-issued strings), NOT Kapp
// internal UUIDs. The KChat ID is the only stable identifier that
// the operator can know BEFORE the user has ever authenticated to
// Kapp; the Kapp internal UUID is generated by uuid.New() at the
// INSERT site of upsertUser, so keying on it would force a two-step
// bootstrap (login → look up the new UUID → set the env var →
// login again) and would make the wasInsert=true branch in
// upsertUser unreachable in practice. Comparison is whitespace-
// trimmed and case-sensitive; empty entries are silently ignored so
// a stray trailing comma cannot block an otherwise valid login.
//
// Operators upgrading from the pre-Phase-1 design (where this var
// held Kapp UUIDs) MUST switch to KChat IDs — the legacy mode is
// not supported. The migration docstring at
// migrations/000051_users_platform_admin.sql calls this out
// explicitly so anyone reading the SQL has the same picture as
// anyone reading the Go side.
func (s *SSOService) bootstrapAdmin(kchatID string) bool {
	if kchatID == "" {
		return false
	}
	raw := os.Getenv("KAPP_PLATFORM_ADMIN_USERS")
	if raw == "" {
		return false
	}
	for _, field := range strings.Split(raw, ",") {
		candidate := strings.TrimSpace(field)
		if candidate == "" {
			continue
		}
		if candidate == kchatID {
			return true
		}
	}
	return false
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
