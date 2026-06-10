package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// managementAPIPrefix is iam-core's Management API base path under the
// issuer. All provisioning calls (tenants, users, applications) hang
// off it.
const managementAPIPrefix = "/api/v1/management"

// IAMCoreClient is the umbrella facade kapp-fab uses to talk to
// iam-core. It bundles the three building blocks a deployment needs:
//
//   - OAuth2 (*OAuth2Client): interactive Authorization-Code login.
//   - M2M (*M2MClient): client_credentials tokens for the calls below.
//   - Validator (*JWKSValidator): token validation shared with the
//     API middleware.
//
// and adds the Management API methods (CreateTenant / CreateApplication
// / CreateUser) that tenant onboarding drives. Keeping all of this on
// one struct means deps_build wires a single optional dependency and
// every consumer asks it for exactly the sub-client it needs.
//
// All fields are safe for concurrent use.
type IAMCoreClient struct {
	issuer     string
	mgmtBase   string
	oauth2     *OAuth2Client
	m2m        *M2MClient
	validator  *JWKSValidator
	httpClient *http.Client
	logger     *slog.Logger
}

// IAMCoreClientConfig assembles an IAMCoreClient from already-built
// sub-clients. The OAuth2 client and M2M client are optional
// independently: a deployment that only validates tokens (no
// interactive login, no provisioning) can supply just the validator.
// Management methods return an error when M2M is absent; OAuth2
// methods are reached via OAuth2() which may be nil.
type IAMCoreClientConfig struct {
	Issuer     string
	OAuth2     *OAuth2Client
	M2M        *M2MClient
	Validator  *JWKSValidator
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// NewIAMCoreClient assembles the facade. A validator is required (it
// is the one piece every deployment with iam-core enabled needs); the
// rest are optional per the config doc.
func NewIAMCoreClient(cfg IAMCoreClientConfig) (*IAMCoreClient, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("auth: iam-core client requires an issuer")
	}
	if cfg.Validator == nil {
		return nil, errors.New("auth: iam-core client requires a jwks validator")
	}
	issuer := strings.TrimRight(cfg.Issuer, "/")
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &IAMCoreClient{
		issuer:     issuer,
		mgmtBase:   issuer + managementAPIPrefix,
		oauth2:     cfg.OAuth2,
		m2m:        cfg.M2M,
		validator:  cfg.Validator,
		httpClient: httpClient,
		logger:     logger,
	}, nil
}

// Issuer returns the iam-core base URL.
func (c *IAMCoreClient) Issuer() string { return c.issuer }

// OAuth2 returns the interactive login client (may be nil).
func (c *IAMCoreClient) OAuth2() *OAuth2Client { return c.oauth2 }

// Validator returns the shared JWKS validator.
func (c *IAMCoreClient) Validator() *JWKSValidator { return c.validator }

// --- Management API request/response shapes --------------------------
//
// These mirror iam-core's Management API JSON contract
// (api/management/v1). We model only the fields kapp-fab sends or
// reads; iam-core may return more and we ignore the rest so its API
// can evolve without breaking us.

// IAMTenantSpec is the request body for POST /management/tenants.
type IAMTenantSpec struct {
	Name string `json:"name"`
	// Domain is optional; iam-core uses it for tenant resolution by
	// custom domain.
	Domain string `json:"domain,omitempty"`
	// Metadata carries the kapp_tenant_id mapping (see
	// CreateTenant) plus any operator-supplied tags. iam-core
	// surfaces metadata to its claim-mapping rules.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// IAMTenant is the subset of iam-core's Tenant we consume.
type IAMTenant struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// IAMApplicationSpec is the request body for POST /management/applications.
type IAMApplicationSpec struct {
	Name           string   `json:"name"`
	AppType        string   `json:"app_type,omitempty"`
	RedirectURIs   []string `json:"redirect_uris,omitempty"`
	GrantTypes     []string `json:"grant_types,omitempty"`
	Scopes         []string `json:"scopes,omitempty"`
	RequireConsent bool     `json:"require_consent"`
}

// IAMApplication is the subset of iam-core's Application we consume —
// notably the freshly-minted client credentials.
type IAMApplication struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Name         string `json:"name"`
}

