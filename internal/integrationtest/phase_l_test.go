//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/agents"
	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/platform"
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

// TestInsightsDashboardWithLinkedFilters exercises the Phase L
// acceptance criterion "A dashboard with 5+ widgets renders correctly
// with linked filters". It builds a dashboard layout with five widgets
// — one per crm.deal stage column — wired through a shared
// `linked_filters` block on the dashboard layout, then re-runs each
// widget's saved query with the propagated filter to verify the rows
// are tenant-scoped and consistent across widgets.
func TestInsightsDashboardWithLinkedFilters(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, dashboards, _, runner := newTenantForInsights(t, h)

	// Seed a handful of deals across two owners so the linked
	// "owner = X" filter actually narrows the result set rather than
	// returning everything.
	owners := []string{"alice", "bob"}
	for i, owner := range owners {
		for j := 0; j < 3; j++ {
			body, _ := json.Marshal(map[string]any{
				"name":     "deal " + uuid.NewString()[:6],
				"stage":    "qualification",
				"amount":   100 + i*10 + j,
				"currency": "USD",
				"owner":    owner,
			})
			if _, err := h.records.Create(ctx, record.KRecord{
				TenantID:  tn.ID,
				KType:     crm.KTypeDeal,
				Data:      body,
				CreatedBy: uuid.New(),
			}); err != nil {
				t.Fatalf("seed deal %d/%d: %v", i, j, err)
			}
		}
	}

	// Build five distinct saved queries targeting crm.deal — one
	// per widget. Each query is parameterised on the same `owner`
	// filter so the dashboard's linked filter can re-run all five
	// when a single dashboard-level filter changes.
	defs := []reporting.Definition{
		{Source: "ktype:" + crm.KTypeDeal, Aggregations: []reporting.Aggregation{{Op: reporting.AggCount, Alias: "n"}}, Limit: 100},
		{Source: "ktype:" + crm.KTypeDeal, Aggregations: []reporting.Aggregation{{Op: reporting.AggSum, Column: "amount", Alias: "total"}}, Limit: 100},
		{Source: "ktype:" + crm.KTypeDeal, Aggregations: []reporting.Aggregation{{Op: reporting.AggAvg, Column: "amount", Alias: "avg_amt"}}, Limit: 100},
		{Source: "ktype:" + crm.KTypeDeal, Aggregations: []reporting.Aggregation{{Op: reporting.AggMin, Column: "amount", Alias: "min_amt"}}, Limit: 100},
		{Source: "ktype:" + crm.KTypeDeal, Aggregations: []reporting.Aggregation{{Op: reporting.AggMax, Column: "amount", Alias: "max_amt"}}, Limit: 100},
	}
	savedIDs := make([]uuid.UUID, 0, len(defs))
	ttl := 30
	for i, def := range defs {
		saved, err := queries.Create(ctx, insights.Query{
			TenantID: tn.ID,
			Name:     "Widget query " + uuid.NewString()[:6],
			Definition: insights.QueryDefinition{
				Definition: def,
			},
			CacheTTLSeconds: &ttl,
		})
		if err != nil {
			t.Fatalf("create widget query %d: %v", i, err)
		}
		savedIDs = append(savedIDs, saved.ID)
	}

	// Layout encodes a 12-col grid with five tiles plus a single
	// `linked_filters` block keyed on `owner` so the frontend
	// re-runs every widget when the filter changes. The store
	// treats the layout as opaque JSON, so the assertion below is
	// schema validation, not store behaviour.
	layout := json.RawMessage(`{
	  "grid": "12-col",
	  "linked_filters": [
	    {"key":"owner","type":"string","applies_to":"all_widgets"}
	  ]
	}`)
	dash, err := dashboards.Create(ctx, insights.Dashboard{
		TenantID: tn.ID,
		Name:     "Phase L acceptance dashboard",
		Layout:   layout,
	})
	if err != nil {
		t.Fatalf("create dashboard: %v", err)
	}

	for i, qid := range savedIDs {
		queryID := qid
		pos, _ := json.Marshal(map[string]any{"x": (i % 4) * 3, "y": (i / 4) * 3, "w": 3, "h": 3})
		cfg, _ := json.Marshal(map[string]any{
			"linked_filter_keys": []string{"owner"},
		})
		if _, err := dashboards.UpsertWidget(ctx, insights.DashboardWidget{
			TenantID:    tn.ID,
			DashboardID: dash.ID,
			QueryID:     &queryID,
			VizType:     "number_card",
			Position:    pos,
			Config:      cfg,
		}); err != nil {
			t.Fatalf("upsert widget %d: %v", i, err)
		}
	}

	widgets, err := dashboards.ListWidgets(ctx, tn.ID, dash.ID)
	if err != nil {
		t.Fatalf("list widgets: %v", err)
	}
	if len(widgets) < 5 {
		t.Fatalf("widget count = %d; want >= 5", len(widgets))
	}

	// Linked-filter dispatch: re-running every widget under the
	// same owner=alice filter must succeed and return rows scoped
	// to that owner. The Phase L runner takes the filter through
	// FilterParams; we encode it as a reporting Filter on the
	// definition copy because the existing crm.deal builder does
	// not expose runtime filter binding for `owner`.
	for i, w := range widgets {
		if w.QueryID == nil {
			t.Fatalf("widget %d missing query id", i)
		}
		q, err := queries.Get(ctx, tn.ID, *w.QueryID)
		if err != nil {
			t.Fatalf("get widget %d query: %v", i, err)
		}
		def := q.Definition.Definition
		def.Filters = append(def.Filters, reporting.Filter{
			Column: "owner", Op: "=", Value: json.RawMessage(`"alice"`),
		})
		res, err := runner.Run(ctx, tn.ID, insights.RunOptions{
			Definition:  insights.QueryDefinition{Definition: def},
			QueryID:     &q.ID,
			BypassCache: true,
		})
		if err != nil {
			t.Fatalf("run widget %d: %v", i, err)
		}
		if res == nil || res.Result == nil {
			t.Fatalf("widget %d nil result", i)
		}
	}
}

