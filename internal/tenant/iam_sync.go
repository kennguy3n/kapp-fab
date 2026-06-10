package tenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// IAMProvisioner is the narrow contract tenant onboarding needs to
// mirror a Kapp tenant (and its users) into iam-core. It is defined
// here — rather than in internal/auth where the concrete
// *auth.IAMCoreClient lives — so that internal/tenant does NOT import
// internal/auth. The dependency runs the other way (auth → tenant via
// the middleware's TenantResolver), and adding a tenant → auth edge
// would close an import cycle. The concrete adapter that satisfies
// this interface therefore lives in internal/auth
// (auth.TenantProvisioner wrapping *auth.IAMCoreClient) and is wired
// in at boot.
//
// Every method takes the iam-core tenant id (not the Kapp UUID) for
// the calls that operate under a tenant, because iam-core scopes
// Management API writes by its own tenant identifier (the X-Tenant-ID
// header). The Kapp UUID is passed only where it must be stamped into
// iam-core metadata so it round-trips back as a token claim
// (kapp_tenant_id / kapp_user_id).
type IAMProvisioner interface {
	// ProvisionTenant mirrors a Kapp tenant into iam-core and returns
	// the iam-core tenant id. The Kapp tenant UUID is stamped into
	// iam-core metadata as kapp_tenant_id so every token minted for
	// the tenant carries it (the JWKS validator reads it back as
	// Claims.TenantID). Implementations should be idempotent where
	// the upstream API allows it; the caller additionally guards with
	// Tenant.HasIAMTenant so a re-run never re-provisions.
	ProvisionTenant(ctx context.Context, kappTenantID uuid.UUID, name, slug string) (iamTenantID string, err error)

	// ProvisionWebApplication registers (or confirms) the OAuth2
	// application for the Kapp web frontend under the given iam-core
	// tenant and returns its non-secret client_id for operator audit.
	//
	// In the default zero-ops, 5000-tenant model the platform uses a
	// single operator-configured confidential client (IAM_CORE_CLIENT_ID
	// / _SECRET) shared across every tenant — iam-core isolates tenants
	// via the kapp_tenant_id claim mapping, not via per-tenant client
	// registrations, and the 000082 migration deliberately adds no
	// per-tenant client columns to store a per-tenant secret. Such an
	// implementation returns ("", nil) to opt out. An implementation
	// that DOES register a per-tenant app returns the client_id only;
	// the secret is never surfaced here because there is nowhere to
	// persist it securely under the current schema.
	ProvisionWebApplication(ctx context.Context, iamTenantID, tenantName string) (clientID string, err error)

	// ProvisionUser mirrors a Kapp user into iam-core under the tenant
	// and returns the iam-core user id, stamping the Kapp user UUID as
	// kapp_user_id metadata (mirror of kapp_tenant_id).
	ProvisionUser(ctx context.Context, iamTenantID string, kappUserID uuid.UUID, email, displayName string) (iamUserID string, err error)
}

// iamTenantStore is the slice of *PGStore the sync needs. Narrowed to
// an interface so the orchestrator is unit-testable with a fake store
// and so it documents exactly which store methods onboarding touches.
type iamTenantStore interface {
	Get(ctx context.Context, id uuid.UUID) (*Tenant, error)
	SetIAMTenantID(ctx context.Context, id uuid.UUID, iamTenantID string) error
}

// iamUserStore is the slice of *UserStore the sync needs.
type iamUserStore interface {
	GetUser(ctx context.Context, id uuid.UUID) (*User, error)
	SetIAMUserID(ctx context.Context, id uuid.UUID, iamUserID string) error
}

// IAMSync orchestrates mirroring Kapp tenants and users into iam-core.
// It is the single place the "provision then persist the mapping"
// two-step lives so the wizard, the user-import path, and any future
// caller share identical idempotency and error semantics.
//
// A nil *IAMSync is never wired when the integration is disabled; the
// wizard checks `w.iamSync != nil` before calling, so the methods here
// always run with a non-nil provisioner.
type IAMSync struct {
	prov    IAMProvisioner
	tenants iamTenantStore
	users   iamUserStore
	logger  *slog.Logger
}

