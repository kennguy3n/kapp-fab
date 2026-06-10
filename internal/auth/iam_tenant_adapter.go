package auth

import (
	"context"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TenantProvisionerConfig tunes how the adapter mirrors Kapp tenants
// and users into iam-core.
type TenantProvisionerConfig struct {
	// ProvisionPerTenantApp controls whether EnsureTenant registers a
	// dedicated OAuth2 application per tenant. It defaults to false:
	// the zero-ops, 5000-tenant model uses a single operator-configured
	// confidential client (IAM_CORE_CLIENT_ID/_SECRET) shared across all
	// tenants, with per-tenant isolation provided by the kapp_tenant_id
	// claim mapping rather than per-tenant client registrations. The
	// 000082 migration intentionally stores no per-tenant client secret,
	// so leaving this false is the supported production posture. Set it
	// true only for a deployment that wants a distinct app registration
	// per tenant (e.g. for per-tenant Universal Login branding); the
	// adapter then returns the app's client_id for audit but still does
	// not persist its secret.
	ProvisionPerTenantApp bool
	// AppRedirectURIs are the redirect URIs registered on the per-tenant
	// application when ProvisionPerTenantApp is true. Ignored otherwise.
	AppRedirectURIs []string
	// AppScopes overrides the default scopes on the per-tenant
	// application. Ignored when ProvisionPerTenantApp is false.
	AppScopes []string
}

// tenantProvisioner adapts *IAMCoreClient to tenant.IAMProvisioner.
// It lives in internal/auth (which already imports internal/tenant via
// the middleware's TenantResolver) so that internal/tenant need not
// import internal/auth — keeping the dependency edge one-directional
// and the import graph acyclic.
type tenantProvisioner struct {
	client *IAMCoreClient
	cfg    TenantProvisionerConfig
}

// TenantProvisioner returns an adapter that satisfies
// tenant.IAMProvisioner, letting tenant onboarding mirror tenants and
// users into iam-core through this client. Returns nil when the client
// cannot provision (no M2M client configured) so callers can cleanly
// decide not to wire the integration's provisioning half.
func (c *IAMCoreClient) TenantProvisioner(cfg TenantProvisionerConfig) tenant.IAMProvisioner {
	if c == nil || c.m2m == nil {
		return nil
	}
	return &tenantProvisioner{client: c, cfg: cfg}
}

// ProvisionTenant mirrors a Kapp tenant into iam-core. The Kapp slug
// is carried in Metadata (as kapp_slug) for operator-side correlation
// — iam-core assigns its own slug, so this preserves the Kapp-side
// identifier without colliding with it. The kapp_tenant_id UUID
// (injected by CreateTenant) remains the authoritative mapping.
func (p *tenantProvisioner) ProvisionTenant(ctx context.Context, kappTenantID uuid.UUID, name, slug string) (string, error) {
	t, err := p.client.CreateTenant(ctx, kappTenantID.String(), IAMTenantSpec{
		Name:     name,
		Domain:   "",
		Metadata: map[string]any{"kapp_slug": slug},
	})
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// ProvisionWebApplication registers a per-tenant OAuth2 application in
// iam-core when ProvisionPerTenantApp is enabled, returning its
// (non-secret) client_id. In the default shared-client model it is a
// no-op returning an empty id.
func (p *tenantProvisioner) ProvisionWebApplication(ctx context.Context, iamTenantID, tenantName string) (string, error) {
	// Shared-client model: opt out of per-tenant app registration. See
	// TenantProvisionerConfig.ProvisionPerTenantApp.
	if !p.cfg.ProvisionPerTenantApp {
		return "", nil
	}
	app, err := p.client.CreateApplication(ctx, iamTenantID, IAMApplicationSpec{
		Name:         tenantName + " (Kapp Web)",
		AppType:      "regular_web",
		RedirectURIs: p.cfg.AppRedirectURIs,
		GrantTypes:   []string{"authorization_code", "refresh_token"},
		Scopes:       p.cfg.AppScopes,
	})
	if err != nil {
		return "", err
	}
	// Return only the non-secret client_id. The minted client_secret is
	// intentionally not surfaced or persisted (no per-tenant secret
	// storage exists in the schema); a per-tenant-app deployment that
	// needs the secret must capture it from iam-core's admin surface.
	return app.ClientID, nil
}

// ProvisionUser mirrors a Kapp user into the given iam-core tenant,
// returning the iam-core user id. The kapp_user_id is injected into
// the user's metadata by CreateUser for claim mapping.
func (p *tenantProvisioner) ProvisionUser(ctx context.Context, iamTenantID string, kappUserID uuid.UUID, email, displayName string) (string, error) {
	u, err := p.client.CreateUser(ctx, iamTenantID, kappUserID.String(), IAMUserSpec{
		Email: email,
		Name:  displayName,
	})
	if err != nil {
		return "", err
	}
	return u.ID, nil
}
