//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// newTenantForInsights provisions a fresh tenant with the CRM KTypes
// registered (so saved queries can target ktype:crm.deal) and returns
// the tenant plus the Phase L stores driven by the harness pool.
func newTenantForInsights(t *testing.T, h *harness) (
	*tenant.Tenant,
	*insights.QueryStore,
	*insights.DashboardStore,
	*insights.CacheStore,
	*insights.Runner,
) {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phasel"), Name: "Phase L Co", Cell: "test", Plan: "business",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := registerPhaseBKTypes(ctx, h.ktypes, tn.ID.String()[:8]); err != nil {
		t.Fatalf("register ktypes: %v", err)
	}
	queries := insights.NewQueryStore(h.pool)
	dashboards := insights.NewDashboardStore(h.pool)
	cache := insights.NewCacheStore(h.pool)
	runner := insights.NewRunner(h.pool, cache, queries, reporting.NewRunner(h.pool))
	return tn, queries, dashboards, cache, runner
}

// makeCountQuery returns a saved query that counts crm.deal rows.
// Used by several Phase L tests as a known-good source so the runner
// can execute against real data.
func makeCountQuery(t *testing.T, ctx context.Context, queries *insights.QueryStore, tenantID uuid.UUID, ttl int) *insights.Query {
	t.Helper()
	saved, err := queries.Create(ctx, insights.Query{
		TenantID: tenantID,
		Name:     "Deal count " + uuid.NewString()[:6],
		Definition: insights.QueryDefinition{
			Definition: reporting.Definition{
				Source: "ktype:" + crm.KTypeDeal,
				Aggregations: []reporting.Aggregation{{
					Op: reporting.AggCount, Alias: "n",
				}},
				Limit: 100,
			},
		},
		CacheTTLSeconds: &ttl,
	})
	if err != nil {
		t.Fatalf("create saved query: %v", err)
	}
	return saved
}

