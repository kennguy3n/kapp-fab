//go:build integration
// +build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestInsightsCreateUpdateQueryRoundTripsSQLMode is the regression
// guard for the Phase M Task 1 review finding that the
// insightsQueryRequest struct dropped `mode` and `raw_sql` on the
// way through `json.Decoder.Decode`. The original integration test
// for the SQL editor mode exercised the store directly, so a missing
// field on the request struct went unnoticed: every saved query
// landed as `mode='visual'` regardless of the payload.
//
// This test mounts the real createQuery + updateQuery + getQuery
// chain through chi (so URL params resolve), POSTs a SQL-mode
// payload, asserts the persisted row carries `mode='sql'` +
// `raw_sql='SELECT 1'`, then issues an update flipping the mode +
// body and re-reads to confirm both fields round-trip.
func TestInsightsCreateUpdateQueryRoundTripsSQLMode(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()

	tenants := tenant.NewPGStore(pool)
	tn, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "ins-h-" + uuid.NewString()[:8], Name: "ins handler", Cell: "test", Plan: "enterprise",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	queries := insights.NewQueryStore(pool)
	dashboards := insights.NewDashboardStore(pool)
	runner := insights.NewRunner(pool, insights.NewCacheStore(pool), queries, nil)
	_ = dashboards // dashboards isn't used by createQuery / updateQuery / getQuery; keep the wiring obvious

	h := &insightsHandlers{
		queries:    queries,
		dashboards: dashboards,
		runner:     runner,
	}

	r := chi.NewRouter()
	r.Route("/q", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(platform.WithTenant(req.Context(), tn)))
			})
		})
		r.Post("/", h.createQuery)
		r.Get("/{id}", h.getQuery)
		r.Put("/{id}", h.updateQuery)
	})

	createBody := `{
		"name": "sql-mode-handler-rt",
		"description": "raw SQL via handler",
		"definition": {"source": "ktype:insights.placeholder"},
		"mode": "sql",
		"raw_sql": "SELECT 1::int AS one"
	}`
	req := httptest.NewRequest(http.MethodPost, "/q/", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var saved insights.Query
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if saved.Mode != insights.QueryModeSQL {
		t.Fatalf("create response Mode = %q; want %q", saved.Mode, insights.QueryModeSQL)
	}
	if saved.RawSQL != "SELECT 1::int AS one" {
		t.Fatalf("create response RawSQL = %q; want SELECT 1::int AS one", saved.RawSQL)
	}

	// Re-read via getQuery to make sure the fields were persisted,
	// not just echoed back from the request.
	getReq := httptest.NewRequest(http.MethodGet, "/q/"+saved.ID.String(), nil)
	getRR := httptest.NewRecorder()
	r.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get: code=%d body=%s", getRR.Code, getRR.Body.String())
	}
	var fetched insights.Query
	if err := json.Unmarshal(getRR.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched.Mode != insights.QueryModeSQL {
		t.Fatalf("persisted Mode = %q; want %q (regression: handler dropped mode field)", fetched.Mode, insights.QueryModeSQL)
	}
	if fetched.RawSQL != "SELECT 1::int AS one" {
		t.Fatalf("persisted RawSQL = %q; want SELECT 1::int AS one (regression: handler dropped raw_sql field)", fetched.RawSQL)
	}

	// Update flips the mode back to visual; the store's normalize
	// helper accepts a non-empty Definition + empty raw_sql when
	// mode='visual'.
	updateBody := `{
		"name": "sql-mode-handler-rt",
		"definition": {"source": "ktype:insights.placeholder"},
		"mode": "visual",
		"raw_sql": ""
	}`
	upReq := httptest.NewRequest(http.MethodPut, "/q/"+saved.ID.String(), bytes.NewBufferString(updateBody))
	upReq.Header.Set("Content-Type", "application/json")
	upRR := httptest.NewRecorder()
	r.ServeHTTP(upRR, upReq)
	if upRR.Code != http.StatusOK {
		t.Fatalf("update: code=%d body=%s", upRR.Code, upRR.Body.String())
	}
	var updated insights.Query
	if err := json.Unmarshal(upRR.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.Mode != insights.QueryModeVisual {
		t.Fatalf("update response Mode = %q; want %q (regression: handler dropped mode on update)", updated.Mode, insights.QueryModeVisual)
	}
	if updated.RawSQL != "" {
		t.Fatalf("update response RawSQL = %q; want empty (regression: handler retained stale raw_sql on visual update)", updated.RawSQL)
	}
}

// TestInsightsCreateQueryGatesSQLModeOnFeatureFlag is the regression
// guard for the second Phase M Task 1 review finding: createQuery /
// updateQuery accepted a mode='sql' body from a non-enterprise
// tenant who only had `insights=true`, persisting a SQL-mode row
// the gate at /run-sql wouldn't see (because RunSaved bypassed it).
//
// Strategy: provision a business-tier tenant, seed
// insights_sql_editor=false, post a SQL-mode body and assert 403
// with the canonical `feature: insights_sql_editor` envelope. Then
// flip the flag on and assert the same payload now returns 201.
// updateQuery is exercised in turn against the previously-persisted
// row to confirm the gate fires on the put path too.
func TestInsightsCreateQueryGatesSQLModeOnFeatureFlag(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()

	tenants := tenant.NewPGStore(pool)
	tn, err := tenants.Create(ctx, tenant.CreateInput{
		Slug: "ins-h-gate-" + uuid.NewString()[:8], Name: "ins gate", Cell: "test", Plan: "business",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	queries := insights.NewQueryStore(pool)
	features := tenant.NewFeatureStore(pool)
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: false}); err != nil {
		t.Fatalf("seed sql editor=false: %v", err)
	}
	dashboards := insights.NewDashboardStore(pool)
	runner := insights.NewRunner(pool, insights.NewCacheStore(pool), queries, nil)
	h := &insightsHandlers{
		queries:    queries,
		dashboards: dashboards,
		runner:     runner,
		features:   features,
	}

	r := chi.NewRouter()
	r.Route("/q", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(platform.WithTenant(req.Context(), tn)))
			})
		})
		r.Post("/", h.createQuery)
		r.Put("/{id}", h.updateQuery)
		r.Get("/{id}", h.getQuery)
	})

	sqlBody := `{
		"name": "gate-test",
		"definition": {"source": "ktype:insights.placeholder"},
		"mode": "sql",
		"raw_sql": "SELECT 1::int AS one"
	}`

	// Disabled: 403 with the canonical envelope.
	req := httptest.NewRequest(http.MethodPost, "/q/", strings.NewReader(sqlBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("create with sql editor disabled: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode 403 envelope: %v", err)
	}
	if env["feature"] != tenant.FeatureInsightsSQLEditor {
		t.Fatalf("403 envelope feature = %v; want %s", env["feature"], tenant.FeatureInsightsSQLEditor)
	}

	// A visual-mode payload from the same tenant must still pass —
	// the gate only fires when the request opts into SQL mode.
	visualBody := `{
		"name": "gate-test-visual",
		"definition": {"source": "ktype:insights.placeholder"},
		"mode": "visual"
	}`
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/q/", strings.NewReader(visualBody)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create visual with sql editor disabled: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var visual insights.Query
	if err := json.Unmarshal(rr.Body.Bytes(), &visual); err != nil {
		t.Fatalf("decode visual: %v", err)
	}

	// Enable the flag and the SQL payload now succeeds.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: true}); err != nil {
		t.Fatalf("enable sql editor: %v", err)
	}
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/q/", strings.NewReader(sqlBody)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create with sql editor enabled: code=%d body=%s", rr.Code, rr.Body.String())
	}
	var saved insights.Query
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode created sql-mode: %v", err)
	}

	// updateQuery must fire the same gate. Disable the flag,
	// attempt to flip the existing visual row to SQL: 403.
	if err := features.SetFeatures(ctx, tn.ID, map[string]bool{tenant.FeatureInsightsSQLEditor: false}); err != nil {
		t.Fatalf("re-disable sql editor: %v", err)
	}
	upBody := `{
		"name": "gate-test-visual",
		"definition": {"source": "ktype:insights.placeholder"},
		"mode": "sql",
		"raw_sql": "SELECT 1::int AS one"
	}`
	upReq := httptest.NewRequest(http.MethodPut, "/q/"+visual.ID.String(), strings.NewReader(upBody))
	upReq.Header.Set("Content-Type", "application/json")
	upRR := httptest.NewRecorder()
	r.ServeHTTP(upRR, upReq)
	if upRR.Code != http.StatusForbidden {
		t.Fatalf("update with sql editor disabled: code=%d body=%s", upRR.Code, upRR.Body.String())
	}
}
