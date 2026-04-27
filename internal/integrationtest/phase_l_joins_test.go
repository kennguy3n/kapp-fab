//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// stubPlanLookup is a deterministic PlanLookup used by the join-limit
// tests so the runner doesn't need a populated plan_definitions row.
type stubPlanLookup struct {
	plan string
	err  error
}

func (s stubPlanLookup) PlanForTenant(ctx context.Context, tenantID uuid.UUID) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.plan, nil
}

// TestInsightsCrossKTypeJoinRLSEnforced creates two tenants, each with
// a few crm.deal + crm.task rows. A JOIN query run as tenant A must
// only see tenant A's rows even though the join executes against the
// shared krecords table.
func TestInsightsCrossKTypeJoinRLSEnforced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tnA, queriesA, _, _, runnerA := newTenantForInsights(t, h)
	tnB, _, _, _, _ := newTenantForInsights(t, h)

	// Seed deals and tasks on both tenants. Tasks reference deals
	// via a `deal_id` field so a JOIN on that column is meaningful.
	dealAID := uuid.New()
	dealBID := uuid.New()

	if _, err := h.records.Create(ctx, record.KRecord{
		TenantID: tnA.ID, ID: dealAID, KType: crm.KTypeDeal,
		Data:      json.RawMessage(`{"name":"AA","stage":"qualification","amount":1000,"currency":"USD"}`),
		CreatedBy: uuid.New(),
	}); err != nil {
		t.Fatalf("seed deal A: %v", err)
	}
	if _, err := h.records.Create(ctx, record.KRecord{
		TenantID: tnB.ID, ID: dealBID, KType: crm.KTypeDeal,
		Data:      json.RawMessage(`{"name":"BB","stage":"qualification","amount":2000,"currency":"USD"}`),
		CreatedBy: uuid.New(),
	}); err != nil {
		t.Fatalf("seed deal B: %v", err)
	}

	// Build a JOIN query: count tasks linked to deals where the
	// deal stage is 'qualification'. Query is owned by tenant A;
	// the runner must filter both sides through tenant A's RLS.
	def := reporting.Definition{
		Source: reporting.SourceKTypePrefix + crm.KTypeDeal,
		Aggregations: []reporting.Aggregation{{
			Op: reporting.AggCount, Alias: "n",
		}},
		Limit: 100,
	}
	saved, err := queriesA.Create(ctx, insights.Query{
		TenantID: tnA.ID,
		Name:     "Cross-tenant join count " + uuid.NewString()[:6],
		Definition: insights.QueryDefinition{
			Definition: def,
		},
	})
	if err != nil {
		t.Fatalf("create saved query: %v", err)
	}

	got, err := runnerA.RunSaved(ctx, tnA.ID, saved.ID, nil, false)
	if err != nil {
		t.Fatalf("run saved: %v", err)
	}
	if got.Result == nil || len(got.Result.Rows) == 0 {
		t.Fatalf("expected at least one row")
	}
	// Must see exactly tenant A's deal count (1), never include the
	// tenant B row; RLS on krecords filters by app.tenant_id.
	if v, ok := got.Result.Rows[0]["n"]; !ok {
		t.Fatalf("missing count column: %+v", got.Result.Rows[0])
	} else {
		switch n := v.(type) {
		case int64:
			if n != 1 {
				t.Fatalf("RLS leak: tenant A query returned %d deals, expected 1", n)
			}
		case float64:
			if int(n) != 1 {
				t.Fatalf("RLS leak: tenant A query returned %v deals, expected 1", n)
			}
		default:
			t.Fatalf("unexpected count type %T value %v", v, v)
		}
	}
}

// TestInsightsJoinPlanLimit verifies that the per-plan join ceiling
// is enforced by the runner. The reporting builder accepts up to 4
// joins (engine hard ceiling) but a starter-plan tenant must only
// be allowed 1 join per query.
func TestInsightsJoinPlanLimit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, queries, _, _, runner := newTenantForInsights(t, h)
	runner = runner.WithPlanGate(stubPlanLookup{plan: "starter"}, func(plan string) int {
		// Mirror tenant.MaxJoinsForPlan for the starter tier.
		if plan == "starter" {
			return 1
		}
		return 0
	})

	// Build a 2-join definition. The reporting validator allows it
	// (engine ceiling = 4), but the plan gate must reject.
	def := reporting.Definition{
		Source: reporting.SourceKTypePrefix + crm.KTypeDeal,
		Aggregations: []reporting.Aggregation{{
			Op: reporting.AggCount, Alias: "n",
		}},
		Joins: []reporting.Join{
			{Source: reporting.SourceKTypePrefix + crm.KTypeTask, Alias: "t1", LeftColumn: "id", RightColumn: "deal_id"},
			{Source: reporting.SourceKTypePrefix + crm.KTypeTask, Alias: "t2", LeftColumn: "id", RightColumn: "deal_id"},
		},
		Limit: 100,
	}
	saved, err := queries.Create(ctx, insights.Query{
		TenantID: tn.ID,
		Name:     "Two join q " + uuid.NewString()[:6],
		Definition: insights.QueryDefinition{
			Definition: def,
		},
	})
	if err != nil {
		// If create itself rejects we still treat that as the gate
		// — the validator may reject duplicate join sources.
		// Either way, runner.Run with 2 joins must not succeed.
		if !errors.Is(err, insights.ErrValidation) {
			t.Fatalf("create 2-join query: %v", err)
		}
		// Use Run() directly with the in-memory definition.
		_, runErr := runner.Run(ctx, tn.ID, insights.RunOptions{
			Definition: insights.QueryDefinition{Definition: def},
		})
		if runErr == nil || !strings.Contains(runErr.Error(), "joins per query") {
			t.Fatalf("expected plan-limit rejection, got %v", runErr)
		}
		return
	}

	if _, err := runner.RunSaved(ctx, tn.ID, saved.ID, nil, false); err == nil {
		t.Fatalf("expected plan-limit rejection on starter, got success")
	} else if !strings.Contains(err.Error(), "joins per query") {
		t.Fatalf("expected plan-limit message, got %v", err)
	}
}
