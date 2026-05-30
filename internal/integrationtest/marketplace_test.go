//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMarketplaceRegistry_EndToEnd is the integration check for
// Phase B2 (marketplace registry tables, PR #126):
//
//  1. CreateExtension inserts a unique listing; a second create with
//     the same (publisher, slug) returns ErrConflict (UNIQUE
//     constraint mapped to a 409-shaped sentinel).
//  2. PublishVersion creates an immutable per-version row, seeds the
//     review_state row, and returns ErrConflict on a re-publish of
//     the same (extension_id, version).
//  3. ListExtensions / GetVersion / ListVersions filter correctly,
//     including yanked-version visibility.
//  4. SetListedVersion + UpdateExtensionStatus walk the allowed
//     transition graph and reject disallowed transitions.
//  5. YankVersion soft-removes; the active-only index ignores yanked
//     rows but ListVersions(includeYanked=true) still returns them.
//  6. Install / UpdateInstallStatus / GetInstallation /
//     ListInstallationsForTenant are RLS-scoped — tenant A cannot
//     read tenant B's installation, even with the correct id, even
//     through the same Store instance.
//  7. The BEFORE UPDATE immutability trigger rejects an attempt to
//     mutate write-once columns on marketplace_extension_versions.
//  8. Review state UpdateReviewState walks the spec transition graph
//     and stamps reviewer/reviewed_at correctly when transitioning
//     to terminal states.
func TestMarketplaceRegistry_EndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// --- (1) Extension creation + uniqueness ---
	pub := uniqueSlug("acme")
	pub = strings.ReplaceAll(pub, "-", "_") // publisher regex disallows hyphens
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pub, Slug: "shipping",
		DisplayName: "Shipping Labels", Description: "Print labels via vendor X",
		Author: "ACME Corp", License: "MIT",
		Homepage: "https://acme.example/shipping",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}
	if ext.Name != pub+".shipping" {
		t.Fatalf("extension name composed wrong: %q", ext.Name)
	}
	if ext.Status != marketplace.ExtensionStatusUnpublished {
		t.Errorf("new extension should start unpublished, got %q", ext.Status)
	}
	if _, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pub, Slug: "shipping",
		DisplayName: "dup", Description: "dup",
		Author: "dup", License: "MIT",
	}); !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("duplicate Publisher/Slug should return ErrConflict, got %v", err)
	}

	gotByID, err := store.GetExtension(ctx, ext.ID)
	if err != nil || gotByID.ID != ext.ID {
		t.Fatalf("GetExtension by id: %v %+v", err, gotByID)
	}
	gotByName, err := store.GetExtensionByName(ctx, ext.Name)
	if err != nil || gotByName.ID != ext.ID {
		t.Fatalf("GetExtensionByName: %v %+v", err, gotByName)
	}
	if _, err := store.GetExtension(ctx, uuid.New()); !errors.Is(err, marketplace.ErrNotFound) {
		t.Fatalf("missing id should return ErrNotFound, got %v", err)
	}

	// --- (2) Publish v1.0.0 + ErrConflict on re-publish ---
	manifest1 := &marketplace.Manifest{
		SchemaVersion:    1,
		Name:             ext.Name,
		Publisher:        ext.Publisher,
		Slug:             ext.Slug,
		Version:          "1.0.0",
		Author:           "ACME Corp",
		License:          "MIT",
		Description:      "v1",
		MinKappVersion:   "1.0.0",
		FeaturesRequired: []string{"inventory"},
		KTypes:           []marketplace.KTypeRef{{Schema: "./ktypes/label.json"}},
	}
	hash1 := strings.Repeat("a", 64)
	ver1, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: manifest1,
		BundleHash: hash1, BundleSize: 4096,
		BundleURL: "https://cdn.example/bundles/v1.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	if ver1.Version != "1.0.0" || ver1.KtypesCount != 1 {
		t.Fatalf("PublishVersion result wrong: %+v", ver1)
	}
	// review_state row must be auto-created with default 'submitted'
	rs := store.Reviews()
	r1, err := rs.GetReviewState(ctx, ver1.ID)
	if err != nil {
		t.Fatalf("GetReviewState: %v", err)
	}
	if r1.Status != marketplace.ReviewStatusSubmitted {
		t.Errorf("seeded review status: want submitted got %q", r1.Status)
	}

	// Re-publish same version → 409
	if _, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: manifest1,
		BundleHash: hash1, BundleSize: 4096,
		BundleURL: "https://cdn.example/bundles/v1.tgz",
	}); !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("re-publish should return ErrConflict, got %v", err)
	}

	// Bundle-size cap is the easy 10 MiB+1 case
	if _, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: &marketplace.Manifest{
			SchemaVersion: 1, Name: ext.Name, Publisher: ext.Publisher, Slug: ext.Slug,
			Version: "9.9.9", Author: "x", License: "MIT", Description: "x",
			MinKappVersion: "1.0.0",
		},
		BundleHash: strings.Repeat("c", 64),
		BundleSize: marketplace.MaxBundleSizeBytes + 1,
		BundleURL:  "https://cdn.example/bundles/oversize.tgz",
	}); !errors.Is(err, marketplace.ErrBundleTooLarge) {
		t.Fatalf("oversized bundle should return ErrBundleTooLarge, got %v", err)
	}

	// --- (3) Second version + ListVersions ---
	manifest2 := *manifest1
	manifest2.Version = "1.1.0"
	ver2, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: &manifest2,
		BundleHash: strings.Repeat("b", 64), BundleSize: 8192,
		BundleURL: "https://cdn.example/bundles/v2.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion v2: %v", err)
	}
	versions, err := store.ListVersions(ctx, ext.ID, false)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
	// (versions order is published_at DESC per index)
	if versions[0].Version != "1.1.0" {
		t.Errorf("ListVersions order wrong: %v", versions[0].Version)
	}

	// --- (4) Extension status transitions ---
	// unpublished → deprecated MUST fail (only unpublished→listed allowed)
	if err := store.UpdateExtensionStatus(ctx, ext.ID, marketplace.ExtensionStatusDeprecated); err == nil {
		t.Error("expected rejection of unpublished→deprecated")
	}
	if err := store.SetListedVersion(ctx, ext.ID, "1.1.0"); err != nil {
		t.Fatalf("SetListedVersion: %v", err)
	}
	if err := store.UpdateExtensionStatus(ctx, ext.ID, marketplace.ExtensionStatusListed); err != nil {
		t.Fatalf("UpdateExtensionStatus listed: %v", err)
	}
	postListed, _ := store.GetExtension(ctx, ext.ID)
	if postListed.ListedVersion != "1.1.0" {
		t.Errorf("listed_version: want 1.1.0 got %q", postListed.ListedVersion)
	}
	if postListed.Status != marketplace.ExtensionStatusListed {
		t.Errorf("status: want listed got %q", postListed.Status)
	}

	// --- (5) Yank v1.0.0 + filtered visibility ---
	if err := store.YankVersion(ctx, ver1.ID, "CVE-2025-1234 superseded by 1.1.0"); err != nil {
		t.Fatalf("YankVersion: %v", err)
	}
	activeOnly, _ := store.ListVersions(ctx, ext.ID, false)
	if len(activeOnly) != 1 || activeOnly[0].ID == ver1.ID {
		t.Errorf("active list should exclude yanked: %+v", activeOnly)
	}
	all, _ := store.ListVersions(ctx, ext.ID, true)
	if len(all) != 2 {
		t.Errorf("includeYanked should return both: got %d", len(all))
	}

	// --- (6) Tenant-scoped install + RLS isolation ---
	tnA, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("mkt-a"), Name: "MktA Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("mkt-b"), Name: "MktB Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}

	// InstalledBy intentionally left nil — the optional installer
	// UUID FKs to users(id) and we don't seed a user row here.
	instA, err := store.Install(ctx, marketplace.InstallInput{
		TenantID: tnA.ID, ExtensionID: ext.ID, ExtensionVersionID: ver2.ID,
		WebhookBase: "https://tenant-a.example/hooks",
		Settings:    []byte(`{"foo":"bar"}`),
	})
	if err != nil {
		t.Fatalf("Install A: %v", err)
	}
	instB, err := store.Install(ctx, marketplace.InstallInput{
		TenantID: tnB.ID, ExtensionID: ext.ID, ExtensionVersionID: ver2.ID,
		WebhookBase: "https://tenant-b.example/hooks",
	})
	if err != nil {
		t.Fatalf("Install B: %v", err)
	}
	if instA.Status != marketplace.InstallStatusPending {
		t.Errorf("new install should be pending: got %q", instA.Status)
	}

	// Re-install in tenant A → conflict (UNIQUE per (tenant, extension))
	if _, err := store.Install(ctx, marketplace.InstallInput{
		TenantID: tnA.ID, ExtensionID: ext.ID, ExtensionVersionID: ver2.ID,
		WebhookBase: "https://tenant-a.example/hooks2",
	}); !errors.Is(err, marketplace.ErrConflict) {
		t.Errorf("re-install in same tenant should ErrConflict, got %v", err)
	}

	// RLS isolation: A's pool MUST NOT see B's install even with the
	// right id. Install / Get / List are all RLS-gated via
	// app.tenant_id. We only assert the cross-tenant negative case
	// when the test connection actually has policies applied — if the
	// connection is a superuser / BYPASSRLS role (common on shared
	// dev databases) every policy is short-circuited and the
	// negative assertion would be a false negative. The positive
	// case (each tenant sees its own row) is always asserted because
	// it's correct regardless of BYPASSRLS status.
	rlsEnforced := poolEnforcesRLS(ctx, t, h)
	if rlsEnforced {
		if _, err := store.GetInstallation(ctx, tnA.ID, instB.ID); !errors.Is(err, marketplace.ErrNotFound) {
			t.Errorf("RLS leak: tenant A read tenant B's install (id=%s): err=%v", instB.ID, err)
		}
		if _, err := store.GetInstallation(ctx, tnB.ID, instA.ID); !errors.Is(err, marketplace.ErrNotFound) {
			t.Errorf("RLS leak: tenant B read tenant A's install (id=%s): err=%v", instA.ID, err)
		}
	} else {
		t.Logf("connection bypasses RLS (superuser / BYPASSRLS); skipping cross-tenant negative assertions")
	}
	listA, _ := store.ListInstallationsForTenant(ctx, tnA.ID)
	foundA := false
	for _, in := range listA {
		if in.ID == instA.ID {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("ListInstallationsForTenant A: tenant A should see its own install, got %+v", listA)
	}
	listB, _ := store.ListInstallationsForTenant(ctx, tnB.ID)
	foundB := false
	for _, in := range listB {
		if in.ID == instB.ID {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("ListInstallationsForTenant B: tenant B should see its own install, got %+v", listB)
	}

	// Lifecycle: failed requires failure_reason
	if err := store.UpdateInstallStatus(ctx, tnA.ID, instA.ID, marketplace.InstallStatusFailed, ""); err == nil {
		t.Error("expected rejection: failed without reason")
	}
	if err := store.UpdateInstallStatus(ctx, tnA.ID, instA.ID, marketplace.InstallStatusActive, ""); err != nil {
		t.Fatalf("transition to active: %v", err)
	}
	got, _ := store.GetInstallation(ctx, tnA.ID, instA.ID)
	if got.Status != marketplace.InstallStatusActive {
		t.Errorf("install status: want active got %q", got.Status)
	}
	if err := store.RecordInstallHealthCheck(ctx, tnA.ID, instA.ID, "ok"); err != nil {
		t.Fatalf("RecordInstallHealthCheck: %v", err)
	}
	gotHC, _ := store.GetInstallation(ctx, tnA.ID, instA.ID)
	if gotHC.LastHealthCheckStatus != "ok" || gotHC.LastHealthCheckAt == nil {
		t.Errorf("health check not stamped: %+v", gotHC)
	}

	// --- (7) Immutability trigger: try to mutate a write-once column ---
	//
	// We have to go through dbutil.WithTenantTx for the installations
	// table, but the versions table is not RLS-gated — a plain
	// admin-style query is enough. The expected error message comes
	// from the marketplace_extension_versions_immutable trigger.
	_, err = h.pool.Exec(ctx,
		`UPDATE marketplace_extension_versions SET bundle_hash = $1 WHERE id = $2`,
		strings.Repeat("d", 64), ver2.ID,
	)
	if err == nil {
		t.Error("immutability trigger did not fire on bundle_hash mutation")
	} else if !strings.Contains(err.Error(), "bundle_hash is immutable") {
		t.Errorf("expected immutability message, got: %v", err)
	}

	// --- (8) Review state transition graph ---
	r1, err = rs.GetReviewState(ctx, ver1.ID)
	if err != nil {
		t.Fatalf("re-read review state: %v", err)
	}
	if r1.Status != marketplace.ReviewStatusSubmitted {
		t.Fatalf("precondition: submitted, got %q", r1.Status)
	}
	// submitted → approved must fail (only via automated_passed → manual_review → approved)
	if _, err := rs.UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID: ver1.ID, Status: marketplace.ReviewStatusApproved, Reviewer: "alice@example.com",
	}); err == nil {
		t.Error("expected rejection: submitted→approved (must walk the graph)")
	}
	// Walk the happy path one step at a time
	for _, step := range []marketplace.ReviewStatus{
		marketplace.ReviewStatusAutomatedPassed,
		marketplace.ReviewStatusManualReview,
		marketplace.ReviewStatusApproved,
	} {
		in := marketplace.UpdateReviewStateInput{
			VersionID: ver1.ID, Status: step,
			AutomatedChecks: []byte(`{"passed":true}`),
		}
		if step == marketplace.ReviewStatusApproved {
			in.Reviewer = "alice@example.com"
		}
		updated, err := rs.UpdateReviewState(ctx, in)
		if err != nil {
			t.Fatalf("transition to %s: %v", step, err)
		}
		if updated.Status != step {
			t.Errorf("transition: want %s got %s", step, updated.Status)
		}
		if step == marketplace.ReviewStatusApproved && updated.ReviewedAt == nil {
			t.Errorf("approved should stamp reviewed_at")
		}
		if step == marketplace.ReviewStatusApproved && updated.Reviewer != "alice@example.com" {
			t.Errorf("reviewer not persisted: %q", updated.Reviewer)
		}
	}
	// approved is terminal: rejected should fail
	if _, err := rs.UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID: ver1.ID, Status: marketplace.ReviewStatusRejected,
		Reviewer: "alice@example.com",
	}); err == nil {
		t.Error("approved→rejected should be rejected (terminal)")
	}

	// --- (9) ListExtensions filter ---
	listed, err := store.ListExtensions(ctx, marketplace.ListExtensionsFilter{
		Status: marketplace.ExtensionStatusListed,
	})
	if err != nil {
		t.Fatalf("ListExtensions: %v", err)
	}
	foundOurs := false
	for _, e := range listed {
		if e.ID == ext.ID {
			foundOurs = true
		}
	}
	if !foundOurs {
		t.Errorf("filtered list missing our listed extension; got %d rows", len(listed))
	}
	byPub, err := store.ListExtensions(ctx, marketplace.ListExtensionsFilter{Publisher: pub})
	if err != nil {
		t.Fatalf("ListExtensions by publisher: %v", err)
	}
	if len(byPub) != 1 || byPub[0].ID != ext.ID {
		t.Errorf("publisher-filtered list wrong: %+v", byPub)
	}
}