// IAMUserSpec is the request body for POST /management/users.
type IAMUserSpec struct {
	Email         string `json:"email"`
	Name          string `json:"name,omitempty"`
	GivenName     string `json:"given_name,omitempty"`
	FamilyName    string `json:"family_name,omitempty"`
	EmailVerified bool   `json:"email_verified,omitempty"`
	// Password is optional — omit it for passwordless / social
	// users. When omitted iam-core provisions a user with no local
	// password credential.
	Password string `json:"password,omitempty"`
	// Metadata carries kapp_user_id so iam-core can stamp it into
	// the user's tokens via a claim-mapping rule.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// IAMUser is the subset of iam-core's User we consume.
type IAMUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// CreateTenant provisions a tenant in iam-core. The Kapp tenant UUID
// is injected into Metadata as `kapp_tenant_id` so iam-core's
// claim-mapping rule can stamp it into every access token minted for
// users of this tenant — that claim is what the JWKS validator reads
// back as Claims.TenantID, closing the loop without a per-request
// Management API lookup.
func (c *IAMCoreClient) CreateTenant(ctx context.Context, kappTenantID string, spec IAMTenantSpec) (*IAMTenant, error) {
	if spec.Metadata == nil {
		spec.Metadata = map[string]any{}
	}
	spec.Metadata["kapp_tenant_id"] = kappTenantID
	var out IAMTenant
	if err := c.do(ctx, http.MethodPost, c.mgmtBase+"/tenants", "", spec, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("auth: iam-core create tenant returned no id")
	}
	return &out, nil
}

// CreateApplication registers an OAuth2 application for the Kapp web
// frontend under the given iam-core tenant. The returned credentials
// let the operator wire IAM_CORE_CLIENT_ID / _SECRET (or, in a fully
// automated rollout, persist them per tenant).
func (c *IAMCoreClient) CreateApplication(ctx context.Context, iamTenantID string, spec IAMApplicationSpec) (*IAMApplication, error) {
	if iamTenantID == "" {
		return nil, errors.New("auth: iam tenant id required to create application")
	}
	if spec.AppType == "" {
		spec.AppType = "regular_web"
	}
	var out IAMApplication
	if err := c.do(ctx, http.MethodPost, c.mgmtBase+"/applications", iamTenantID, spec, &out); err != nil {
		return nil, err
	}
	if out.ClientID == "" {
		return nil, errors.New("auth: iam-core create application returned no client_id")
	}
	return &out, nil
}

// CreateUser provisions a user in iam-core under the given tenant,
// stamping the Kapp user UUID into Metadata as `kapp_user_id` (the
// mirror of CreateTenant's kapp_tenant_id). Used by user import /
// sync so iam-core-minted tokens carry the Kapp identity.
func (c *IAMCoreClient) CreateUser(ctx context.Context, iamTenantID, kappUserID string, spec IAMUserSpec) (*IAMUser, error) {
	if iamTenantID == "" {
		return nil, errors.New("auth: iam tenant id required to create user")
	}
	if spec.Metadata == nil {
		spec.Metadata = map[string]any{}
	}
	if kappUserID != "" {
		spec.Metadata["kapp_user_id"] = kappUserID
	}
	var out IAMUser
	if err := c.do(ctx, http.MethodPost, c.mgmtBase+"/users", iamTenantID, spec, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("auth: iam-core create user returned no id")
	}
	return &out, nil
}

// do performs an authenticated Management API request. It attaches a
// fresh M2M bearer token and, when tenantID is non-empty, the
// X-Tenant-ID header iam-core uses to scope the operation. On a 401 it
// invalidates the cached M2M token and retries exactly once, covering
// the window where iam-core rotated its signing key out from under a
// still-cached token.
func (c *IAMCoreClient) do(ctx context.Context, method, url, tenantID string, body, out any) error {
	if c.m2m == nil {
		return errors.New("auth: iam-core management calls require an m2m client")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("auth: marshal management request: %w", err)
	}
	attempt := func() (int, []byte, error) {
		token, terr := c.m2m.Token(ctx)
		if terr != nil {
			return 0, nil, fmt.Errorf("auth: obtain m2m token: %w", terr)
		}
		req, rerr := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
		if rerr != nil {
			return 0, nil, fmt.Errorf("auth: build management request: %w", rerr)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if tenantID != "" {
			req.Header.Set("X-Tenant-ID", tenantID)
		}
		resp, derr := c.httpClient.Do(req)
		if derr != nil {
			return 0, nil, fmt.Errorf("auth: management request: %w", derr)
		}
		defer func() { _ = resp.Body.Close() }()
		respBody, berr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if berr != nil {
			return resp.StatusCode, nil, fmt.Errorf("auth: read management response: %w", berr)
		}
		return resp.StatusCode, respBody, nil
	}

	status, respBody, err := attempt()
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		c.m2m.Invalidate()
		status, respBody, err = attempt()
		if err != nil {
			return err
		}
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("auth: management %s %s: status %d: %s",
			method, url, status, truncateForErr(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("auth: decode management response: %w", err)
		}
	}
	return nil
}

// truncateForErr bounds an error body so a large/HTML error page does
// not blow up logs. 512 bytes is enough to capture iam-core's JSON
// error envelope.
func truncateForErr(b []byte) string {
	const maxLen = 512
	s := strings.TrimSpace(string(b))
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
