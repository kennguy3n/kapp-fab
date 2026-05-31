//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundle"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
	mksettings "github.com/kennguy3n/kapp-fab/internal/marketplace/settings"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMarketplaceAPI_EndToEnd exercises the B6 happy path
// against a real Postgres pool: bundle resolution, settings
// schema validation (handler-side gate), engine install,
// engine update-settings, engine uninstall. The handler logic
// itself is unit-tested in services/api/marketplace_handlers_test.go;
// this test pins the contract that the resolver + validator +
// engine compose correctly.
//
// We use the bundle.InMemoryResolver instead of the HTTP
// resolver so the test runs hermetically (no fixture HTTP
// server, no flake risk on tar fixtures). The resolver is the
// same interface the production handler uses, so a mismatch in
// the resolver→engine signature shape is still caught.
func TestMarketplaceAPI_EndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// --- Seed catalog: extension + version, listed, approved. ---
	pub := strings.ReplaceAll(uniqueSlug("api"), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher:   pub,
		Slug:        "audit",
		DisplayName: "Audit Logger",
		Description: "API end-to-end test extension",
		Author:      "ACME",
		License:     "MIT",
		Homepage:    "https://acme.example/audit",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}
	man := minimalRuntimeManifest(ext)
	// Declare a settings schema so the handler-side validator
	// fires. The schema requires one string field; the test
	// install supplies a valid value, the malformed-install
	// test supplies an invalid one.
	man.SettingsSchema = "./settings.json"
	manifestJSON, err := json.Marshal(map[string]any{
		"schema_version":   man.SchemaVersion,
		"name":             man.Name,
		"publisher":        man.Publisher,
		"slug":             man.Slug,
		"version":          man.Version,
		"author":           man.Author,
		"license":          man.License,
		"description":      man.Description,
		"min_kapp_version": man.MinKappVersion,
		"settings_schema":  man.SettingsSchema,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID:  ext.ID,
		Manifest:     man,
		BundleHash:   strings.Repeat("d", 64),
		BundleSize:   4096,
		BundleURL:    "https://cdn.example/bundles/api.tgz",
		ManifestJSON: manifestJSON,
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	// Walk the review graph to approved, then list the
	// version. The engine's install-side precondition requires
	// the extension to be in 'listed' status with a non-yanked
	// version.
	rs := store.Reviews()
	reviewer := "00000000-0000-0000-0000-000000000001"
	for _, step := range []marketplace.ReviewStatus{
		marketplace.ReviewStatusAutomatedPassed,
		marketplace.ReviewStatusManualReview,
		marketplace.ReviewStatusApproved,
	} {
		if _, err := rs.UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
			VersionID: ver.ID,
			Status:    step,
			Reviewer:  reviewer,
		}); err != nil {
			t.Fatalf("review transition %s: %v", step, err)
		}
	}
	if err := store.SetListedVersion(ctx, ext.ID, ver.Version); err != nil {
		t.Fatalf("SetListedVersion: %v", err)
	}
	if err := store.UpdateExtensionStatus(ctx, ext.ID, marketplace.ExtensionStatusListed); err != nil {
		t.Fatalf("UpdateExtensionStatus listed: %v", err)
	}

	// --- Bundle: programmatically built ResolvedBundle that
	// includes the settings schema body. The InMemoryResolver
	// is keyed on version.ID, so the engine.Install call below
	// receives this bundle when it asks for the version's
	// resolved form.
	settingsSchema := json.RawMessage(`{
		"type":"object",
		"required":["api_key"],
		"properties":{"api_key":{"type":"string","minLength":1}}
	}`)
	rb := minimalResolvedBundle(man)
	rb.SettingsSchemaJSON = settingsSchema
	resolver := bundle.NewInMemoryResolver()
	resolver.Set(ver.ID.String(), rb)

	// --- Tenant + engine wiring. NoopHooks so the test
	// doesn't depend on an HTTP fixture; the install /
	// uninstall lifecycle correctness is covered by
	// marketplace_runtime_test.go.
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("api-t"), Name: "API-T", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("Create tenant: %v", err)
	}
	engine, err := runtime.NewEngine(runtime.EngineOptions{
		Pool:  h.pool,
		Store: store,
		Hooks: runtime.NoopHooks(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// --- (1) Schema-validation gate fires for an invalid
	// settings document before the engine is invoked. The
	// production handler does the same check; we exercise the
	// validator directly here against the resolved schema.
	validator, err := mksettings.NewValidator(rb.SettingsSchemaJSON)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if err := validator.Validate(map[string]any{}); err == nil {
		t.Fatalf("missing api_key: validator returned nil, want error")
	}
	if err := validator.Validate(map[string]any{"api_key": "tok"}); err != nil {
		t.Fatalf("valid settings: validator returned %v, want nil", err)
	}

	// --- (2) Resolve bundle then install via engine.
	resolved, err := resolver.Resolve(ctx, ver)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bytesEqual(resolved.SettingsSchemaJSON, settingsSchema) {
		t.Fatalf("resolver did not preserve settings schema body")
	}
	installResult, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID:    tn.ID,
		ExtensionID: ext.ID,
		VersionID:   ver.ID,
		WebhookBase: "https://tenant.example/hooks",
		Settings:    map[string]any{"api_key": "initial-key"},
	}, resolved)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installResult.Installation == nil ||
		installResult.Installation.Status != marketplace.InstallStatusActive {
		t.Fatalf("install status = %v, want active",
			installResult.Installation)
	}
	// Sanity: install row is visible via the tenant-scoped
	// listing. The explicit tenant_id predicate (Devin Review
	// ANALYSIS_0005 on PR #128) means this is correct under
	// both RLS and BYPASSRLS roles.
	listed, err := store.ListInstallationsForTenant(ctx, tn.ID)
	if err != nil {
		t.Fatalf("ListInstallationsForTenant: %v", err)
	}
	var seen bool
	for _, in := range listed {
		if in.ID == installResult.Installation.ID {
			seen = true
			break
		}
	}
	if !seen {
		t.Fatalf("install row not in tenant listing")
	}

	// --- (3) UpdateSettings happy path. The engine assumes
	// the caller validated against the manifest schema, so we
	// pre-validate here to mirror the handler's gate.
	newSettings := map[string]any{"api_key": "rotated-key"}
	if err := validator.Validate(newSettings); err != nil {
		t.Fatalf("validator rejected pre-update settings: %v", err)
	}
	updRes, err := engine.UpdateSettings(ctx, &runtime.UpdateSettingsRequest{
		TenantID:       tn.ID,
		InstallationID: installResult.Installation.ID,
		Settings:       newSettings,
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if updRes.Installation == nil {
		t.Fatalf("UpdateSettings: nil installation")
	}
	// Verify the new settings actually landed in the row by
	// reading back through the store.
	current, err := store.GetInstallation(ctx, tn.ID, installResult.Installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(current.Settings, &got); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if got["api_key"] != "rotated-key" {
		t.Fatalf("settings.api_key = %v, want rotated-key", got["api_key"])
	}

	// --- (4) Uninstall flips the row to 'uninstalled'.
	if _, err := engine.Uninstall(ctx, &runtime.UninstallRequest{
		TenantID:       tn.ID,
		InstallationID: installResult.Installation.ID,
	}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	uninstalled, err := store.GetInstallation(ctx, tn.ID, installResult.Installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation post-uninstall: %v", err)
	}
	if uninstalled.Status != marketplace.InstallStatusUninstalled {
		t.Fatalf("post-uninstall status = %v, want uninstalled", uninstalled.Status)
	}

	// --- (5) UpdateSettings after uninstall is rejected. The
	// engine inspects the install row's status under FOR
	// UPDATE; an uninstalled row must not accept new settings.
	_, err = engine.UpdateSettings(ctx, &runtime.UpdateSettingsRequest{
		TenantID:       tn.ID,
		InstallationID: installResult.Installation.ID,
		Settings:       map[string]any{"api_key": "post-uninstall"},
	})
	if err == nil {
		t.Fatalf("UpdateSettings on uninstalled row: err = nil, want non-nil")
	}

	// --- (6) Cross-tenant isolation. Tenant B has no install
	// row for this extension; the engine's GetInstallation
	// reads under tenant-scoped tx so the row is invisible.
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("api-b"), Name: "API-B", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("Create tenant B: %v", err)
	}
	_, err = store.GetInstallation(ctx, tnB.ID, installResult.Installation.ID)
	if !errors.Is(err, marketplace.ErrNotFound) {
		t.Fatalf("cross-tenant GetInstallation: err = %v, want ErrNotFound", err)
	}
}

// TestMarketplaceAPI_BundleResolverErrorPath ensures the
// InMemoryResolver surfaces ErrBundleNotFound when asked for a
// version it doesn't know about. The production handler maps
// this sentinel to HTTP 502 (Bad Gateway) via
// marketplaceHandlers.writeError.
func TestMarketplaceAPI_BundleResolverErrorPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	pub := strings.ReplaceAll(uniqueSlug("api-err"), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher:   pub,
		Slug:        "audit",
		DisplayName: "Audit",
		Description: "Bundle-error test",
		Author:      "ACME",
		License:     "MIT",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}
	ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: minimalRuntimeManifest(ext),
		BundleHash: strings.Repeat("e", 64),
		BundleSize: 1024,
		BundleURL:  "https://cdn.example/bundles/api-err.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	resolver := bundle.NewInMemoryResolver()
	// Intentionally do not register the bundle.
	_, err = resolver.Resolve(ctx, ver)
	if !errors.Is(err, bundle.ErrBundleNotFound) {
		t.Fatalf("unregistered bundle: err = %v, want ErrBundleNotFound", err)
	}
}

// bytesEqual is a small helper for json.RawMessage comparison
// in tests; avoids pulling in reflect.DeepEqual just for a slice
// compare.
func bytesEqual(a, b json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// unusedDBImports keeps go vet happy when this file is built
// with the integration tag but the helpers above don't reach
// for dbutil or pgx directly. The harness imports them
// transitively; an explicit reference here would dead-code-
// trip a future cleanup.
var (
	_ = pgx.ErrNoRows
	_ = dbutil.WithTenantTx
)
