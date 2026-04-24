package tenant

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// defaultSLABreachActionType mirrors helpdesk.ActionTypeSLABreach.
// The tenant package cannot import helpdesk without creating a cycle
// (platform → tenant → helpdesk → ktype → platform), so the literal
// is duplicated here with a test-enforced drift check in
// internal/integrationtest/sla_breach_test.go. Keep them in sync.
const (
	defaultSLABreachActionType      = "sla_breach_check"
	defaultSLABreachIntervalSeconds = 300
)

// SetupWizardConfig is the payload a tenant owner submits to seed their
// newly-created tenant. It covers the first-run choices ERPNext surfaces
// in its own Setup Wizard — company profile, country/industry, the
// chart-of-accounts template, and the initial role roster.
type SetupWizardConfig struct {
	CompanyName  string       `json:"company_name"`
	Industry     string       `json:"industry,omitempty"`
	Country      string       `json:"country,omitempty"`
	CurrencyCode string       `json:"currency_code,omitempty"`
	CoATemplate  string       `json:"coa_template,omitempty"`
	Roles        []WizardRole `json:"roles,omitempty"`
	Users        []WizardUser `json:"users,omitempty"`
	SampleData   bool         `json:"sample_data,omitempty"`
	Plan         string       `json:"plan,omitempty"`
	CreatedBy    uuid.UUID    `json:"created_by,omitempty"`
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
// fake.
//
// The `accounts`, `roles`, and `user_tenants` tables are all
// RLS-protected (migrations/000001_initial_schema.sql). Under the
// production `kapp_app` role (migrations/000002_admin_role.sql) every
// INSERT/UPDATE must execute inside a transaction that has
// `app.tenant_id` set — otherwise the RLS WITH CHECK clause rejects
// the write. So every seed step here runs inside
// `dbutil.WithTenantTx`, which issues `SELECT set_config('app.tenant_id', …, true)`
// on the tx before calling the closure.
type Wizard struct {
	pool *pgxpool.Pool
}

// NewWizard binds the wizard to the shared pool.
func NewWizard(pool *pgxpool.Pool) *Wizard {
	return &Wizard{pool: pool}
}

// RunSetupWizard applies the supplied config to an existing tenant.
// Account seeding and role seeding share one tenant-scoped tx so a
// failure halfway through rolls both back. User seeding runs in a
// follow-up tenant-scoped tx since the control-plane user upsert on
// `users` (not RLS-gated) is independent of the `user_tenants` write
// (RLS-gated), and we want the `user_tenants` INSERT under the tenant
// GUC regardless.
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

	if err := dbutil.WithTenantTx(ctx, w.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		accountsInserted, err := seedAccounts(ctx, tx, tenantID, accounts)
		if err != nil {
			return err
		}
		out.AccountsInserted = accountsInserted

		rolesInserted, err := seedRoles(ctx, tx, tenantID, roles)
		if err != nil {
			return err
		}
		out.RolesInserted = rolesInserted

		// Seed the default per-tenant scheduled_actions rows the
		// worker handlers expect (SLA breach sweeper +
		// recurring-invoice generator). Idempotent on
		// (tenant_id, action_type) so a re-imported tenant never
		// duplicates queue rows. The interval defaults match the
		// values asserted by the integration drift tests in
		// internal/integrationtest/{sla_breach_test,recurring_invoice_test}.go.
		if err := seedDefaultScheduledActions(ctx, tx, tenantID); err != nil {
			return err
		}
		// Seed plan-appropriate feature flags. Free plan tenants
		// land on CRM-only; paid tiers unlock the rest. Uses
		// ON CONFLICT DO NOTHING so a re-run of the wizard never
		// overwrites operator-applied overrides.
		if err := seedDefaultFeatures(ctx, tx, tenantID, cfg.Plan); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("tenant: wizard seed accounts/roles: %w", err)
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
		// Side-fix: the `roles` table (migrations/000001) does not
		// carry a `description` column — the original wizard INSERT
		// referenced one, which made every first-run seed fail with
		// a 42703 once a test finally exercised this path (Task 4).
		// WizardRole.Description is still accepted on the API and
		// preserved in the struct; it is simply not persisted. A
		// follow-up migration can restore storage if the column is
		// ever wanted.
		_, err := tx.Exec(ctx,
			`INSERT INTO roles (tenant_id, name, permissions)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, name) DO NOTHING`,
			tenantID, r.Name, perms,
		)
		if err != nil {
			return inserted, fmt.Errorf("tenant: seed role %s: %w", r.Name, err)
		}
		inserted++
	}
	return inserted, nil
}

