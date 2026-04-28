//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/authz"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestAuthzMultiRoleAndHierarchy exercises gaps 1, 4, 7, and 8 end-to-end:
//
//   - the wizard seeds the new granular roles into both `roles`
//     (with parent_role from migration 000050) and the legacy permissions
//     table fallback,
//   - a user assigned two roles in user_tenant_roles gets the union of
//     both roles' permissions back from PGEvaluator.ListPermissions,
//   - wildcard patterns ("finance.*") match concrete actions
//     ("finance.invoice.write") via Authorize,
//   - the parent_role chain inherits permissions: a finance.admin (whose
//     parent is tenant.member) is granted krecord.read because
//     tenant.member's permission pack includes it.
//
// The test runs only when KAPP_TEST_DB_URL is set (gated by newHarness).
func TestAuthzMultiRoleAndHierarchy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	slug := uniqueSlug("authz")
	tt, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: slug, Name: "Authz Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Run the wizard so the default + Phase RBAC roles are seeded.
	w := tenant.NewWizard(h.pool)
	userEmail := slug + "@example.com"
	wizCfg := tenant.SetupWizardConfig{
		CompanyName: "Authz Co",
		CoATemplate: "us_gaap_basic",
		Users: []tenant.WizardUser{
			{
				Email:       userEmail,
				DisplayName: "Test Operator",
				Roles:       []string{"finance.admin", "crm.rep"},
			},
		},
	}
	if _, err := w.RunSetupWizard(ctx, tt.ID, wizCfg); err != nil {
		t.Fatalf("wizard: %v", err)
	}

	// Resolve the seeded user_id.
	var userID uuid.UUID
	if err := h.pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, userEmail).Scan(&userID); err != nil {
		t.Fatalf("lookup user: %v", err)
	}

	// Sanity-check user_tenant_roles got both rows.
	if err := dbutil.WithTenantTx(ctx, h.pool, tt.ID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT role_name FROM user_tenant_roles WHERE tenant_id = $1 AND user_id = $2`,
			tt.ID, userID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			got = append(got, r)
		}
		if len(got) < 2 {
			t.Fatalf("expected >= 2 user_tenant_roles, got %d (%v)", len(got), got)
		}
		return nil
	}); err != nil {
		t.Fatalf("query user_tenant_roles: %v", err)
	}

	// Spin up an evaluator and exercise the authz surface.
	cache := platform.NewLRUCache(32, 5*time.Second)
	eval := authz.NewPGEvaluator(h.pool, cache)

	// Gap 4 — wildcard match ("finance.*" → "finance.invoice.write").
	if err := eval.Authorize(ctx, tt.ID, userID, "finance.invoice.write", ""); err != nil {
		t.Errorf("expected finance.* wildcard to grant finance.invoice.write: %v", err)
	}
	// Gap 1 — multi-role union: crm.rep grants crm.deal.write.
	if err := eval.Authorize(ctx, tt.ID, userID, "crm.deal.write", ""); err != nil {
		t.Errorf("expected crm.rep to grant crm.deal.write: %v", err)
	}
	// Gap 8 — hierarchy inherits tenant.member's krecord.read floor.
	if err := eval.Authorize(ctx, tt.ID, userID, "krecord.read", ""); err != nil {
		t.Errorf("expected tenant.member krecord.read to be inherited: %v", err)
	}

	// Gap 8 — adding a fresh permission to tenant.member should be
	// visible to descendants without editing each role.
	cache.Purge()
	if _, err := h.pool.Exec(ctx,
		`UPDATE roles
		    SET permissions = $3
		  WHERE tenant_id = $1 AND name = $2`,
		tt.ID, "tenant.member", json.RawMessage(`["tenant.member","krecord.read","platform.ping"]`),
	); err != nil {
		t.Fatalf("update tenant.member permissions: %v", err)
	}
	if err := eval.Authorize(ctx, tt.ID, userID, "platform.ping", ""); err != nil {
		t.Errorf("expected inherited platform.ping after tenant.member edit: %v", err)
	}

	// Gap 2 — owner_only condition: insert a permission with
	// {"owner_only":true} and assert AuthorizeRecord matches when
	// the actor owns the record but not when someone else does.
	cache.Purge()
	permID := uuid.New()
	if err := dbutil.WithTenantTx(ctx, h.pool, tt.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO permissions (id, tenant_id, role_name, ktype, action, conditions, granted_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			permID, tt.ID, "crm.rep", "crm.deal", "crm.deal.delete",
			json.RawMessage(`{"owner_only":true}`), userID,
		)
		return err
	}); err != nil {
		t.Fatalf("insert conditional permission: %v", err)
	}

	other := uuid.New()
	if err := eval.AuthorizeRecord(ctx, tt.ID, userID, "crm.deal.delete", "crm.deal", map[string]any{
		"owner": userID.String(),
	}); err != nil {
		t.Errorf("owner_only should grant when actor owns: %v", err)
	}
	if err := eval.AuthorizeRecord(ctx, tt.ID, userID, "crm.deal.delete", "crm.deal", map[string]any{
		"owner": other.String(),
	}); err == nil {
		t.Errorf("owner_only should deny when actor is not owner")
	}
}
