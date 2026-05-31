//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMarketplaceRuntime_Upgrade_HappyPath exercises the
// Engine.Upgrade flow end-to-end:
//
//  1. Seed a publisher with two listed-and-approved versions (v1.0
//     and v1.1) of the same extension.
//  2. Install v1.0 on a tenant — verifies the install row +
//     runtime tables (ktypes / workflows / agent_tools /
//     webhook_subscriptions) hold v1.0's bundle shape.
//  3. Call Engine.Upgrade(from=v1.0, to=v1.1) with KeepSettings.
//  4. Assert: the install row's extension_version_id moved to v1.1,
//     status remains 'active', updated_at advanced, settings JSONB
//     preserved verbatim.
//  5. Assert: every runtime table now holds v1.1's NEW resource
//     names (different KType / workflow / tool / webhook than v1.0).
//     The old v1.0 rows were UnregisterAll'd inside the same tx
//     before RegisterAll wrote the v1.1 set, so we can verify the
//     net row count is still exactly one of each.
//  6. Assert: pre_upgrade + post_upgrade both fired on the in-
//     memory transport (counted via Len).
//  7. Cleanup: uninstall to leave the runtime tables clean for
//     subsequent tests sharing the same harness.
func TestMarketplaceRuntime_Upgrade_HappyPath(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, toVer, fromBundle, toBundle := seedTwoListedVersions(ctx, t, store, "upg")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upg-tn"), Name: "UPG-T", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{
		Pool:  h.pool,
		Store: store,
		Hooks: hooks,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID:    tn.ID,
		ExtensionID: ext.ID,
		VersionID:   fromVer.ID,
		WebhookBase: "https://tenant.example/hooks",
		Settings:    map[string]any{"foo": "bar", "limit": float64(42)},
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install v1.0: %v", err)
	}
	if installRes.Installation.ExtensionVersionID != fromVer.ID {
		t.Fatalf("post-install version_id = %s, want %s",
			installRes.Installation.ExtensionVersionID, fromVer.ID)
	}
	preUpgradeUpdatedAt := installRes.Installation.UpdatedAt

	// Sanity-check pre-upgrade runtime row shape: v1.0 names.
	beforeCounts := readRuntimeCounts(ctx, t, h, tn.ID, installRes.Installation.ID)
	if beforeCounts.ktypes != 1 || beforeCounts.workflows != 1 || beforeCounts.tools != 1 || beforeCounts.webhooks != 1 {
		t.Fatalf("pre-upgrade counts = %+v, want 1 of each", beforeCounts)
	}
	beforeKType := readSingleKTypeName(ctx, t, h, tn.ID, installRes.Installation.ID)
	wantFromKType := fmt.Sprintf("ext.%s.%s_label", ext.Publisher, ext.Slug)
	if beforeKType != wantFromKType {
		t.Fatalf("pre-upgrade ktype name = %q, want %q", beforeKType, wantFromKType)
	}

	transportCallsBeforeUpgrade := transport.Len()

	// Sleep a hair so the post-upgrade updated_at is strictly
	// greater than the post-install one (Postgres now() resolution
	// is microsecond; without the sleep a fast machine can produce
	// equal timestamps and the inequality check below is racy).
	time.Sleep(2 * time.Millisecond)

	upRes, err := engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle)
	if err != nil {
		t.Fatalf("Upgrade v1.0→v1.1: %v", err)
	}
	if upRes.Installation == nil {
		t.Fatalf("Upgrade returned nil Installation")
	}
	if upRes.Installation.ExtensionVersionID != toVer.ID {
		t.Fatalf("post-upgrade version_id = %s, want %s",
			upRes.Installation.ExtensionVersionID, toVer.ID)
	}
	if upRes.FromVersionID != fromVer.ID {
		t.Fatalf("UpgradeResult.FromVersionID = %s, want %s",
			upRes.FromVersionID, fromVer.ID)
	}
	if upRes.Installation.Status != marketplace.InstallStatusActive {
		t.Fatalf("post-upgrade status = %q, want active", upRes.Installation.Status)
	}
	if !upRes.Installation.UpdatedAt.After(preUpgradeUpdatedAt) {
		t.Fatalf("post-upgrade updated_at %v not strictly after pre-upgrade %v",
			upRes.Installation.UpdatedAt, preUpgradeUpdatedAt)
	}
	// KeepSettings → existing settings document preserved verbatim.
	// Decoding the JSONB bytes back to a map should produce the
	// same key set we installed with.
	var preserved map[string]any
	if err := json.Unmarshal(upRes.Installation.Settings, &preserved); err != nil {
		t.Fatalf("decode post-upgrade settings: %v", err)
	}
	if preserved["foo"] != "bar" || preserved["limit"] != float64(42) {
		t.Fatalf("KeepSettings did not preserve settings; got %+v", preserved)
	}

	// Lifecycle assertions: both hooks fired.
	if upRes.PreUpgradeResult == nil || upRes.PreUpgradeResult.Aborted {
		t.Fatalf("pre_upgrade result = %+v, want non-aborted", upRes.PreUpgradeResult)
	}
	if upRes.PostUpgradeResult == nil || upRes.PostUpgradeResult.Aborted {
		t.Fatalf("post_upgrade result = %+v, want non-aborted", upRes.PostUpgradeResult)
	}
	if got := transport.Len() - transportCallsBeforeUpgrade; got != 2 {
		t.Fatalf("upgrade hook dispatches = %d, want 2 (pre + post)", got)
	}

	// Runtime tables: same install_id, but the resources are now
	// v1.1's. Counts should still be 1 of each (UnregisterAll +
	// RegisterAll atomically swapped them in-tx). The KType name
	// also changed to v1.1's local slug.
	afterCounts := readRuntimeCounts(ctx, t, h, tn.ID, installRes.Installation.ID)
	if afterCounts.ktypes != 1 || afterCounts.workflows != 1 || afterCounts.tools != 1 || afterCounts.webhooks != 1 {
		t.Fatalf("post-upgrade counts = %+v, want 1 of each (no orphan rows)", afterCounts)
	}
	afterKType := readSingleKTypeName(ctx, t, h, tn.ID, installRes.Installation.ID)
	wantToKType := fmt.Sprintf("ext.%s.%s_label_v2", ext.Publisher, ext.Slug)
	if afterKType != wantToKType {
		t.Fatalf("post-upgrade ktype name = %q, want %q", afterKType, wantToKType)
	}

	// Dispatch log: post_install + pre_upgrade + post_upgrade
	// rows MUST be visible against this install_id. pre_install
	// writes with installation_id = NULL (the install row does
	// not yet exist at pre_install dispatch time — see
	// dispatch_log.go:54 comment), so the WHERE-equal query
	// readDispatchLogCount uses correctly excludes it. We expect
	// at least 3 rows from the visible set; the pre_install row
	// is verified separately by the runtime end-to-end test.
	if got := readDispatchLogCount(ctx, t, h, tn.ID, installRes.Installation.ID); got < 3 {
		t.Fatalf("dispatch_log rows = %d, want >=3 (post_install + pre_upgrade + post_upgrade)", got)
	}
}