// seedUsers upserts into `users` on the control-plane pool (no RLS on
// that table) and then INSERTs into `user_tenants` under a
// tenant-scoped tx so the RLS WITH CHECK clause on `user_tenants`
// (migrations/000001_initial_schema.sql) is satisfied under
// `kapp_app`.
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
		if err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO user_tenants (tenant_id, user_id, role, status)
				 VALUES ($1, $2, $3, 'active')
				 ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = EXCLUDED.role, status = 'active'`,
				tenantID, userID, u.Role,
			)
			return err
		}); err != nil {
			return inserted, fmt.Errorf("tenant: seed user_tenants %s: %w", u.Email, err)
		}
		inserted++
	}
	return inserted, nil
}

// Default scheduled-action constants. Kept local — duplicating the
// strings here avoids a tenant → finance import cycle (finance
// already depends on internal/scheduler which depends on internal/
// platform which the wizard reaches indirectly through dbutil).
// The drift-check integration test
// (internal/integrationtest/recurring_invoice_test.go::TestSetupWizardSeedsRecurringInvoiceAction)
// asserts both sides stay in lock-step.
const (
	defaultRecurringInvoiceActionType      = "recurring_invoice"
	defaultRecurringInvoiceIntervalSeconds = 3600
)

// defaultInventoryReorderActionType mirrors inventory.ActionTypeReorder.
// Duplicated for the same cycle reason defaultSLABreachActionType is.
// The hourly cadence matches the finance recurring-invoice sweeper:
// row-level eligibility is gated on the item's reorder_level so a run
// more often than once per day costs only SQL filter passes.
const (
	defaultInventoryReorderActionType      = "inventory_reorder"
	defaultInventoryReorderIntervalSeconds = 3600
)

// seedDefaultScheduledActions seeds the per-tenant scheduled_actions
// rows the platform expects to exist after a successful wizard run.
// Uses INSERT … WHERE NOT EXISTS so re-running the wizard is a no-op
// and never duplicates queue rows.
func seedDefaultScheduledActions(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	now := time.Now().UTC()
	defaults := []struct {
		actionType      string
		intervalSeconds int
	}{
		{defaultSLABreachActionType, defaultSLABreachIntervalSeconds},
		{defaultRecurringInvoiceActionType, defaultRecurringInvoiceIntervalSeconds},
		{defaultInventoryReorderActionType, defaultInventoryReorderIntervalSeconds},
	}
	for _, d := range defaults {
		if _, err := tx.Exec(ctx,
			`INSERT INTO scheduled_actions
			     (tenant_id, action_type, interval_seconds, next_run_at, payload, enabled)
			 SELECT $1, $2, $3, $4, '{}'::jsonb, TRUE
			  WHERE NOT EXISTS (
			      SELECT 1 FROM scheduled_actions
			       WHERE tenant_id = $1 AND action_type = $2
			  )`,
			tenantID, d.actionType, d.intervalSeconds, now,
		); err != nil {
			return fmt.Errorf("tenant: seed scheduled action %s: %w", d.actionType, err)
		}
	}
	return nil
}

// seedDefaultFeatures inserts one tenant_features row per canonical
// feature flag with enabled = DefaultFeaturesForPlan(plan)[feature].
// INSERT … ON CONFLICT DO NOTHING so re-running the wizard after a
// tenant has manually overridden a flag is a no-op on that flag —
// the platform only seeds the default, it never rewrites operator
// intent.
func seedDefaultFeatures(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, plan string) error {
	defaults := DefaultFeaturesForPlan(plan)
	for _, key := range AllFeatures {
		enabled, ok := defaults[key]
		if !ok {
			// Unmapped feature → default enabled so new
			// additions opt in automatically.
			enabled = true
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_features (tenant_id, feature_key, enabled)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, feature_key) DO NOTHING`,
			tenantID, key, enabled,
		); err != nil {
			return fmt.Errorf("tenant: seed feature %q: %w", key, err)
		}
	}
	return nil
}