// NewIAMSync builds the orchestrator. The provisioner and tenant store
// are required; the user store may be nil for a deployment that
// provisions tenants but not users (SyncUser then returns an error if
// called). The logger defaults to slog.Default when nil.
func NewIAMSync(prov IAMProvisioner, tenants iamTenantStore, users iamUserStore, logger *slog.Logger) (*IAMSync, error) {
	if prov == nil {
		return nil, errors.New("tenant: iam sync requires a provisioner")
	}
	if tenants == nil {
		return nil, errors.New("tenant: iam sync requires a tenant store")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &IAMSync{prov: prov, tenants: tenants, users: users, logger: logger}, nil
}

// EnsureTenant mirrors the tenant into iam-core and returns the
// iam-core tenant id. It is idempotent:
//
//   - If the tenant already carries an iam_tenant_id mapping, that id
//     is returned without any upstream call.
//   - Otherwise it provisions the tenant in iam-core, persists the
//     mapping, then (best-effort) provisions the web application.
//
// The persist step uses SetIAMTenantID, which refuses to repoint an
// already-mapped tenant. If two onboarding runs race, the loser sees
// ErrIAMTenantAlreadyMapped, re-reads the row, and returns the winner's
// id — so callers always converge on a single iam-core tenant.
func (s *IAMSync) EnsureTenant(ctx context.Context, t *Tenant) (string, error) {
	if t == nil {
		return "", errors.New("tenant: iam sync ensure tenant: nil tenant")
	}
	if t.HasIAMTenant() {
		return t.IAMTenantID, nil
	}

	iamTenantID, err := s.prov.ProvisionTenant(ctx, t.ID, t.Name, t.Slug)
	if err != nil {
		return "", fmt.Errorf("tenant: iam sync provision tenant: %w", err)
	}
	if iamTenantID == "" {
		return "", errors.New("tenant: iam sync: provisioner returned empty iam tenant id")
	}

	if err := s.tenants.SetIAMTenantID(ctx, t.ID, iamTenantID); err != nil {
		if errors.Is(err, ErrIAMTenantAlreadyMapped) {
			// A concurrent run won the race and mapped a different
			// iam-core tenant. Treat its mapping as authoritative —
			// the tenant iam-core just created for us is an orphan
			// (no kapp row points at it) that an operator/janitor can
			// reap; re-pointing here would orphan the winner's tokens.
			existing, getErr := s.tenants.Get(ctx, t.ID)
			if getErr != nil {
				return "", fmt.Errorf("tenant: iam sync reload after mapping race: %w", getErr)
			}
			s.logger.WarnContext(ctx, "iam-core tenant provisioning raced; keeping existing mapping",
				"kapp_tenant_id", t.ID.String(),
				"kept_iam_tenant_id", existing.IAMTenantID,
				"orphaned_iam_tenant_id", iamTenantID,
			)
			return existing.IAMTenantID, nil
		}
		return "", fmt.Errorf("tenant: iam sync persist mapping: %w", err)
	}

	// Web application registration is best-effort and non-fatal: the
	// shared-client model (see ProvisionWebApplication doc) does not
	// depend on it, and a per-tenant-app deployment can re-run
	// onboarding to retry. We log the outcome for operator audit but
	// never fail tenant onboarding on it.
	if clientID, err := s.prov.ProvisionWebApplication(ctx, iamTenantID, t.Name); err != nil {
		s.logger.WarnContext(ctx, "iam-core web application provisioning failed (non-fatal)",
			"kapp_tenant_id", t.ID.String(),
			"iam_tenant_id", iamTenantID,
			"error", err,
		)
	} else if clientID != "" {
		s.logger.InfoContext(ctx, "iam-core web application provisioned",
			"kapp_tenant_id", t.ID.String(),
			"iam_tenant_id", iamTenantID,
			"client_id", clientID,
		)
	}

	return iamTenantID, nil
}

// SyncUser mirrors a single Kapp user into iam-core under the given
// iam-core tenant and persists the resulting iam_user_id. It is
// idempotent: SetIAMUserID only writes a NULL column, so a re-sync of
// an already-mapped user is a benign no-op that returns the existing
// id. A non-empty iamTenantID is required — callers obtain it from
// EnsureTenant.
func (s *IAMSync) SyncUser(ctx context.Context, iamTenantID string, userID uuid.UUID, email, displayName string) (string, error) {
	if s.users == nil {
		return "", errors.New("tenant: iam sync: user store not configured")
	}
	if iamTenantID == "" {
		return "", errors.New("tenant: iam sync sync user: iam tenant id required")
	}
	if userID == uuid.Nil {
		return "", errors.New("tenant: iam sync sync user: user id required")
	}

	iamUserID, err := s.prov.ProvisionUser(ctx, iamTenantID, userID, email, displayName)
	if err != nil {
		return "", fmt.Errorf("tenant: iam sync provision user: %w", err)
	}
	if iamUserID == "" {
		return "", errors.New("tenant: iam sync: provisioner returned empty iam user id")
	}

	if err := s.users.SetIAMUserID(ctx, userID, iamUserID); err != nil {
		if errors.Is(err, ErrIAMUserAlreadyMapped) {
			// A concurrent run won the race and mapped a different
			// iam-core user. Mirror EnsureTenant's handling: treat the
			// winner's mapping as authoritative and return it so the
			// caller (wizard) does not abort onboarding. The user
			// iam-core just created for us is an orphan an operator can
			// reap; re-pointing would orphan the winner's identity.
			existing, getErr := s.users.GetUser(ctx, userID)
			if getErr != nil {
				return "", fmt.Errorf("tenant: iam sync reload after user mapping race: %w", getErr)
			}
			s.logger.WarnContext(ctx, "iam-core user provisioning raced; keeping existing mapping",
				"kapp_user_id", userID.String(),
				"kept_iam_user_id", existing.IAMUserID,
				"orphaned_iam_user_id", iamUserID,
			)
			return existing.IAMUserID, nil
		}
		return "", fmt.Errorf("tenant: iam sync persist user mapping: %w", err)
	}

	return iamUserID, nil
}