// TestMarketplaceRuntime_Upgrade_VersionMismatch verifies the
// TOCTOU guard: if the install row's extension_version_id has
// moved between the caller observing it and the engine acquiring
// FOR UPDATE inside the tx, the engine surfaces ErrVersionMismatch
// and the runtime tables are NOT touched.
//
// We simulate the race by issuing a successful upgrade v1.0 → v1.1
// then issuing a second upgrade request that still claims
// FromVersionID = v1.0. The second request must reject.
func TestMarketplaceRuntime_Upgrade_VersionMismatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, toVer, fromBundle, toBundle := seedTwoListedVersions(ctx, t, store, "upgmis")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upgmis-tn"), Name: "UPG-M", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: hooks})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install v1.0: %v", err)
	}

	// First upgrade: v1.0 → v1.1, succeeds.
	if _, err := engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle); err != nil {
		t.Fatalf("first Upgrade v1.0→v1.1: %v", err)
	}

	// Second upgrade: caller still believes the install is at
	// v1.0. The engine's in-tx FOR UPDATE re-read finds v1.1 and
	// surfaces ErrVersionMismatch. The pre-tx GetInstallation
	// catches this first in fact — both paths satisfy the
	// sentinel-error contract.
	_, err = engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID, // stale
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle)
	if !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatalf("second Upgrade should ErrVersionMismatch; got %v", err)
	}

	// Confirm the install row is still at v1.1 — the rejected
	// second upgrade did NOT touch it.
	in, err := store.GetInstallation(ctx, tn.ID, installRes.Installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	if in.ExtensionVersionID != toVer.ID {
		t.Fatalf("post-reject install at %s, want v1.1 %s", in.ExtensionVersionID, toVer.ID)
	}

	// And runtime tables still hold exactly v1.1's rows (no
	// orphan unregisters from the rejected attempt).
	counts := readRuntimeCounts(ctx, t, h, tn.ID, installRes.Installation.ID)
	if counts.ktypes != 1 || counts.workflows != 1 || counts.tools != 1 || counts.webhooks != 1 {
		t.Fatalf("post-reject counts = %+v, want 1 of each", counts)
	}
}