// TestMarketplaceRegistry_RLS_RawSQL is a defense-in-depth check
// confirming that the RLS policy on marketplace_extension_installations
// is enforced at the DB level (not just by the Go store filtering on
// tenant_id columns). Two installs under different tenants are
// inserted via WithTenantTx; then a single WithTenantTx for tenant A
// runs `SELECT count(*) FROM marketplace_extension_installations` and
// must see exactly 1 row even though 2 rows physically exist.
func TestMarketplaceRegistry_RLS_RawSQL(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	pub := strings.ReplaceAll(uniqueSlug("rls"), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pub, Slug: "thing",
		DisplayName: "Thing", Description: "rls test",
		Author: "x", License: "MIT",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}
	mf := &marketplace.Manifest{
		SchemaVersion: 1, Name: ext.Name, Publisher: ext.Publisher, Slug: ext.Slug,
		Version: "1.0.0", Author: "x", License: "MIT", Description: "x",
		MinKappVersion: "1.0.0",
	}
	ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: mf,
		BundleHash: strings.Repeat("e", 64), BundleSize: 1024,
		BundleURL: "https://cdn.example/rls.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	tnA, _ := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("rls-a"), Name: "rlsA", Cell: "test", Plan: "free",
	})
	tnB, _ := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("rls-b"), Name: "rlsB", Cell: "test", Plan: "free",
	})
	for _, in := range []marketplace.InstallInput{
		{TenantID: tnA.ID, ExtensionID: ext.ID, ExtensionVersionID: ver.ID, WebhookBase: "https://a.example/h"},
		{TenantID: tnB.ID, ExtensionID: ext.ID, ExtensionVersionID: ver.ID, WebhookBase: "https://b.example/h"},
	} {
		if _, err := store.Install(ctx, in); err != nil {
			t.Fatalf("Install(%s): %v", in.TenantID, err)
		}
	}

	if !poolEnforcesRLS(ctx, t, h) {
		t.Skip("connection bypasses RLS (superuser / BYPASSRLS); RLS negative assertions cannot be exercised on this DB — verified at production-like deploys via the kapp_app role.")
	}

	// In tenant A's context, the policy USING clause MUST hide tenant B's row.
	var seenAsA int64
	if err := dbutil.WithTenantTx(ctx, h.pool, tnA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM marketplace_extension_installations
			 WHERE extension_id = $1`,
			ext.ID,
		).Scan(&seenAsA)
	}); err != nil {
		t.Fatalf("count as tenant A: %v", err)
	}
	if seenAsA != 1 {
		t.Errorf("RLS failed: tenant A should see 1 install, saw %d", seenAsA)
	}

	// In tenant B's context, the policy USING clause MUST hide tenant A's row.
	var seenAsB int64
	if err := dbutil.WithTenantTx(ctx, h.pool, tnB.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM marketplace_extension_installations
			 WHERE extension_id = $1`,
			ext.ID,
		).Scan(&seenAsB)
	}); err != nil {
		t.Fatalf("count as tenant B: %v", err)
	}
	if seenAsB != 1 {
		t.Errorf("RLS failed: tenant B should see 1 install, saw %d", seenAsB)
	}

	// And confirm both rows physically exist by counting from the
	// admin pool (or, if no admin pool is configured for the test,
	// from two consecutive tenant contexts).
	if h.adminPool != nil {
		var total int64
		if err := h.adminPool.QueryRow(ctx,
			`SELECT count(*) FROM marketplace_extension_installations
			 WHERE extension_id = $1`,
			ext.ID,
		).Scan(&total); err != nil {
			t.Fatalf("admin count: %v", err)
		}
		if total != 2 {
			t.Errorf("admin sees %d rows, expected 2", total)
		}
	}
	// uuid import-protection (refers it without forcing the test to
	// always emit ids in messages):
	_ = fmt.Sprintf("%v", uuid.New())
}

// poolEnforcesRLS returns true when the current connection role would
// honour RLS policies. A role with rolbypassrls or rolsuper short-
// circuits every USING / WITH CHECK clause, so negative RLS
// assertions executed against such a role are false negatives.
// Production runs as kapp_app (neither superuser nor BYPASSRLS) so
// this returns true; shared dev DBs often run tests as `kapp`
// (superuser) and return false, in which case the caller should skip
// the strict negative test rather than emit a misleading failure.
func poolEnforcesRLS(ctx context.Context, t *testing.T, h *harness) bool {
	t.Helper()
	var isSuper, bypassRLS bool
	if err := h.pool.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&isSuper, &bypassRLS); err != nil {
		t.Fatalf("pg_roles lookup: %v", err)
	}
	return !(isSuper || bypassRLS)
}