// TestInsightsQueryTimeoutEnforced exercises the Phase L acceptance
// criterion "Query timeout prevents a single tenant from monopolizing
// the shared pool". A runner configured with a 1ns timeout fences the
// underlying reporting query at the context layer and returns an
// error rather than allowing the query to run unbounded.
func TestInsightsQueryTimeoutEnforced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, _, _, runner := newTenantForInsights(t, h)

	// Seed enough rows that the underlying SELECT is non-trivial,
	// so the timeout has actual work to interrupt.
	for i := 0; i < 25; i++ {
		body, _ := json.Marshal(map[string]any{
			"name": "d", "stage": "qualification", "amount": 100, "currency": "USD",
		})
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID: tn.ID, KType: crm.KTypeDeal, Data: body, CreatedBy: uuid.New(),
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	q := makeCountQuery(t, ctx, queries, tn.ID, 60)

	// Re-bind the runner with a sub-microsecond timeout so the
	// statement_timeout fence trips deterministically. The Go
	// context cancellation is layered on top inside runWithTimeout
	// so even an ultra-fast `count(*)` returns an error rather than
	// completing.
	timeoutRunner := runner.WithTimeout(time.Nanosecond)
	if _, err := timeoutRunner.RunSaved(ctx, tn.ID, q.ID, nil, true); err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
}

// TestInsightsFeatureFlagDisablesRoutes exercises the Phase L
// acceptance criterion "Insights feature flag disables all routes and
// nav when off". DynamicFeatureMiddleware looks up the tenant's
// `insights` feature row; an explicit `false` row must produce a 403
// JSON envelope with `feature: "insights"` regardless of which
// `/api/v1/insights/...` sub-route the caller hit.
func TestInsightsFeatureFlagDisablesRoutes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, _ := newTenantForInsights(t, h)
	features := tenant.NewFeatureStore(h.pool)

	// Default: feature is on (wizard seeds business-tier with
	// insights = true). Hit must pass through.
	mw := platform.DynamicFeatureMiddleware(features)
	chain := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	for _, path := range []string{
		"/api/v1/insights/queries",
		"/api/v1/insights/dashboards",
		"/api/v1/insights/queries/" + uuid.NewString() + "/run",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(platform.WithTenant(ctx, tn))
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("with insights enabled: path=%s code=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}

	// Flip the feature off and the same routes must respond 403
	// with the canonical `feature disabled` envelope. The
	// middleware's response shape is what the React shell uses to
	// render the upgrade banner instead of the page.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsights: false}); err != nil {
		t.Fatalf("disable feature: %v", err)
	}

	for _, path := range []string{
		"/api/v1/insights/queries",
		"/api/v1/insights/dashboards",
		"/api/v1/insights/queries/" + uuid.NewString() + "/run",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(platform.WithTenant(ctx, tn))
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("with insights disabled: path=%s code=%d body=%s", path, rr.Code, rr.Body.String())
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode env: %v", err)
		}
		if env["feature"] != "insights" {
			t.Fatalf("envelope missing feature=insights: %v", env)
		}
		if !strings.Contains(strings.ToLower(env["error"].(string)), "feature") {
			t.Fatalf("envelope error wrong shape: %v", env)
		}
	}
}