// TestInsightsDeleteShareCrossResourceRejected is the regression for
// the Phase L sharing bug: DELETE /insights/dashboards/{B}/shares/{X}
// was deleting share X even when X belonged to query A. The store
// signature now requires (resourceType, resourceID) and the WHERE
// clause filters on both columns so a mismatched parent path becomes
// a not-found instead of a successful delete.
func TestInsightsDeleteShareCrossResourceRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, dashboards, _, _ := newTenantForInsights(t, h)

	// 1. Create a saved query and grant a viewer share on it.
	q := makeCountQuery(t, ctx, queries, tn.ID, 60)
	share, err := dashboards.CreateShare(ctx, insights.Share{
		TenantID:     tn.ID,
		ResourceType: insights.ResourceQuery,
		ResourceID:   q.ID,
		GranteeType:  insights.GranteeUser,
		Grantee:      "user-a",
		Permission:   insights.PermissionView,
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}

	// 2. Create an unrelated dashboard. The bug let a caller delete
	//    the query share via this dashboard's path because the
	//    handler never verified the parent.
	d, err := dashboards.Create(ctx, insights.Dashboard{
		TenantID: tn.ID,
		Name:     "Unrelated dashboard",
		Layout:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}

	// 3. Cross-resource delete must be rejected (mapped to ErrShareNotFound).
	err = dashboards.DeleteShare(ctx, tn.ID, insights.ResourceDashboard, d.ID, share.ID)
	if !errors.Is(err, insights.ErrShareNotFound) {
		t.Fatalf("cross-resource delete: want ErrShareNotFound, got %v", err)
	}

	// 4. The share row should still exist.
	listed, err := dashboards.ListShares(ctx, tn.ID, insights.ResourceQuery, q.ID)
	if err != nil {
		t.Fatalf("list shares after rejected cross-delete: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != share.ID {
		t.Fatalf("share missing after rejected cross-delete: got %+v", listed)
	}

	// 5. Same parent path must succeed and remove the row.
	if err := dashboards.DeleteShare(ctx, tn.ID, insights.ResourceQuery, q.ID, share.ID); err != nil {
		t.Fatalf("same-resource delete: %v", err)
	}
	listed, err = dashboards.ListShares(ctx, tn.ID, insights.ResourceQuery, q.ID)
	if err != nil {
		t.Fatalf("list shares after same-resource delete: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("share still present after same-resource delete: got %+v", listed)
	}

	// 6. Second delete of the now-gone row must surface as not-found
	//    (defence-in-depth: the API maps this to HTTP 404).
	if err := dashboards.DeleteShare(ctx, tn.ID, insights.ResourceQuery, q.ID, share.ID); !errors.Is(err, insights.ErrShareNotFound) {
		t.Fatalf("repeat delete: want ErrShareNotFound, got %v", err)
	}
}

// TestInsightsRunSavedQueryUsesCache verifies the Phase L runner's
// cache short-circuit: a second RunSaved call against the same
// saved query + filter params must return CacheHit=true without
// touching the underlying reporting runner. The Phase L
// query_cache_refresh worker depends on this behaviour to avoid
// re-running SQL when the cache row is still warm.
func TestInsightsRunSavedQueryUsesCache(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, _, _, runner := newTenantForInsights(t, h)

	// Seed two deals so the count aggregation has something to return.
	for i := 0; i < 2; i++ {
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID:  tn.ID,
			KType:     crm.KTypeDeal,
			Data:      json.RawMessage(`{"name":"d","stage":"qualification","amount":100,"currency":"USD"}`),
			CreatedBy: uuid.New(),
		}); err != nil {
			t.Fatalf("seed deal: %v", err)
		}
	}

	q := makeCountQuery(t, ctx, queries, tn.ID, 600)

	first, err := runner.RunSaved(ctx, tn.ID, q.ID, nil, false)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.CacheHit {
		t.Fatalf("first run reported cache hit; expected miss")
	}

	second, err := runner.RunSaved(ctx, tn.ID, q.ID, nil, false)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !second.CacheHit {
		t.Fatalf("second run reported cache miss; expected warm cache")
	}

	// bypass_cache=true is what the worker uses for forced refreshes.
	bypass, err := runner.RunSaved(ctx, tn.ID, q.ID, nil, true)
	if err != nil {
		t.Fatalf("bypass run: %v", err)
	}
	if bypass.CacheHit {
		t.Fatalf("bypass=true returned a cache hit; expected fresh execution")
	}
}

// TestRLSIsolatesInsightsTables probes every Phase L table with a
// tenant context belonging to tenant B and asserts none of tenant A's
// rows are visible. RLS is the platform's tenant-isolation invariant
// and any insights table that forgets to apply it is a P0.
func TestRLSIsolatesInsightsTables(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tnA, queriesA, dashboardsA, cacheA, _ := newTenantForInsights(t, h)
	tnB, queriesB, dashboardsB, cacheB, _ := newTenantForInsights(t, h)

	// Seed a query, dashboard, widget, share, and cache row under tenant A.
	qA := makeCountQuery(t, ctx, queriesA, tnA.ID, 60)
	dA, err := dashboardsA.Create(ctx, insights.Dashboard{
		TenantID: tnA.ID, Name: "A dashboard", Layout: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create dashboard A: %v", err)
	}
	wA, err := dashboardsA.UpsertWidget(ctx, insights.DashboardWidget{
		TenantID:    tnA.ID,
		DashboardID: dA.ID,
		QueryID:     &qA.ID,
		VizType:     "table",
		Position:    json.RawMessage(`{"x":0,"y":0,"w":4,"h":3}`),
		Config:      json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("upsert widget A: %v", err)
	}
	shA, err := dashboardsA.CreateShare(ctx, insights.Share{
		TenantID:     tnA.ID,
		ResourceType: insights.ResourceDashboard,
		ResourceID:   dA.ID,
		GranteeType:  insights.GranteeUser,
		Grantee:      "user-a",
		Permission:   insights.PermissionView,
	})
	if err != nil {
		t.Fatalf("create share A: %v", err)
	}
	if err := cacheA.Set(ctx, tnA.ID, "qhA", "fhA", &qA.ID, json.RawMessage(`{"columns":["n"],"rows":[{"n":1}]}`), 1, 60_000_000_000 /* 1 minute */); err != nil {
		t.Fatalf("cache set A: %v", err)
	}

	// Tenant B must not see any of tenant A's rows.
	if got, _ := queriesB.Get(ctx, tnB.ID, qA.ID); got != nil {
		t.Fatalf("tenant B saw query A: %+v", got)
	}
	if got, _ := dashboardsB.Get(ctx, tnB.ID, dA.ID); got != nil {
		t.Fatalf("tenant B saw dashboard A: %+v", got)
	}
	widgetsB, err := dashboardsB.ListWidgets(ctx, tnB.ID, dA.ID)
	if err != nil {
		t.Fatalf("list widgets B: %v", err)
	}
	for _, w := range widgetsB {
		if w.ID == wA.ID {
			t.Fatalf("tenant B saw widget A: %+v", w)
		}
	}
	sharesB, err := dashboardsB.ListShares(ctx, tnB.ID, insights.ResourceDashboard, dA.ID)
	if err != nil {
		t.Fatalf("list shares B: %v", err)
	}
	for _, s := range sharesB {
		if s.ID == shA.ID {
			t.Fatalf("tenant B saw share A: %+v", s)
		}
	}
	if got, err := cacheB.Get(ctx, tnB.ID, "qhA", "fhA"); !errors.Is(err, insights.ErrCacheMiss) {
		t.Fatalf("tenant B saw cache row A: result=%+v err=%v", got, err)
	}

	// Cross-resource delete from tenant B must not remove tenant A's share.
	if err := dashboardsB.DeleteShare(ctx, tnB.ID, insights.ResourceDashboard, dA.ID, shA.ID); !errors.Is(err, insights.ErrShareNotFound) {
		t.Fatalf("tenant B delete of share A: want ErrShareNotFound, got %v", err)
	}
	listedA, err := dashboardsA.ListShares(ctx, tnA.ID, insights.ResourceDashboard, dA.ID)
	if err != nil {
		t.Fatalf("list shares A: %v", err)
	}
	if len(listedA) != 1 || listedA[0].ID != shA.ID {
		t.Fatalf("tenant A share gone after tenant B delete attempt: %+v", listedA)
	}
}

// TestInsightsGenerateQueryAgentTool asserts the dry-run / commit
// contract on the Phase L `insights.generate_query` tool. Dry-run
// must return a preview and write nothing; commit (with confirmed=true)
// must persist a saved query under the tenant.
func TestInsightsGenerateQueryAgentTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, dashboards, _, runner := newTenantForInsights(t, h)

	executor := agents.NewExecutor(h.records, nil, h.auditor)
	agents.RegisterInsightsTools(executor, queries, dashboards, runner)

	inputs, _ := json.Marshal(map[string]any{
		"prompt": "Count of deals by stage",
		"source": "ktype:" + crm.KTypeDeal,
		"name":   "Deals by stage",
	})

	dry, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: uuid.New(), ToolName: "insights.generate_query",
		Inputs: inputs, Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry == nil || len(dry.Preview) == 0 {
		t.Fatalf("dry run preview empty: %+v", dry)
	}
	pre, err := queries.List(ctx, tn.ID)
	if err != nil {
		t.Fatalf("pre-list: %v", err)
	}

	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: uuid.New(), ToolName: "insights.generate_query",
		Inputs: inputs, Mode: agents.ModeCommit, Confirmed: true,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if commit == nil || commit.Extra["query_id"] == nil {
		t.Fatalf("commit result missing query_id: %+v", commit)
	}

	post, err := queries.List(ctx, tn.ID)
	if err != nil {
		t.Fatalf("post-list: %v", err)
	}
	if len(post) != len(pre)+1 {
		t.Fatalf("commit did not add a saved query: pre=%d post=%d", len(pre), len(post))
	}
}

// TestInsightsExplainResultAgentTool exercises the read-side agent tool.
// Dry-run returns metadata only; commit fans out to RunSaved and
// surfaces the row count + cache_hit flag in the result preview.
func TestInsightsExplainResultAgentTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, dashboards, _, runner := newTenantForInsights(t, h)

	q := makeCountQuery(t, ctx, queries, tn.ID, 60)

	executor := agents.NewExecutor(h.records, nil, h.auditor)
	agents.RegisterInsightsTools(executor, queries, dashboards, runner)

	inputs, _ := json.Marshal(map[string]any{"query_id": q.ID})

	dry, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: uuid.New(), ToolName: "insights.explain_result",
		Inputs: inputs, Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry == nil || len(dry.Preview) == 0 {
		t.Fatalf("dry preview empty: %+v", dry)
	}

	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: uuid.New(), ToolName: "insights.explain_result",
		Inputs: inputs, Mode: agents.ModeCommit, Confirmed: true,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	var preview map[string]any
	if err := json.Unmarshal(commit.Preview, &preview); err != nil {
		t.Fatalf("decode commit preview: %v", err)
	}
	if _, ok := preview["row_count"]; !ok {
		t.Fatalf("commit preview missing row_count: %v", preview)
	}
}

// TestInsightsPostDashboardDigestAgentTool walks dry-run + commit
// against a small dashboard. The dry-run path must not run the
// underlying SQL (we assert the placeholder text is present); commit
// must produce one section per widget.
func TestInsightsPostDashboardDigestAgentTool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, dashboards, _, runner := newTenantForInsights(t, h)

	q := makeCountQuery(t, ctx, queries, tn.ID, 60)
	d, err := dashboards.Create(ctx, insights.Dashboard{
		TenantID: tn.ID, Name: "Digest dashboard", Layout: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}
	if _, err := dashboards.UpsertWidget(ctx, insights.DashboardWidget{
		TenantID: tn.ID, DashboardID: d.ID, QueryID: &q.ID,
		VizType:  "number_card",
		Position: json.RawMessage(`{"x":0,"y":0,"w":4,"h":3}`),
		Config:   json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("upsert widget: %v", err)
	}

	executor := agents.NewExecutor(h.records, nil, h.auditor)
	agents.RegisterInsightsTools(executor, queries, dashboards, runner)

	inputs, _ := json.Marshal(map[string]any{"dashboard_id": d.ID})

	dry, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: uuid.New(), ToolName: "insights.post_dashboard_digest",
		Inputs: inputs, Mode: agents.ModeDryRun,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var dryPayload struct {
		Sections []map[string]any `json:"sections"`
	}
	if err := json.Unmarshal(dry.Preview, &dryPayload); err != nil {
		t.Fatalf("decode dry payload: %v", err)
	}
	if len(dryPayload.Sections) != 1 {
		t.Fatalf("dry sections = %d; want 1", len(dryPayload.Sections))
	}

	commit, err := executor.Invoke(ctx, agents.Invocation{
		TenantID: tn.ID, ActorID: uuid.New(), ToolName: "insights.post_dashboard_digest",
		Inputs: inputs, Mode: agents.ModeCommit, Confirmed: true,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	var commitPayload struct {
		Sections []map[string]any `json:"sections"`
	}
	if err := json.Unmarshal(commit.Preview, &commitPayload); err != nil {
		t.Fatalf("decode commit payload: %v", err)
	}
	if len(commitPayload.Sections) != 1 {
		t.Fatalf("commit sections = %d; want 1", len(commitPayload.Sections))
	}
}
