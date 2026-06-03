package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ActionTypeBillingUsageSync is the scheduled_actions.action_type the
// billing usage-sync worker registers under. The wizard's
// AutoProvision path seeds one row per self-service tenant with a 24h
// cadence so metered usage is pushed to Stripe and trial-grace expiry
// is enforced daily.
//
// The constant lives in the tenant package (not internal/billing)
// because the wizard seeds the row and tenant must not import billing
// — billing imports tenant, and the dependency edge is kept one-way.
// internal/billing references this constant (see
// internal/billing/usage_sync.go) so a rename here fails the billing
// build.
const ActionTypeBillingUsageSync = "billing_usage_sync"

// defaultBillingUsageSyncIntervalSeconds is the cadence of the
// billing usage-sync scheduled action (24h), matching the daily
// snapshot job.
const defaultBillingUsageSyncIntervalSeconds = 86400

// RoleTenantOwner is the role granted to the user who signs a tenant
// up. It matches the "owner" role DefaultRoles() seeds (permissions
// ["*"]) so the owner has full control of their new tenant out of the
// box.
const RoleTenantOwner = "owner"

// defaultSignupCell is the cell a self-service tenant lands in when
// the SignupService is constructed without an explicit cell. The
// control-plane operator can rebalance cells later; signup just needs
// a non-empty value because tenants.cell is NOT NULL.
const defaultSignupCell = "default"

// SignupIdentity is the verified KChat identity a signup is bound to.
// It is the minimal projection of a KChat profile the signup flow
// needs to create the owning user; the API layer adapts
// auth.KChatClient into a SignupIdentityVerifier so the tenant
// package never imports auth (which would be an import cycle —
// internal/auth imports internal/tenant).
type SignupIdentity struct {
	KChatUserID string
	Email       string
	DisplayName string
}

// SignupIdentityVerifier exchanges a KChat OAuth code for the
// verified identity of the signing-up user. Implemented in the API
// layer over auth.KChatClient.
type SignupIdentityVerifier interface {
	VerifySignup(ctx context.Context, code, redirectURI string) (SignupIdentity, error)
}

// SignupInput is the validated payload of POST /api/v1/signup.
type SignupInput struct {
	// KChatCode + RedirectURI are exchanged for the owner's verified
	// identity. Signup fails closed if the exchange fails — we never
	// create a tenant for an unverified email.
	KChatCode   string
	RedirectURI string

	// CompanyName seeds the tenant name and the wizard's company
	// profile. Required.
	CompanyName string

	// Slug is the tenant's URL-safe identifier. When empty it is
	// derived from CompanyName.
	Slug string

	// Plan is the plan the tenant signs up on. Empty defaults to the
	// free plan. Paid plans are created but the actual plan switch is
	// gated on Stripe checkout by the billing layer (the signup flow
	// itself never blocks on payment).
	Plan string

	Country      string
	CurrencyCode string
}

// SignupResult is returned by SignupService.Signup.
type SignupResult struct {
	Tenant *Tenant       `json:"tenant"`
	User   *User         `json:"user"`
	Wizard *WizardResult `json:"wizard,omitempty"`
}

// SignupService implements self-service tenant creation: verify the
// KChat identity, create the tenant row, attach the owner, and run
// the wizard's AutoProvision flow (seed → ZK fabric → features →
// welcome DM). It deliberately does NOT touch Stripe — billing is a
// separate package the API handler invokes after signup for paid
// plans, which keeps the tenant package free of a billing import.
type SignupService struct {
	store    *PGStore
	users    *UserStore
	wizard   *Wizard
	verifier SignupIdentityVerifier
	cell     string
}

// NewSignupService wires the signup flow. cell may be empty, in which
// case defaultSignupCell is used.
func NewSignupService(store *PGStore, users *UserStore, wizard *Wizard, verifier SignupIdentityVerifier, cell string) *SignupService {
	if cell == "" {
		cell = defaultSignupCell
	}
	return &SignupService{
		store:    store,
		users:    users,
		wizard:   wizard,
		verifier: verifier,
		cell:     cell,
	}
}

// ErrInvalidSignup is returned when the signup payload is missing
// required fields. The handler maps it to a 400.
var ErrInvalidSignup = errors.New("tenant: invalid signup request")

// ErrIdentityVerification is returned when the caller's KChat identity
// could not be verified (bad/expired code, empty profile, or a KChat
// outage). The handler maps it to a 401 with a generic message so an
// unauthenticated caller never sees the upstream error detail.
var ErrIdentityVerification = errors.New("tenant: signup identity verification failed")