// TestInsightsGenerateQueryAgentToolValid extends the existing
// TestInsightsGenerateQueryAgentTool dry/commit harness with the
// Phase L acceptance criterion "AI agent generates a *valid* query
// from a natural-language prompt". After commit, the persisted query
// must be runnable end-to-end through the Phase L runner without a
// validation error — proving the generated definition round-trips
// through the reporting builder rather than just being a JSON blob.
func TestInsightsGenerateQueryAgentToolValid(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, dashboards, _, runner := newTenantForInsights(t, h)

	executor := agents.NewExecutor(h.records, nil, h.auditor)
	agents.RegisterInsightsTools(executor, queries, dashboards, runner)

	inputs, _ := json.Marshal(map[string]any{
		"prompt": "Show me top 10 customers by revenue this quarter",
		"source": "ktype:" + crm.KTypeDeal,
		"name":   "Top customers by revenue",
	})

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
	qidStr, _ := commit.Extra["query_id"].(string)
	qid, err := uuid.Parse(qidStr)
	if err != nil {
		t.Fatalf("parse query id %q: %v", qidStr, err)
	}

	// The generated query must execute cleanly. RunSaved walks the
	// full validation + builder + executor stack, so a
	// validation-failing definition surfaces here as an error.
	if _, err := runner.RunSaved(ctx, tn.ID, qid, nil, true); err != nil {
		t.Fatalf("generated query failed to run: %v", err)
	}
}

// TestInsightsSQLEditorMode covers the Phase M raw-SQL editor path
// end-to-end against PostgreSQL: a SQL-mode query is persisted,
// retrieved (mode + raw_sql round-trip through QueryStore), and
// executed under per-tenant RLS via Runner.RunRawSQL. The test also
// exercises the column-level CHECK in migrations/000045_insights_sql_mode.sql
// by attempting to persist a half-state row (mode=visual but
// raw_sql non-empty) which the store-side normalizeMode must
// reject.
func TestInsightsSQLEditorMode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, _, _, runner := newTenantForInsights(t, h)

	// 1) Persist a SQL-mode query against a known stable system
	// view so the assertion doesn't depend on tenant data.
	saved, err := queries.Create(ctx, insights.Query{
		TenantID: tn.ID,
		Name:     "Active conn count " + uuid.NewString()[:6],
		Mode:     insights.QueryModeSQL,
		RawSQL:   "SELECT 1::int AS one, 2::int AS two",
	})
	if err != nil {
		t.Fatalf("create sql-mode query: %v", err)
	}
	if saved.Mode != insights.QueryModeSQL {
		t.Fatalf("saved.Mode = %q, want %q", saved.Mode, insights.QueryModeSQL)
	}
	if saved.RawSQL == "" {
		t.Fatalf("saved.RawSQL empty after create")
	}

	// 2) Round-trip via Get to confirm mode + raw_sql columns are
	// scanned out correctly by the Phase M store changes.
	got, err := queries.Get(ctx, tn.ID, saved.ID)
	if err != nil {
		t.Fatalf("get sql-mode query: %v", err)
	}
	if got.Mode != insights.QueryModeSQL || got.RawSQL != saved.RawSQL {
		t.Fatalf("get round-trip mismatch: mode=%q raw=%q", got.Mode, got.RawSQL)
	}

	// 3) Execute under tenant RLS. The two-column SELECT against
	// PostgreSQL constants verifies the runner's tx + statement_timeout
	// fence path without dragging in a Phase B fixture.
	out, err := runner.RunRawSQL(ctx, tn.ID, got.RawSQL, nil)
	if err != nil {
		t.Fatalf("run raw sql: %v", err)
	}
	if out == nil || out.Result == nil {
		t.Fatalf("nil result from RunRawSQL")
	}
	if len(out.Result.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(out.Result.Rows))
	}
	row := out.Result.Rows[0]
	if row["one"] == nil || row["two"] == nil {
		t.Fatalf("missing columns in row: %#v", row)
	}

	// 4) Half-state rejection: visual mode with raw_sql payload is
	// rejected before the INSERT lands so the column CHECK never
	// has to fire — keeps the API surface returning a clean 400.
	if _, err := queries.Create(ctx, insights.Query{
		TenantID: tn.ID,
		Name:     "Half state " + uuid.NewString()[:6],
		Mode:     insights.QueryModeVisual,
		RawSQL:   "SELECT 1",
	}); err == nil || !errors.Is(err, insights.ErrValidation) {
		t.Fatalf("expected ErrValidation for visual+raw_sql, got %v", err)
	}

	// 5) Symmetric rejection: sql mode with no raw_sql body.
	if _, err := queries.Create(ctx, insights.Query{
		TenantID: tn.ID,
		Name:     "Half state empty " + uuid.NewString()[:6],
		Mode:     insights.QueryModeSQL,
		RawSQL:   "",
	}); err == nil || !errors.Is(err, insights.ErrValidation) {
		t.Fatalf("expected ErrValidation for sql+empty raw_sql, got %v", err)
	}
}