// TestMarketplaceRuntime_Upgrade_PreHookRejected verifies the
// BLOCKING pre_upgrade contract: when the extension's webhook
// returns a non-2xx, non-404 status (the engine treats 404 as
// "the extension didn't implement this hook" → continue),
// Engine.Upgrade surfaces ErrPreUpgradeRejected and the
// install row's extension_version_id is unchanged.
func TestMarketplaceRuntime_Upgrade_PreHookRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, toVer, fromBundle, toBundle := seedTwoListedVersions(ctx, t, store, "upgrej")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upgrej-tn"), Name: "UPG-R", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	// Install transport: 200 OK for the install hooks.
	okTransport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	okHooks := runtime.NewTransportHooks(okTransport, h.pool, nil)
	installEngine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: okHooks})
	if err != nil {
		t.Fatalf("NewEngine install: %v", err)
	}
	installRes, err := installEngine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install v1.0: %v", err)
	}

	// Upgrade transport: returns 422 for the pre_upgrade hook —
	// extension is refusing the upgrade (e.g. data migration not
	// ready). Body carries a structured reason so the engine has
	// something to put in AbortReason.
	rejectTransport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(422, []byte(`{"error":"migration not ready"}`)),
	}
	rejectHooks := runtime.NewTransportHooks(rejectTransport, h.pool, nil)
	rejectEngine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: rejectHooks})
	if err != nil {
		t.Fatalf("NewEngine reject: %v", err)
	}

	_, err = rejectEngine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle)
	if !errors.Is(err, runtime.ErrPreUpgradeRejected) {
		t.Fatalf("Upgrade with 422 pre_upgrade should ErrPreUpgradeRejected; got %v", err)
	}

	// Install row still at v1.0 — pre-hook reject aborted before
	// any DB write.
	in, err := store.GetInstallation(ctx, tn.ID, installRes.Installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	if in.ExtensionVersionID != fromVer.ID {
		t.Fatalf("post-reject install at %s, want v1.0 %s", in.ExtensionVersionID, fromVer.ID)
	}

	// Runtime tables still hold v1.0's rows — the in-tx
	// UnregisterAll never ran.
	wantFromKType := fmt.Sprintf("ext.%s.%s_label", ext.Publisher, ext.Slug)
	gotKType := readSingleKTypeName(ctx, t, h, tn.ID, installRes.Installation.ID)
	if gotKType != wantFromKType {
		t.Fatalf("post-reject ktype name = %q, want %q (v1.0 unchanged)", gotKType, wantFromKType)
	}

	// Exactly one hook attempt went out — the pre_upgrade. The
	// post_upgrade hook is never reached on pre-reject.
	if got := rejectTransport.Len(); got != 1 {
		t.Fatalf("hook attempts on pre-reject = %d, want 1 (pre_upgrade only)", got)
	}
}

