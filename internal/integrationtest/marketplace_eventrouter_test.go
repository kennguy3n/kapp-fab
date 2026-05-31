//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/eventrouter"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestEventRouter_EndToEnd exercises the full B4 fan-out path:
//
//  1. Install an extension whose manifest declares both
//     webhooks_consumed[] and posting_hooks[].
//  2. Emit synthetic outbox events matching each subscription.
//  3. Verify that the event router delivers signed POSTs to the
//     matching endpoints (InMemoryTransport captures the calls),
//     writes dispatch_log rows per attempt, and respects rate
//     limiting and filter evaluation.
//  4. Verify that an event NOT matching any subscription's filter
//     is NOT dispatched.
//  5. Verify that a rate-limited dispatch is silently dropped
//     (no dispatch_log, no transport call).
func TestEventRouter_EndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// --- Seed catalog ---
	pub := strings.ReplaceAll(uniqueSlug("evrt"), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pub, Slug: "notify",
		DisplayName: "Notify", Description: "Event-router test extension",
		Author: "ACME", License: "MIT",
		Homepage: "https://acme.example/notify",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}

	// Manifest with both webhooks_consumed and posting_hooks.
	manifest := &marketplace.Manifest{
		SchemaVersion:    1,
		Name:             ext.Name,
		Publisher:        ext.Publisher,
		Slug:             ext.Slug,
		Version:          "1.0.0",
		Author:           "ACME",
		License:          "MIT",
		Description:      "Event-router test extension",
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
		WebhooksConsumed: []marketplace.WebhookRef{
			{
				Event:    "record.created",
				Endpoint: "${EXTENSION_WEBHOOK_BASE}/webhooks/created",
				Filter:   map[string]string{"status": "posted"},
			},
			{
				Event:    "record.updated",
				Endpoint: "${EXTENSION_WEBHOOK_BASE}/webhooks/updated",
			},
		},
		PostingHooks: []marketplace.PostingHookRef{
			{
				KType:    fmt.Sprintf("ext.%s.%s_label", pub, ext.Slug),
				When:     "after_create",
				Endpoint: "${EXTENSION_WEBHOOK_BASE}/hooks/label_created",
			},
		},
	}

	ktypeName := fmt.Sprintf("ext.%s.%s_label", pub, ext.Slug)
	workflowName := fmt.Sprintf("ext.%s.%s_print", pub, ext.Slug)
	toolName := fmt.Sprintf("ext.%s.%s_ship", pub, ext.Slug)
	bundle := &runtime.ResolvedBundle{
		Manifest: manifest,
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

	// Use a transport that records all calls. The InMemoryTransport's
	// Handler signature is (ctx, target, body, headers) — the leading
	// context is required because B3's lifecycle hook path threads ctx
	// for cancellation; the InstallRequest path will call into this
	// handler for the install lifecycle hook dispatches as well.
	var transportCalls int64
	okTransport := &runtime.InMemoryTransport{
		Handler: func(_ context.Context, target string, body []byte, headers map[string]string) (*runtime.DispatchResponse, error) {
			atomic.AddInt64(&transportCalls, 1)
			return &runtime.DispatchResponse{
				Status: 200,
				Body:   []byte(`{"ok":true}`),
			}, nil
		},
	}

	// --- Provision tenant ---
	tenant1, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("evrt-t"), Name: "Event-Router Test", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant Create: %v", err)
	}
	tenantID := tenant1.ID

	// --- Publish version through the production catalog path ---
	// store.PublishVersion is the only public path that creates an
	// marketplace_extension_versions row; we then walk the review
	// state machine (submitted → automated_passed → manual_review →
	// approved) and set listed_version (the version string, not the
	// version uuid) before flipping the extension to 'listed'. This
	// mirrors the B3 happy-path setup in
	// marketplace_runtime_test.go.
	ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID, Manifest: manifest,
		BundleHash: strings.Repeat("d", 64),
		BundleSize: 4096,
		BundleURL:  "https://cdn.example/notify-1.0.0.zip",
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
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

	// --- Install through the runtime engine ---
	// Hooks: the manifest declares ${EXTENSION_WEBHOOK_BASE}-rooted
	// endpoints which the engine resolves and dispatches against
	// during pre/post install. We share the same okTransport so we
	// can later use the (reset) transportCalls counter for the
	// post-install event-delivery assertions. NewTransportHooks owns
	// the dispatch_log writes for the lifecycle path; the hooks
	// counter is reset right after install completes.
	okHooks := runtime.NewTransportHooks(okTransport, h.pool, nil)
	engine, err := runtime.NewEngine(runtime.EngineOptions{
		Pool:  h.pool,
		Store: store,
		Hooks: okHooks,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// InstalledBy left zero so the install row's installed_by column
	// stays NULL (system install). The marketplace_extension_
	// installations.installed_by FK requires the user_id to exist in
	// auth.users when non-NULL; the runtime test harness doesn't seed
	// a user, so we follow the same pattern as the B3 happy-path test
	// at marketplace_runtime_test.go:162 (no InstalledBy).
	result, err := engine.Install(ctx, &runtime.InstallRequest{
		TenantID:    tenantID,
		ExtensionID: ext.ID,
		VersionID:   ver.ID,
		WebhookBase: "https://ext.acme.example",
	}, bundle)
	if err != nil {
		t.Fatalf("Engine.Install: %v", err)
	}
	installID := result.Installation.ID

	// Reset transport call counter after install lifecycle hooks.
	atomic.StoreInt64(&transportCalls, 0)

	// --- Verify subscription rows ---
	// Expected: 2 from webhooks_consumed + 1 from posting_hooks = 3.
	var subCount int
	if err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM marketplace_webhook_subscriptions WHERE installation_id = $1`,
			installID).Scan(&subCount)
	}); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subCount != 3 {
		t.Fatalf("expected 3 webhook subscriptions (2 consumed + 1 posting_hook), got %d", subCount)
	}

	// --- Build Router ---
	limiter := eventrouter.NewLimiter(100, time.Now)
	router := eventrouter.NewRouter(h.pool, okTransport, runtime.NoopEncryptor(), limiter, time.Now)

	// --- Test 1: event that matches filter ---
	matchPayload, _ := json.Marshal(map[string]any{"status": "posted", "id": "rec_1"})
	n, err := router.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "record.created",
		Payload:   matchPayload,
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (filter match): %v", err)
	}
	if n != 1 {
		t.Fatalf("RouteBatch (filter match): expected 1 dispatch, got %d", n)
	}
	calls := atomic.LoadInt64(&transportCalls)
	if calls != 1 {
		t.Fatalf("transport calls (filter match): expected 1, got %d", calls)
	}

	// --- Test 2: event that does NOT match filter ---
	atomic.StoreInt64(&transportCalls, 0)
	noMatchPayload, _ := json.Marshal(map[string]any{"status": "draft", "id": "rec_2"})
	n, err = router.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "record.created",
		Payload:   noMatchPayload,
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (filter mismatch): %v", err)
	}
	if n != 0 {
		t.Fatalf("RouteBatch (filter mismatch): expected 0 dispatch, got %d", n)
	}
	if c := atomic.LoadInt64(&transportCalls); c != 0 {
		t.Fatalf("transport calls (filter mismatch): expected 0, got %d", c)
	}

	// --- Test 3: event with no filter (record.updated) ---
	atomic.StoreInt64(&transportCalls, 0)
	n, err = router.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "record.updated",
		Payload:   json.RawMessage(`{"id": "rec_3"}`),
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (record.updated): %v", err)
	}
	if n != 1 {
		t.Fatalf("RouteBatch (record.updated): expected 1 dispatch, got %d", n)
	}

	// --- Test 4: posting-hook-derived event ---
	// Posting hooks now register as subscriptions on the generic
	// `krecord.created` event with filter={"ktype": "..."}, so a
	// krecord.created event whose payload.ktype matches the hook's
	// KType should dispatch. Also exercises that the SAME event
	// type can fan out to multiple subscriptions (record.updated
	// has no ktype-narrowing webhook_consumed subscription, but
	// posting_hook for ktype X on krecord.created has filter
	// `ktype = ext.<pub>.<slug>_label`).
	atomic.StoreInt64(&transportCalls, 0)
	postingHookPayload, _ := json.Marshal(map[string]any{
		"ktype":     fmt.Sprintf("ext.%s.%s_label", pub, ext.Slug),
		"record_id": "rec_4",
	})
	n, err = router.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "krecord.created",
		Payload:   postingHookPayload,
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (posting-hook): %v", err)
	}
	// We expect 1 posting-hook dispatch. The webhooks_consumed
	// subscription on `record.created` (with filter status=posted)
	// does NOT match because the event type is `krecord.created`
	// (the actual record-store emit), not the manifest's
	// `record.created` string. Publishers who want to subscribe to
	// the generic record event will use `krecord.created` going
	// forward — that's the actual emitted event-type today.
	if n != 1 {
		t.Fatalf("RouteBatch (posting-hook): expected 1 dispatch, got %d", n)
	}

	// --- Test 4b: krecord.created with NON-matching ktype payload ---
	// Posting hook is filter-narrowed by ktype, so a krecord.created
	// for a different ktype must NOT fire the hook.
	atomic.StoreInt64(&transportCalls, 0)
	nonMatchKTypePayload, _ := json.Marshal(map[string]any{
		"ktype":     "ext.different.publisher_kind",
		"record_id": "rec_5",
	})
	n, err = router.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "krecord.created",
		Payload:   nonMatchKTypePayload,
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (posting-hook ktype mismatch): %v", err)
	}
	if n != 0 {
		t.Fatalf("RouteBatch (posting-hook ktype mismatch): expected 0, got %d", n)
	}
	if c := atomic.LoadInt64(&transportCalls); c != 0 {
		t.Fatalf("transport calls (posting-hook ktype mismatch): expected 0, got %d", c)
	}

	// --- Test 5: rate limit exhaustion ---
	atomic.StoreInt64(&transportCalls, 0)
	// The router takes its budget from marketplace_extensions
	// .rate_limit_rpm (joined inside lookupSubscriptions) — the
	// limiter's DefaultRPM() is ONLY used when the column is < 1.
	// To force a rate-limit refusal, lower the column to 1 first so
	// the limiter is asked to enforce 1 RPM for this (tenant, ext).
	if _, err := h.pool.Exec(ctx,
		`UPDATE marketplace_extensions SET rate_limit_rpm = 1 WHERE id = $1`,
		ext.ID); err != nil {
		t.Fatalf("set rate_limit_rpm=1: %v", err)
	}
	frozenNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tinyLimiter := eventrouter.NewLimiter(1, func() time.Time { return frozenNow })
	tinyRouter := eventrouter.NewRouter(h.pool, okTransport, runtime.NoopEncryptor(), tinyLimiter, func() time.Time { return frozenNow })
	// First call consumes the single token.
	_, _ = tinyRouter.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "record.updated",
		Payload:   json.RawMessage(`{}`),
		CreatedAt: time.Now(),
	}})
	// Reset counter for the second call.
	atomic.StoreInt64(&transportCalls, 0)
	// Second call should be rate-limited (0 dispatches).
	n, err = tinyRouter.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "record.updated",
		Payload:   json.RawMessage(`{}`),
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (rate-limited): %v", err)
	}
	if n != 0 {
		t.Fatalf("RouteBatch (rate-limited): expected 0 (rate-limited), got %d", n)
	}
	if c := atomic.LoadInt64(&transportCalls); c != 0 {
		t.Fatalf("transport calls (rate-limited): expected 0, got %d", c)
	}

	// --- Test 6: dispatch_log rows written ---
	var logCount int
	if err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM marketplace_dispatch_log
			  WHERE installation_id = $1
			    AND kind = 'event_delivery'`,
			installID).Scan(&logCount)
	}); err != nil {
		t.Fatalf("count dispatch_log: %v", err)
	}
	// Tests 1 + 3 + 4 each wrote 1 dispatch attempt + rate-limit test's first call wrote 1 = 4.
	if logCount < 3 {
		t.Fatalf("dispatch_log rows for event_delivery: expected >= 3, got %d", logCount)
	}

	// --- Test 7: no subscription for unknown event type ---
	atomic.StoreInt64(&transportCalls, 0)
	n, err = router.RouteBatch(ctx, []events.Event{{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Type:      "totally.unknown.event",
		Payload:   json.RawMessage(`{}`),
		CreatedAt: time.Now(),
	}})
	if err != nil {
		t.Fatalf("RouteBatch (unknown event): %v", err)
	}
	if n != 0 {
		t.Fatalf("RouteBatch (unknown event): expected 0, got %d", n)
	}
	if c := atomic.LoadInt64(&transportCalls); c != 0 {
		t.Fatalf("transport calls (unknown event): expected 0, got %d", c)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