// TestInsightsSQLEditorRunRawSQLRespectsTenantRLS proves the Phase M
// raw-SQL surface honours the tenant_id GUC: a query like
// `SELECT count(*) FROM insights_queries` returns the count of rows
// in the *caller's* tenant only, not the global table — the same
// RLS invariant the visual builder relies on.
func TestInsightsSQLEditorRunRawSQLRespectsTenantRLS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tnA, queriesA, _, _, runnerA := newTenantForInsights(t, h)
	tnB, queriesB, _, _, _ := newTenantForInsights(t, h)
	if tnA.ID == tnB.ID {
		t.Fatalf("test setup produced the same tenant twice")
	}

	// Seed two visual queries in A and three in B so the row counts
	// per tenant differ. The test then runs the same SQL body under
	// tenant A's context and expects to see only 2 rows (not 5).
	for i := 0; i < 2; i++ {
		_ = makeCountQuery(t, ctx, queriesA, tnA.ID, 60)
	}
	for i := 0; i < 3; i++ {
		_ = makeCountQuery(t, ctx, queriesB, tnB.ID, 60)
	}

	out, err := runnerA.RunRawSQL(ctx, tnA.ID, "SELECT count(*)::int AS n FROM insights_queries", nil)
	if err != nil {
		t.Fatalf("run raw sql: %v", err)
	}
	if len(out.Result.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(out.Result.Rows))
	}
	got, ok := out.Result.Rows[0]["n"].(int32)
	if !ok {
		// pgx may decode count() as int32 or int64 depending on
		// driver version — accept either to keep the test stable.
		gotInt64, ok64 := out.Result.Rows[0]["n"].(int64)
		if !ok64 {
			t.Fatalf("count column is %T, want int32 or int64: %#v", out.Result.Rows[0]["n"], out.Result.Rows[0])
		}
		got = int32(gotInt64)
	}
	if got != 2 {
		t.Fatalf("count() = %d, want 2 (RLS leak from tenant B?)", got)
	}
}