// TestMarketplaceRuntime_Upgrade_SameVersionRejected verifies the
// Validate-side guard against upgrading to the version the install
// is already at. The validator must reject before any DB read so
// we don't even dispatch a pre_upgrade hook for a no-op.
func TestMarketplaceRuntime_Upgrade_SameVersionRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, _, fromBundle, _ := seedTwoListedVersions(ctx, t, store, "upgsame")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upgsame-tn"), Name: "UPG-S", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: hooks})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install v1.0: %v", err)
	}

	transportCallsBefore := transport.Len()

	_, err = engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    fromVer.ID, // same version
		KeepSettings:   true,
	}, fromBundle)
	if !errors.Is(err, runtime.ErrSameVersionUpgrade) {
		t.Fatalf("Upgrade with same from/to should ErrSameVersionUpgrade; got %v", err)
	}

	// No hook attempts — Validate() rejected before dispatch.
	if got := transport.Len() - transportCallsBefore; got != 0 {
		t.Fatalf("hook attempts on same-version upgrade = %d, want 0", got)
	}
}

// TestMarketplaceRuntime_Upgrade_SettingsMigration verifies the
// caller-supplied Settings branch: a migrated document is written
// to the install row verbatim (the handler is responsible for
// schema-validating it before the engine sees the request, so the
// engine treats it as opaque JSON).
func TestMarketplaceRuntime_Upgrade_SettingsMigration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, toVer, fromBundle, toBundle := seedTwoListedVersions(ctx, t, store, "upgset")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upgset-tn"), Name: "UPG-S2", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: hooks})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
		Settings:    map[string]any{"old_field": "legacy_value"},
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	migrated := map[string]any{
		"new_field":     "fresh_value",
		"renamed_field": "legacy_value",
		"version":       float64(2),
	}
	upRes, err := engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		Settings:       migrated,
	}, toBundle)
	if err != nil {
		t.Fatalf("Upgrade with migrated settings: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(upRes.Installation.Settings, &got); err != nil {
		t.Fatalf("decode post-upgrade settings: %v", err)
	}
	if got["new_field"] != "fresh_value" || got["renamed_field"] != "legacy_value" || got["version"] != float64(2) {
		t.Fatalf("migrated settings not persisted; got %+v, want %+v", got, migrated)
	}
	if _, has := got["old_field"]; has {
		t.Fatalf("migrated settings should not retain old_field; got %+v", got)
	}
}

// TestMarketplaceRuntime_Upgrade_ConcurrentRace fires two upgrades
// at the same install row concurrently, both starting from v1.0:
// one upgrades to v1.1, the other to v1.2 (an inert third version
// shaped identically to v1.1 — same bundle, registered as a
// distinct catalog row). Exactly one must succeed; the other must
// surface ErrVersionMismatch. The runtime tables must reflect the
// winning upgrade with no orphan rows from the loser.
func TestMarketplaceRuntime_Upgrade_ConcurrentRace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// Reuse the same publisher to seed three versions: v1.0
	// (install), then v1.1 + v1.2 as parallel upgrade targets.
	ext, fromVer, toVerA, fromBundle, toBundleA := seedTwoListedVersions(ctx, t, store, "upgrace")

	// Add a third version (v1.2) on the same extension. The
	// bundle differs from v1.1 by KType slug suffix so the
	// runtime-table assertions can identify the winner.
	v1_2Manifest := minimalRuntimeManifestVersioned(ext, "1.2.0", "v3")
	v1_2, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: v1_2Manifest,
		BundleHash: strings.Repeat("c", 64),
		BundleSize: 4096,
		BundleURL:  "https://cdn.example/bundles/v1.2.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion v1.2: %v", err)
	}
	approveListedVersion(ctx, t, store, ext, v1_2)
	toBundleB := minimalResolvedBundleVersioned(v1_2Manifest, "v3")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upgrace-tn"), Name: "UPG-RACE", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: hooks})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	var (
		wg                  sync.WaitGroup
		mu                  sync.Mutex
		errs                = make([]error, 2)
		results             = make([]*runtime.UpgradeResult, 2)
		barrier             = make(chan struct{})
		concurrentTargets   = []uuid.UUID{toVerA.ID, v1_2.ID}
		concurrentBundles   = []*runtime.ResolvedBundle{toBundleA, toBundleB}
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			<-barrier
			res, err := engine.Upgrade(ctx, &runtime.UpgradeRequest{
				TenantID:       tn.ID,
				InstallationID: installRes.Installation.ID,
				FromVersionID:  fromVer.ID,
				ToVersionID:    concurrentTargets[idx],
				KeepSettings:   true,
			}, concurrentBundles[idx])
			mu.Lock()
			defer mu.Unlock()
			errs[idx] = err
			results[idx] = res
		}(i)
	}
	close(barrier)
	wg.Wait()

	// Outcome contract: exactly one succeeded, the other got
	// ErrVersionMismatch. ANY other pair (both succeed, both
	// fail, fail-with-different-error) is a bug.
	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, runtime.ErrVersionMismatch) {
			t.Fatalf("goroutine %d got unexpected error %v, want nil or ErrVersionMismatch", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 success, got %d (errs=%v)", successes, errs)
	}

	// The install row reflects the winner. Both targets were
	// listed-and-approved, so either v1.1 or v1.2 is acceptable
	// as the winning final state — but exactly one of them.
	in, err := store.GetInstallation(ctx, tn.ID, installRes.Installation.ID)
	if err != nil {
		t.Fatalf("GetInstallation: %v", err)
	}
	if in.ExtensionVersionID != toVerA.ID && in.ExtensionVersionID != v1_2.ID {
		t.Fatalf("post-race install at %s, want v1.1 (%s) or v1.2 (%s)",
			in.ExtensionVersionID, toVerA.ID, v1_2.ID)
	}

	// Runtime tables: still exactly 1 of each (no orphans from
	// the rolled-back loser).
	counts := readRuntimeCounts(ctx, t, h, tn.ID, installRes.Installation.ID)
	if counts.ktypes != 1 || counts.workflows != 1 || counts.tools != 1 || counts.webhooks != 1 {
		t.Fatalf("post-race counts = %+v, want 1 of each", counts)
	}
}

