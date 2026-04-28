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

// TestAuthzMultiRoleAndHierarchy exercises gaps 1, 2, 4, and 8
// end-to-end against the live Postgres fixture:
//
//   - the wizard seeds the granular default roles (with parent_role
//     from migration 000050) into the `roles` table,
//   - a user with two rows in user_tenant_roles gets the union of
//     both roles' permissions back from PGEvaluator.Authorize,
//   - wildcard patterns ("finance.*") match concrete actions
//     ("finance.invoice.write"),
//   - the parent_role chain inherits permissions: a finance.admin
//     (whose parent is tenant.member) is granted krecord.read
//     because tenant.member's permission pack includes it,
//   - permissions.conditions {"owner_only":true} is honoured by
//     AuthorizeRecord — the actor passes only when they own the
//     record.
//
// The test runs only when KAPP_TEST_DB_URL is set (gated by
// newHarness). User and membership rows are inserted directly rather
// than via the wizard's user-seeding path so this test does not
// depend on a unique constraint on users.email.
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

	// Run the wizard with no users — this seeds the default + Phase
	// RBAC roles and the default chart of accounts under the tenant
	// transaction. We deliberately skip the user-seeding path and
	// insert membership rows ourselves below.
	w := tenant.NewWizard(h.pool)
	if _, err := w.RunSetupWizard(ctx, tt.ID, tenant.SetupWizardConfig{
		CompanyName: "Authz Co",
		CoATemplate: "us_gaap_basic",
	}); err != nil {
		t.Fatalf("wizard: %v", err)
	}

	// Insert the test user directly (control-plane row, no RLS).
	userID := uuid.New()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO users (id, kchat_user_id, email, display_name)
		 VALUES ($1, $2, $3, $4)`,
		userID, "kc-"+slug, slug+"@example.com", "Test Operator",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Membership in the tenant — both legacy single-role column
	// and the new junction table. Two roles so the multi-role
	// aggregation has something to union.
	rolesToAssign := []string{"finance.admin", "crm.rep"}
	if err := dbutil.WithTenantTx(ctx, h.pool, tt.ID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_tenants (user_id, tenant_id, role, status)
			 VALUES ($1, $2, $3, 'active')`,
			userID, tt.ID, rolesToAssign[0],
		); err != nil {
			return err
		}
		for _, r := range rolesToAssign {
			if _, err := tx.Exec(ctx,
				`INSERT INTO user_tenant_roles (tenant_id, user_id, role_name)
				 VALUES ($1, $2, $3)
				 ON CONFLICT DO NOTHING`,
				tt.ID, userID, r,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// Sanity-check user_tenant_roles got both rows.
	if err := dbutil.WithTenantTx(ctx, h.pool, tt.ID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT role_name FROM user_tenant_roles
			  WHERE tenant_id = $1 AND user_id = $2
			  ORDER BY role_name`,
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
		if len(got) != len(rolesToAssign) {
			t.Fatalf("expected %d user_tenant_roles, got %d (%v)", len(rolesToAssign), len(got), got)
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
		tt.ID, "tenant.member",
		json.RawMessage(`["tenant.member","krecord.read","platform.ping"]`),
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