// Signup runs the full self-service flow and returns the created
// tenant + owner. The owning user is bound to the tenant with the
// tenant.owner role so a follow-up KChat SSO login resolves the
// membership.
//
// Ordering rationale: the tenant row is created first (status
// 'active'), then the owner membership, then AutoProvision seeds the
// tenant. AutoProvision failures are surfaced to the caller but do
// NOT delete the tenant — a half-provisioned tenant can be re-run
// through the wizard by an operator, which is preferable to losing
// the verified signup. The partial WizardResult is returned alongside
// the error so the handler can report what completed.
func (s *SignupService) Signup(ctx context.Context, in SignupInput) (*SignupResult, error) {
	if s.verifier == nil {
		return nil, errors.New("tenant: signup verifier not wired")
	}
	if strings.TrimSpace(in.CompanyName) == "" {
		return nil, fmt.Errorf("%w: company_name required", ErrInvalidSignup)
	}
	if in.KChatCode == "" {
		return nil, fmt.Errorf("%w: kchat code required", ErrInvalidSignup)
	}

	plan := in.Plan
	if plan == "" {
		plan = PlanFree
	}

	identity, err := s.verifier.VerifySignup(ctx, in.KChatCode, in.RedirectURI)
	if err != nil {
		// Wrap the upstream detail for server-side logs but tag it with
		// ErrIdentityVerification so the handler answers 401 generically.
		return nil, fmt.Errorf("%w: %w", ErrIdentityVerification, err)
	}
	if identity.KChatUserID == "" {
		return nil, fmt.Errorf("%w: identity missing kchat user id", ErrIdentityVerification)
	}

	slug := in.Slug
	if slug == "" {
		slug = Slugify(in.CompanyName)
	}
	if slug == "" {
		return nil, fmt.Errorf("%w: could not derive slug from company_name", ErrInvalidSignup)
	}

	quota, err := s.quotaForPlan(ctx, plan)
	if err != nil {
		return nil, err
	}

	t, err := s.store.Create(ctx, CreateInput{
		Slug:  slug,
		Name:  in.CompanyName,
		Cell:  s.cell,
		Plan:  plan,
		Quota: quota,
	})
	if err != nil {
		return nil, err
	}

	owner, err := s.ensureOwner(ctx, t.ID, identity)
	if err != nil {
		return nil, err
	}

	result := &SignupResult{Tenant: t, User: owner}

	if s.wizard != nil {
		wres, werr := s.wizard.AutoProvision(ctx, t.ID, AutoProvisionConfig{
			SetupWizardConfig: SetupWizardConfig{
				CompanyName:  in.CompanyName,
				Country:      in.Country,
				CurrencyCode: in.CurrencyCode,
				Plan:         plan,
				CreatedBy:    owner.ID,
			},
			OwnerKChatUserID: identity.KChatUserID,
			OwnerDisplayName: identity.DisplayName,
			TenantSlug:       t.Slug,
		})
		result.Wizard = wres
		if werr != nil {
			return result, werr
		}
	}

	return result, nil
}

// ensureOwner creates (or resolves, on a repeat signup with the same
// KChat identity) the owning user and binds it to the tenant with the
// tenant.owner role. A pre-existing user (GetUserByKChatID hit) is
// reused so a person who already has a Kapp identity from another
// tenant can spin up a second tenant without a duplicate users row.
func (s *SignupService) ensureOwner(ctx context.Context, tenantID uuid.UUID, identity SignupIdentity) (*User, error) {
	owner, err := s.users.CreateUser(ctx, User{
		KChatUserID: identity.KChatUserID,
		Email:       identity.Email,
		DisplayName: identity.DisplayName,
	})
	if err != nil {
		if errors.Is(err, ErrKChatUserIDTaken) {
			owner, err = s.users.GetUserByKChatID(ctx, identity.KChatUserID)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if err := s.users.AddUserToTenant(ctx, owner.ID, tenantID, RoleTenantOwner); err != nil &&
		!errors.Is(err, ErrMembershipExists) {
		return nil, err
	}
	return owner, nil
}

// quotaForPlan resolves the plan's limits into the quota JSON stored
// on the tenant row. An unknown plan falls back to an empty quota
// (the metering layer treats absent limits as unlimited) rather than
// failing the signup outright.
func (s *SignupService) quotaForPlan(ctx context.Context, plan string) (json.RawMessage, error) {
	plans := NewPlanStore(s.store.pool)
	p, err := plans.Get(ctx, plan)
	if err != nil {
		if errors.Is(err, ErrPlanNotFound) {
			return json.RawMessage("{}"), nil
		}
		return nil, err
	}
	raw, err := json.Marshal(p.Limits)
	if err != nil {
		return nil, fmt.Errorf("tenant: marshal plan limits: %w", err)
	}
	return raw, nil
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a company name into a URL-safe slug: lower-cased,
// non-alphanumeric runs collapsed to single hyphens, trimmed. Empty
// input (or input with no alphanumerics) yields "".
func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}
