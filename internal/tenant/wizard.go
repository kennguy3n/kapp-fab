package tenant

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetupWizardConfig is the payload a tenant owner submits to seed their
// newly-created tenant. It covers the first-run choices ERPNext surfaces
// in its own Setup Wizard — company profile, country/industry, the
// chart-of-accounts template, and the initial role roster.
type SetupWizardConfig struct {
	CompanyName      string           `json:"company_name"`
	Industry         string           `json:"industry,omitempty"`
	Country          string           `json:"country,omitempty"`
	CurrencyCode     string           `json:"currency_code,omitempty"`
	CoATemplate      string           `json:"coa_template,omitempty"`
	Roles            []WizardRole     `json:"roles,omitempty"`
	Users            []WizardUser     `json:"users,omitempty"`
	SampleData       bool             `json:"sample_data,omitempty"`
	Plan             string           `json:"plan,omitempty"`
	CreatedBy        uuid.UUID        `json:"created_by,omitempty"`
}

// WizardRole captures a role definition the wizard should upsert into
// the tenant's `roles` table. Permissions are a JSON array of action
// strings or action+resource objects; we pass them through verbatim.
type WizardRole struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Permissions json.RawMessage `json:"permissions"`
}

// WizardUser is the minimum identifier + role needed to seed an initial
// membership row in `user_tenants`. If the user does not exist in
// `users` yet, the wizard will create a stub by email.
type WizardUser struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
}

// WizardResult summarises the side-effects the wizard applied. The HTTP
// handler surfaces this so the UI can render a completion screen.
type WizardResult struct {
	TenantID         uuid.UUID `json:"tenant_id"`
	AccountsInserted int       `json:"accounts_inserted"`
	RolesInserted    int       `json:"roles_inserted"`
	UsersInserted    int       `json:"users_inserted"`
	CoATemplateUsed  string    `json:"coa_template_used"`
}

// ---------------------------------------------------------------------------
// Embedded chart-of-accounts templates.
// ---------------------------------------------------------------------------

//go:embed coa_templates/us_gaap_basic.json
var coaUSGAAPBasic []byte

//go:embed coa_templates/ifrs_basic.json
var coaIFRSBasic []byte

// chartOfAccountsTemplates maps the wizard's template name to the
// embedded JSON payload. Adding a new template is a matter of dropping
// a JSON file in coa_templates/ and registering it here.
var chartOfAccountsTemplates = map[string][]byte{
	"us_gaap_basic": coaUSGAAPBasic,
	"ifrs_basic":    coaIFRSBasic,
}

// templateAccount is the shape each entry in a CoA template takes. The
// chart schema mirrors the accounts table columns in
// migrations/000001_initial_schema.sql.
type templateAccount struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ParentCode string `json:"parent_code,omitempty"`
	Active     *bool  `json:"active,omitempty"`
}

// DefaultRoles is the canonical role set the wizard seeds when the
// caller does not supply their own roles list. The permission arrays
// mirror the "role packs" discussed in ARCHITECTURE.md §6.
func DefaultRoles() []WizardRole {
	return []WizardRole{
		{Name: "owner", Description: "Tenant owner", Permissions: json.RawMessage(`["*"]`)},
		{Name: "tenant.admin", Description: "Tenant administrator", Permissions: json.RawMessage(`["tenant.admin","tenant.member","krecord.*"]`)},
		{Name: "tenant.member", Description: "Standard member", Permissions: json.RawMessage(`["tenant.member","krecord.read"]`)},
		{Name: "finance.admin", Description: "Finance administrator", Permissions: json.RawMessage(`["tenant.member","finance.*","krecord.*"]`)},
		{Name: "hr.admin", Description: "HR administrator", Permissions: json.RawMessage(`["tenant.member","hr.*","krecord.*"]`)},
		{Name: "lms.admin", Description: "LMS administrator", Permissions: json.RawMessage(`["tenant.member","lms.*","krecord.*"]`)},
	}
}

// ---------------------------------------------------------------------------
// Wizard
// ---------------------------------------------------------------------------

// Wizard encapsulates the setup flow so the HTTP handler can drive
// `RunSetupWizard` against the live pool while tests can substitute a
// fake. The wizard writes through the shared pool directly since
// neither roles nor accounts have RLS (they are keyed by tenant_id but
// the control-plane owns the seed path).
type Wizard struct {
	pool *pgxpool.Pool
}