// TestMarketplaceRuntime_Upgrade_FromUninstalledRejected verifies
// that an uninstalled install cannot be upgraded. The engine reads
// the install row outside the tx as an early-out and again under
// FOR UPDATE inside the tx — both branches map to ErrConflict.
func TestMarketplaceRuntime_Upgrade_FromUninstalledRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, toVer, fromBundle, toBundle := seedTwoListedVersions(ctx, t, store, "upguni")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upguni-tn"), Name: "UPG-U", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: hooks})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := engine.Uninstall(ctx, &runtime.UninstallRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
	}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	_, err = engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle)
	if !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("Upgrade of uninstalled should ErrConflict; got %v", err)
	}
}

// TestMarketplaceRuntime_Upgrade_FromDisabledRejected pins the
// allowlist semantics of Engine.Upgrade: `disabled` (and `pending`)
// installs must NOT be upgradable. The in-tx UPDATE unconditionally
// sets status='active', so a blocklist that only rejected
// `uninstalled` + `installing` would silently re-activate a
// `disabled` install (bypassing admin intent — e.g. an extension
// paused for billing dispute or security review). Same hazard
// applies to `pending` (would advance the install before first-time
// setup completes).
//
// Both the pre-tx early-out and the in-tx FOR UPDATE guard must
// reject. We can only exercise the pre-tx branch from outside the
// engine — the in-tx branch is covered by code review (same
// allowlist literal, same ErrConflict path).
func TestMarketplaceRuntime_Upgrade_FromDisabledRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ext, fromVer, toVer, fromBundle, toBundle := seedTwoListedVersions(ctx, t, store, "upgdis")

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("upgdis-tn"), Name: "UPG-D", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}

	transport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`)),
	}
	hooks := runtime.NewTransportHooks(transport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{Pool: h.pool, Store: store, Hooks: hooks})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID: tn.ID, ExtensionID: ext.ID, VersionID: fromVer.ID,
		WebhookBase: "https://tn.example/hooks",
	}, fromBundle)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Force the install to 'disabled' via direct SQL — there is
	// no public store method for status transitions in this PR's
	// scope, and the production path (admin Pause endpoint) is
	// out of scope for B6.1. Tenant-scoped tx is required because
	// the install row is RLS-gated.
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE marketplace_extension_installations
			    SET status = 'disabled', updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tn.ID, installRes.Installation.ID)
		return err
	}); err != nil {
		t.Fatalf("force disabled: %v", err)
	}

	_, err = engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle)
	if !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("Upgrade of disabled should ErrConflict; got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected error mentioning 'disabled' status; got %v", err)
	}

	// Confirm the install row is still at FromVersionID and
	// still 'disabled' — i.e. the rejected upgrade did NOT
	// silently re-activate it.
	cur, err := store.GetInstallation(ctx, tn.ID, installRes.Installation.ID)
	if err != nil {
		t.Fatalf("re-read install: %v", err)
	}
	if cur.Status != marketplace.InstallStatusDisabled {
		t.Fatalf("install status mutated by rejected upgrade: got %q want %q",
			cur.Status, marketplace.InstallStatusDisabled)
	}
	if cur.ExtensionVersionID != fromVer.ID {
		t.Fatalf("install version mutated by rejected upgrade: got %s want %s",
			cur.ExtensionVersionID, fromVer.ID)
	}

	// Same allowlist check applies to 'pending'.
	if err := dbutil.WithTenantTx(ctx, h.pool, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE marketplace_extension_installations
			    SET status = 'pending', updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tn.ID, installRes.Installation.ID)
		return err
	}); err != nil {
		t.Fatalf("force pending: %v", err)
	}
	_, err = engine.Upgrade(ctx, &runtime.UpgradeRequest{
		TenantID:       tn.ID,
		InstallationID: installRes.Installation.ID,
		FromVersionID:  fromVer.ID,
		ToVersionID:    toVer.ID,
		KeepSettings:   true,
	}, toBundle)
	if !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("Upgrade of pending should ErrConflict; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// seedTwoListedVersions creates a publisher, one extension, and two
// listed-and-approved versions of that extension (v1.0 + v1.1) with
// distinct resource names so the runtime-table assertions can tell
// them apart after an upgrade. Returns the extension row, both
// version rows, and the resolved bundles the registrar expects.
//
// The publisher / extension slug are derived from the supplied
// `tag` so each test gets its own isolated catalog row (otherwise
// the unique (publisher, slug) constraint would conflict across
// parallel test runs sharing the harness's catalog).
func seedTwoListedVersions(
	ctx context.Context, t *testing.T, store *marketplace.Store, tag string,
) (
	*marketplace.Extension,
	*marketplace.ExtensionVersion,
	*marketplace.ExtensionVersion,
	*runtime.ResolvedBundle,
	*runtime.ResolvedBundle,
) {
	t.Helper()
	pub := strings.ReplaceAll(uniqueSlug(tag), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pub, Slug: "labelmaker",
		DisplayName: "Label Maker", Description: "B6.1 upgrade test",
		Author: "ACME", License: "MIT", Homepage: "https://acme.example/lm",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}

	fromManifest := minimalRuntimeManifestVersioned(ext, "1.0.0", "")
	fromVer, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: fromManifest,
		BundleHash: strings.Repeat("a", 64),
		BundleSize: 4096,
		BundleURL:  "https://cdn.example/bundles/v1.0.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion v1.0: %v", err)
	}
	approveListedVersion(ctx, t, store, ext, fromVer)

	toManifest := minimalRuntimeManifestVersioned(ext, "1.1.0", "v2")
	toVer, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: toManifest,
		BundleHash: strings.Repeat("b", 64),
		BundleSize: 4096,
		BundleURL:  "https://cdn.example/bundles/v1.1.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion v1.1: %v", err)
	}
	approveListedVersion(ctx, t, store, ext, toVer)

	return ext,
		fromVer, toVer,
		minimalResolvedBundleVersioned(fromManifest, ""),
		minimalResolvedBundleVersioned(toManifest, "v2")
}

// minimalRuntimeManifestVersioned mirrors minimalRuntimeManifest
// but lets the caller set the manifest version + an optional
// resource-name suffix so two manifests on the same publisher/slug
// don't collide on KType / workflow / tool / webhook names.
func minimalRuntimeManifestVersioned(ext *marketplace.Extension, version, suffix string) *marketplace.Manifest {
	m := minimalRuntimeManifest(ext)
	m.Version = version
	if suffix != "" {
		// Append the suffix to the agent-tool endpoint so the
		// resolved-bundle helper can produce a distinct name.
		m.AgentTools[0].Endpoint = "${EXTENSION_WEBHOOK_BASE}/tools/ship_" + suffix
		// Webhook subscriptions are stored by (installation_id,
		// event), so the event name needs to differ between v1.0
		// and v1.1 to exercise the unregister-then-register
		// path in the same tx. Use a suffixed event name on
		// the upgrade target.
		m.WebhooksConsumed[0].Event = "record.created_" + suffix
	}
	return m
}

// minimalResolvedBundleVersioned mirrors minimalResolvedBundle but
// applies an optional suffix to the canonical resource names so
// two bundles on the same publisher/slug have distinct
// KType / workflow / tool names.
func minimalResolvedBundleVersioned(m *marketplace.Manifest, suffix string) *runtime.ResolvedBundle {
	suff := ""
	if suffix != "" {
		suff = "_" + suffix
	}
	ktypeName := fmt.Sprintf("ext.%s.%s_label%s", m.Publisher, m.Slug, suff)
	workflowName := fmt.Sprintf("ext.%s.%s_print%s", m.Publisher, m.Slug, suff)
	toolName := fmt.Sprintf("ext.%s.%s_ship%s", m.Publisher, m.Slug, suff)
	return &runtime.ResolvedBundle{
		Manifest: m,
		KTypes: []runtime.ResolvedKType{{
			Name:       ktypeName,
			Version:    1,
			SchemaJSON: json.RawMessage(`{"type":"object"}`),
		}},
		Workflows: []runtime.ResolvedWorkflow{{
			Name:           workflowName,
			Version:        1,
			DefinitionJSON: json.RawMessage(`{"states":["start","end"]}`),
		}},
		AgentTools: []runtime.ResolvedAgentTool{{
			Name:           toolName,
			DescriptorJSON: json.RawMessage(`{"name":"ship","args":{}}`),
		}},
	}
}

// approveListedVersion walks the review state machine to
// `approved` and marks the catalog row as 'listed' + the version
// as the listed-version. This is the same shape the
// marketplace_runtime_test.go end-to-end uses, factored out for
// reuse across the upgrade test cases.
func approveListedVersion(
	ctx context.Context, t *testing.T, store *marketplace.Store,
	ext *marketplace.Extension, ver *marketplace.ExtensionVersion,
) {
	t.Helper()
	rs := store.Reviews()
	reviewer := uuid.New().String()
	for _, step := range []marketplace.ReviewStatus{
		marketplace.ReviewStatusAutomatedPassed,
		marketplace.ReviewStatusManualReview,
		marketplace.ReviewStatusApproved,
	} {
		if _, err := rs.UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
			VersionID: ver.ID, Status: step, Reviewer: reviewer,
		}); err != nil {
			t.Fatalf("approveListedVersion %s: %v", step, err)
		}
	}
	if err := store.SetListedVersion(ctx, ext.ID, ver.Version); err != nil {
		t.Fatalf("SetListedVersion %s: %v", ver.Version, err)
	}
	if err := store.UpdateExtensionStatus(ctx, ext.ID, marketplace.ExtensionStatusListed); err != nil {
		t.Fatalf("UpdateExtensionStatus listed: %v", err)
	}
}

// readSingleKTypeName reads the (sole) KType row registered against
// installID and returns its `name`. Fails the test if not exactly
// one row exists (the upgrade test expects a 1-KType bundle).
func readSingleKTypeName(ctx context.Context, t *testing.T, h *harness, tenantID, installID uuid.UUID) string {
	t.Helper()
	var name string
	if err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT ktype_name FROM marketplace_extension_ktypes WHERE installation_id = $1 ORDER BY ktype_name`,
			installID)
		if err != nil {
			return fmt.Errorf("select ktypes: %w", err)
		}
		defer rows.Close()
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return fmt.Errorf("scan ktype name: %w", err)
			}
			names = append(names, n)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("ktype rows: %w", err)
		}
		if len(names) != 1 {
			return fmt.Errorf("expected exactly 1 ktype, got %d: %v", len(names), names)
		}
		name = names[0]
		return nil
	}); err != nil {
		t.Fatalf("readSingleKTypeName: %v", err)
	}
	return name
}
