//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMarketplaceRuntime_EndToEnd is the integration check for
// Phase B3 (marketplace runtime engine, this PR):
//
//  1. Engine.Install populates an install row (status=active) plus
//     the four runtime tables (ktypes / workflows / agent-tools /
//     webhook-subscriptions) inside a single tenant-scoped tx.
//  2. pre_install hook is dispatched BEFORE any insertion. A
//     pre_install reject (non-2xx, non-404) aborts the whole flow
//     with NO partial rows committed.
//  3. post_install hook is dispatched AFTER the tx commits and is
//     best-effort — failures do NOT roll back the install.
//  4. Engine.Uninstall reverses the registration and flips status
//     to 'uninstalled'.
//  5. Cross-tenant RLS isolation on every runtime table.
//  6. Dispatcher.Invoke looks up an installed agent tool, signs the
//     payload, POSTs to the extension webhook, and writes a
//     dispatch_log row. Verified against a fake in-memory transport
//     that asserts header presence.
//  7. Inactive install (status='uninstalled') rejects dispatcher
//     invocations with ErrInstallationNotActive.
func TestMarketplaceRuntime_EndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// --- Seed catalog (extension + version, listed, not yanked) ---
	pub := strings.ReplaceAll(uniqueSlug("rt"), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pub, Slug: "shipping",
		DisplayName: "Shipping", Description: "Runtime test extension",
		Author: "ACME", License: "MIT",
		Homepage: "https://acme.example/shipping",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}
	manifest := minimalRuntimeManifest(ext)
	ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: manifest,
		BundleHash: strings.Repeat("d", 64),
		BundleSize: 4096,
		BundleURL:  "https://cdn.example/bundles/rt.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	// Approve + list. The catalog must be in 'listed' state and the
	// version non-yanked for the engine to permit install. The review
	// state machine forbids submitted→approved in one hop, so walk
	// the transition graph: submitted → automated_passed → manual_review
	// → approved.
	rs := store.Reviews()
	reviewer := uuid.New().String()
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
			t.Fatalf("review transition to %s: %v", step, err)
		}
	}
	if err := store.SetListedVersion(ctx, ext.ID, ver.Version); err != nil {
		t.Fatalf("SetListedVersion: %v", err)
	}
	if err := store.UpdateExtensionStatus(ctx, ext.ID, marketplace.ExtensionStatusListed); err != nil {
		t.Fatalf("UpdateExtensionStatus listed: %v", err)
	}

	// Two tenants for RLS isolation checks.
	tnA, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("rt-a"), Name: "RT-A", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant A: %v", err)
	}
	tnB, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("rt-b"), Name: "RT-B", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant B: %v", err)
	}

	// --- (2) pre_install reject → no rows committed ---
	rejectTransport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(403, []byte(`{"error":"policy denied"}`)),
	}
	rejectHooks := runtime.NewTransportHooks(rejectTransport, nil)
	rejectEngine, err := runtime.NewEngine(runtime.EngineOptions{
		Pool:  h.pool,
		Store: store,
		Hooks: rejectHooks,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	bundle := minimalResolvedBundle(manifest)
	_, err = rejectEngine.Install(ctx, &runtime.InstallRequest{
		TenantID:    tnA.ID,
		ExtensionID: ext.ID,
		VersionID:   ver.ID,
		WebhookBase: "https://tenant-a.example/hooks",
	}, bundle)
	if !errors.Is(err, runtime.ErrPreInstallRejected) {
		t.Fatalf("pre_install 403 should ErrPreInstallRejected; got %v", err)
	}
	// Verify NO install row was written for tnA × ext. Filtering by
	// extension is necessary because on a BYPASSRLS connection
	// ListInstallationsForTenant returns rows from every tenant,
	// not just tnA — RLS is short-circuited on superuser pools.
	listA, err := store.ListInstallationsForTenant(ctx, tnA.ID)
	if err != nil {
		t.Fatalf("ListInstallationsForTenant A: %v", err)
	}
	for _, ins := range listA {
		if ins.TenantID == tnA.ID && ins.ExtensionID == ext.ID {
			t.Fatalf("pre_install reject committed install row %s for tnA×ext; want zero", ins.ID)
		}
	}

	// --- (1)/(3) happy-path install on tenant A ---
	okTransport := &runtime.InMemoryTransport{Handler: runtime.StaticResponseHandler(200, []byte(`{"ok":true}`))}
	okHooks := runtime.NewTransportHooks(okTransport, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{
		Pool:  h.pool,
		Store: store,
		Hooks: okHooks,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	installRes, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID:    tnA.ID,
		ExtensionID: ext.ID,
		VersionID:   ver.ID,
		WebhookBase: "https://tenant-a.example/hooks",
		Settings:    map[string]interface{}{"foo": "bar"},
	}, bundle)
	if err != nil {
		t.Fatalf("Install A: %v", err)
	}
	if installRes.Installation.Status != marketplace.InstallStatusActive {
		t.Fatalf("post-install status %q, want active", installRes.Installation.Status)
	}
	if installRes.SigningSecret == "" {
		t.Fatal("Install did not return signing secret")
	}
	if installRes.PreInstallResult == nil || installRes.PreInstallResult.Aborted {
		t.Fatalf("pre_install result = %+v", installRes.PreInstallResult)
	}
	if installRes.PostInstallResult == nil || installRes.PostInstallResult.Aborted {
		t.Fatalf("post_install result = %+v", installRes.PostInstallResult)
	}
	// 2 hook calls (pre + post) on the OK transport.
	if got := len(okTransport.Audit); got != 2 {
		t.Fatalf("hook dispatches = %d, want 2 (pre + post)", got)
	}

	// Verify runtime tables populated for tnA.
	counts := readRuntimeCounts(ctx, t, h, tnA.ID, installRes.Installation.ID)
	if counts.ktypes != 1 || counts.workflows != 1 || counts.tools != 1 || counts.webhooks != 1 {
		t.Fatalf("runtime row counts = %+v, want 1 of each", counts)
	}

	// --- (5) RLS isolation: tnB pool MUST NOT see tnA runtime rows ---
	rlsEnforced := poolEnforcesRLS(ctx, t, h)
	if rlsEnforced {
		bCounts := readRuntimeCounts(ctx, t, h, tnB.ID, installRes.Installation.ID)
		if bCounts.ktypes != 0 || bCounts.workflows != 0 || bCounts.tools != 0 || bCounts.webhooks != 0 {
			t.Fatalf("RLS leak: tnB sees tnA's runtime rows: %+v", bCounts)
		}
	} else {
		t.Logf("connection bypasses RLS; skipping cross-tenant negative assertions")
	}

	// --- (6) Dispatcher.Invoke ---
	toolName := manifest.AgentTools[0].Definition
	toolName = canonicalToolNameFromDef(toolName, manifest.Publisher, manifest.Slug)
	dispatchTransport := &runtime.InMemoryTransport{
		Handler: runtime.StaticResponseHandler(200, []byte(`{"result":"shipped"}`)),
	}
	disp := runtime.NewDispatcher(h.pool, dispatchTransport, nil)
	invRes, err := disp.Invoke(ctx, &runtime.InvokeRequest{
		TenantID:       tnA.ID,
		InstallationID: installRes.Installation.ID,
		ToolName:       toolName,
		Body:           []byte(`{"order_id":"123"}`),
	})
	if err != nil {
		t.Fatalf("Dispatcher.Invoke: %v", err)
	}
	if invRes.Status != 200 || invRes.Attempt != 1 {
		t.Fatalf("invoke result = %+v, want status=200 attempt=1", invRes)
	}
	if len(dispatchTransport.Audit) != 1 {
		t.Fatalf("dispatcher audit len = %d, want 1", len(dispatchTransport.Audit))
	}
	if dispatchTransport.Audit[0].Headers[runtime.SignatureHeaderName] == "" {
		t.Fatal("dispatcher did not stamp signature header")
	}
	if dispatchTransport.Audit[0].Headers[runtime.TimestampHeaderName] == "" {
		t.Fatal("dispatcher did not stamp timestamp header")
	}
	// Verify dispatch_log row written.
	logCount := readDispatchLogCount(ctx, t, h, tnA.ID, installRes.Installation.ID)
	if logCount < 1 {
		t.Fatalf("dispatch_log row count = %d, want >=1", logCount)
	}

	// --- (4) Uninstall ---
	uninstallRes, err := engine.Uninstall(ctx, &runtime.UninstallRequest{
		TenantID:       tnA.ID,
		InstallationID: installRes.Installation.ID,
	})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if uninstallRes.Installation.Status != marketplace.InstallStatusUninstalled {
		t.Fatalf("post-uninstall status %q, want uninstalled", uninstallRes.Installation.Status)
	}
	// Runtime tables cleared.
	afterCounts := readRuntimeCounts(ctx, t, h, tnA.ID, installRes.Installation.ID)
	if afterCounts.ktypes != 0 || afterCounts.workflows != 0 || afterCounts.tools != 0 || afterCounts.webhooks != 0 {
		t.Fatalf("uninstall left rows: %+v", afterCounts)
	}

	// --- (7) Dispatcher rejects invocations on uninstalled installs ---
	_, err = disp.Invoke(ctx, &runtime.InvokeRequest{
		TenantID:       tnA.ID,
		InstallationID: installRes.Installation.ID,
		ToolName:       toolName,
		Body:           []byte(`{}`),
	})
	if err == nil {
		t.Fatal("dispatch against uninstalled install should fail")
	}
}

// minimalRuntimeManifest returns a small but spec-valid manifest
// with one KType / workflow / agent-tool / webhook so the runtime
// tables each get exactly one INSERT.
func minimalRuntimeManifest(ext *marketplace.Extension) *marketplace.Manifest {
	return &marketplace.Manifest{
		SchemaVersion:    1,
		Name:             ext.Name,
		Publisher:        ext.Publisher,
		Slug:             ext.Slug,
		Version:          "1.0.0",
		Author:           "ACME",
		License:          "MIT",
		Description:      "Runtime test extension",
		MinKappVersion:   "1.0.0",
		FeaturesRequired: []string{"inventory"},
		KTypes:           []marketplace.KTypeRef{{Schema: "./ktypes/label.json"}},
		Workflows:        []marketplace.WorkflowRef{{Definition: "./workflows/print.json"}},
		AgentTools: []marketplace.AgentToolRef{{
			Definition: "./tools/ship.json",
			Handler:    "webhook",
			Endpoint:   "${EXTENSION_WEBHOOK_BASE}/tools/ship",
			Timeout:    "10s",
		}},
		WebhooksConsumed: []marketplace.WebhookRef{{
			Event:    "record.created",
			Endpoint: "${EXTENSION_WEBHOOK_BASE}/webhooks/created",
		}},
	}
}

// minimalResolvedBundle builds the ResolvedBundle the registrar
// expects. Names follow the canonical `ext.<publisher>.<slug>.<...>`
// convention.
func minimalResolvedBundle(m *marketplace.Manifest) *runtime.ResolvedBundle {
	// Per spec §4 KType names are `ext.<publisher>.<slug>` —
	// exactly three dot-segments where the third segment is the
	// KType's local slug within the publisher's namespace. The
	// slug can be anything matching ^[a-z][a-z0-9_]*$.
	ktypeName := fmt.Sprintf("ext.%s.%s_label", m.Publisher, m.Slug)
	workflowName := fmt.Sprintf("ext.%s.%s_print", m.Publisher, m.Slug)
	toolName := fmt.Sprintf("ext.%s.%s_ship", m.Publisher, m.Slug)
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

type runtimeCounts struct{ ktypes, workflows, tools, webhooks int }

func readRuntimeCounts(ctx context.Context, t *testing.T, h *harness, tenantID, installID uuid.UUID) runtimeCounts {
	t.Helper()
	var c runtimeCounts
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantID.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM marketplace_extension_ktypes WHERE installation_id = $1`,
		installID).Scan(&c.ktypes); err != nil {
		t.Fatalf("count ktypes: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM marketplace_extension_workflows WHERE installation_id = $1`,
		installID).Scan(&c.workflows); err != nil {
		t.Fatalf("count workflows: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM marketplace_extension_agent_tools WHERE installation_id = $1`,
		installID).Scan(&c.tools); err != nil {
		t.Fatalf("count tools: %v", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM marketplace_webhook_subscriptions WHERE installation_id = $1`,
		installID).Scan(&c.webhooks); err != nil {
		t.Fatalf("count webhooks: %v", err)
	}
	return c
}

func readDispatchLogCount(ctx context.Context, t *testing.T, h *harness, tenantID, installID uuid.UUID) int {
	t.Helper()
	conn, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT set_config('app.tenant_id', $1, false)`, tenantID.String()); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT COUNT(*) FROM marketplace_dispatch_log WHERE installation_id = $1`,
		installID).Scan(&n); err != nil {
		t.Fatalf("count dispatch_log: %v", err)
	}
	return n
}

// canonicalToolNameFromDef maps the manifest's `definition` path
// (e.g. "./tools/ship.json") to the canonical name used by the
// registrar. The KType-name regex in B2 (extKTypeNameRegex) only
// allows three dot-segments — `ext.<publisher>.<slug>` — so we
// fold the descriptor basename into the third segment with an
// underscore. This matches what minimalResolvedBundle constructs.
func canonicalToolNameFromDef(def, publisher, slug string) string {
	base := strings.TrimSuffix(def, ".json")
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	return fmt.Sprintf("ext.%s.%s_%s", publisher, slug, base)
}