// NewWizard binds the wizard to the shared pool.
func NewWizard(pool *pgxpool.Pool) *Wizard {
	return &Wizard{pool: pool}
}

// RunSetupWizard applies the supplied config to an existing tenant.
// The operation is best-effort transactional: account seeding and role
// seeding run in the same transaction so a failure halfway through
// rolls back both. User seeding runs in a follow-up transaction
// because creating a stub user is a control-plane mutation unrelated
// to the tenant schema.
func (w *Wizard) RunSetupWizard(ctx context.Context, tenantID uuid.UUID, cfg SetupWizardConfig) (*WizardResult, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: wizard requires tenant id")
	}
	if cfg.CompanyName == "" {
		return nil, errors.New("tenant: wizard requires company_name")
	}
	templateName := cfg.CoATemplate
	if templateName == "" {
		templateName = "us_gaap_basic"
	}
	accounts, err := loadTemplate(templateName)
	if err != nil {
		return nil, err
	}
	roles := cfg.Roles
	if len(roles) == 0 {
		roles = DefaultRoles()
	}

	out := &WizardResult{TenantID: tenantID, CoATemplateUsed: templateName}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant: wizard begin tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	accountsInserted, err := seedAccounts(ctx, tx, tenantID, accounts)
	if err != nil {
		return nil, err
	}
	out.AccountsInserted = accountsInserted

	rolesInserted, err := seedRoles(ctx, tx, tenantID, roles)
	if err != nil {
		return nil, err
	}
	out.RolesInserted = rolesInserted

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("tenant: wizard commit: %w", err)
	}

	if len(cfg.Users) > 0 {
		usersInserted, err := seedUsers(ctx, w.pool, tenantID, cfg.Users)
		if err != nil {
			return out, err
		}
		out.UsersInserted = usersInserted
	}
	return out, nil
}

// loadTemplate returns the parsed CoA for the named template. Unknown
// templates are surfaced as a 4xx via the sentinel error wrap.
func loadTemplate(name string) ([]templateAccount, error) {
	raw, ok := chartOfAccountsTemplates[name]
	if !ok {
		return nil, fmt.Errorf("tenant: unknown coa template %q", name)
	}
	var accounts []templateAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		return nil, fmt.Errorf("tenant: decode coa template %s: %w", name, err)
	}
	return accounts, nil
}

func seedAccounts(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, accounts []templateAccount) (int, error) {
	inserted := 0
	for _, a := range accounts {
		active := true
		if a.Active != nil {
			active = *a.Active
		}
		var parent any
		if a.ParentCode != "" {
			parent = a.ParentCode
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO accounts (tenant_id, code, name, type, parent_code, active)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, code) DO NOTHING`,
			tenantID, a.Code, a.Name, a.Type, parent, active,
		)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed account %s: %w", a.Code, err)
		}
		inserted++
	}
	return inserted, nil
}

func seedRoles(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, roles []WizardRole) (int, error) {
	inserted := 0
	for _, r := range roles {
		if r.Name == "" {
			continue
		}
		perms := r.Permissions
		if len(perms) == 0 {
			perms = json.RawMessage(`[]`)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO roles (tenant_id, name, description, permissions)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, name) DO NOTHING`,
			tenantID, r.Name, r.Description, perms,
		)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed role %s: %w", r.Name, err)
		}
		inserted++
	}
	return inserted, nil
}

func seedUsers(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, users []WizardUser) (int, error) {
	inserted := 0
	for _, u := range users {
		if u.Email == "" || u.Role == "" {
			continue
		}
		var userID uuid.UUID
		err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (email) DO UPDATE SET display_name = COALESCE(EXCLUDED.display_name, users.display_name)
			 RETURNING id`,
			uuid.New(), u.Email, u.DisplayName,
		).Scan(&userID)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed user %s: %w", u.Email, err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO user_tenants (tenant_id, user_id, role, status)
			 VALUES ($1, $2, $3, 'active')
			 ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role, status = 'active'`,
			tenantID, userID, u.Role,
		)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed user_tenants %s: %w", u.Email, err)
		}
		inserted++
	}
	return inserted, nil
}