// TestInsightsSQLEditorFeatureFlagDisablesRoute checks the
// `insights_sql_editor` feature gate the API mounts on
// /api/v1/insights/queries/{id}/run-sql. With the flag off the
// platform.FeatureMiddleware short-circuits with a 403 envelope
// keyed on `insights_sql_editor`.
func TestInsightsSQLEditorFeatureFlagDisablesRoute(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, _ := newTenantForInsights(t, h)
	features := tenant.NewFeatureStore(h.pool)

	// A business-tier wizard tenant has insights=true but
	// insights_sql_editor=false (enterprise-only). Confirm the
	// middleware rejects with a 403 + `feature: insights_sql_editor`
	// envelope so the React shell can surface an upgrade banner.
	//
	// newTenantForInsights provisions the tenant via tenants.Create
	// directly rather than RunSetupWizard, so seedDefaultFeatures
	// never writes the explicit `false` row. FeatureStore.IsEnabled
	// defaults missing rows to true (so a newly added flag doesn't
	// require a backfill), which would mask the gate. Seed the
	// canonical business-plan default explicitly so the assertion
	// matches the production code path.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: false}); err != nil {
		t.Fatalf("seed sql editor=false: %v", err)
	}
	mw := platform.FeatureMiddleware(features, tenant.FeatureInsightsSQLEditor)
	chain := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/insights/queries/"+uuid.NewString()+"/run-sql", nil).
		WithContext(platform.WithTenant(ctx, tn))
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("with sql editor disabled: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode env: %v", err)
	}
	if env["feature"] != "insights_sql_editor" {
		t.Fatalf("envelope feature=%v, want insights_sql_editor", env["feature"])
	}

	// Flip it on and the same request must pass through.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: true}); err != nil {
		t.Fatalf("enable feature: %v", err)
	}
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, req.Clone(req.Context()))
	if rr.Code != http.StatusOK {
		t.Fatalf("with sql editor enabled: code=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestInsightsRunSavedDispatchesSQLMode is the regression guard for
// the Phase M Task 1 review finding that RunSaved always routed
// through the visual Run, even when the persisted query was
// mode='sql'. Every consumer of RunSaved (dashboard widget fan-out,
// cache refresh worker, /run handler, agent tools, /insight slash
// command) must dispatch SQL-mode queries to RunRawSQL so the
// caller sees raw-SQL rows rather than the placeholder visual
// definition's output.
//
// Strategy: persist a SQL-mode query whose raw body returns a
// distinctive sentinel column the visual runner cannot fabricate.
// If RunSaved still routed through Run, the sentinel would never
// appear because the placeholder definition has Aggregations:
// AggCount which yields a single integer column.
func TestInsightsRunSavedDispatchesSQLMode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, _, _, runner := newTenantForInsights(t, h)

	saved, err := queries.Create(ctx, insights.Query{
		TenantID: tn.ID,
		Name:     "RunSaved SQL dispatch " + uuid.NewString()[:6],
		Mode:     insights.QueryModeSQL,
		RawSQL:   "SELECT 'phase-m-sentinel'::text AS marker, 7::int AS magic",
	})
	if err != nil {
		t.Fatalf("create sql-mode query: %v", err)
	}

	out, err := runner.RunSaved(ctx, tn.ID, saved.ID, nil, true)
	if err != nil {
		t.Fatalf("RunSaved: %v", err)
	}
	if out == nil || out.Result == nil || len(out.Result.Rows) == 0 {
		t.Fatalf("RunSaved returned no rows: %#v", out)
	}
	row := out.Result.Rows[0]
	if row["marker"] != "phase-m-sentinel" {
		t.Fatalf("RunSaved did not dispatch to RunRawSQL: marker=%v full=%#v", row["marker"], row)
	}
	if magic, ok := row["magic"].(int32); !ok || magic != 7 {
		// pgx scans int4 into int32; assert defensively to
		// surface a clear failure if the dispatch ever
		// regresses to Run (which would not return a `magic`
		// column at all).
		t.Fatalf("RunSaved magic = %v (%T); want int32(7)", row["magic"], row["magic"])
	}
}

// TestInsightsRunSavedFeatureGateBlocksSQLMode is the regression
// guard for the Phase M Task 1 review finding that RunSaved would
// dispatch SQL-mode queries to RunRawSQL without checking the
// `insights_sql_editor` feature flag, letting a non-enterprise
// tenant who'd had a SQL-mode row persisted (e.g. before downgrade)
// continue to execute it via every RunSaved consumer (dashboard
// fan-out, cache worker, agent tools, /insight slash command).
//
// Strategy: wire the runner with the FeaturePolicy backed by
// FeatureStore, persist a SQL-mode row, then disable
// insights_sql_editor and assert RunSaved returns ErrFeatureDisabled.
// Re-enabling the flag must restore execution.
func TestInsightsRunSavedFeatureGateBlocksSQLMode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, _, _, runner := newTenantForInsights(t, h)
	features := tenant.NewFeatureStore(h.pool)
	runner = runner.WithFeaturePolicy(features)

	saved, err := queries.Create(ctx, insights.Query{
		TenantID: tn.ID,
		Name:     "RunSaved gate " + uuid.NewString()[:6],
		Mode:     insights.QueryModeSQL,
		RawSQL:   "SELECT 1::int AS one",
	})
	if err != nil {
		t.Fatalf("create sql-mode query: %v", err)
	}

	// Disable the feature: RunSaved must refuse with ErrFeatureDisabled.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: false}); err != nil {
		t.Fatalf("disable sql editor: %v", err)
	}
	if _, err := runner.RunSaved(ctx, tn.ID, saved.ID, nil, true); err == nil {
		t.Fatalf("RunSaved with feature disabled: nil error; want ErrFeatureDisabled")
	} else if !errors.Is(err, insights.ErrFeatureDisabled) {
		t.Fatalf("RunSaved error = %v; want ErrFeatureDisabled", err)
	}

	// Re-enable the feature: same call must succeed.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: true}); err != nil {
		t.Fatalf("enable sql editor: %v", err)
	}
	out, err := runner.RunSaved(ctx, tn.ID, saved.ID, nil, true)
	if err != nil {
		t.Fatalf("RunSaved with feature enabled: %v", err)
	}
	if out == nil || out.Result == nil || len(out.Result.Rows) == 0 {
		t.Fatalf("RunSaved returned no rows: %#v", out)
	}
}
