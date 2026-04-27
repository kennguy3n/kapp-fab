package main

import (
	"context"
	"log"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/insights"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// QueryCacheRefreshHandler is the per-tenant scheduler hook that walks
// every saved insights query and re-runs it through the cache-aware
// runner. The runner already short-circuits on a still-fresh cache row
// (see internal/insights/runner.go), so the sweeper is effectively a
// per-query freshness probe — it executes SQL only when a query's
// cache row is missing or has expired.
//
// Tenant scoping flows through the scheduler dispatcher: the loop in
// internal/scheduler/scheduler.go locks the action row, calls the
// registered handler with the tenant_id from the row, and the runner
// itself wraps each Run in dbutil.WithTenantTx so RLS enforces the
// final tenant guarantee.
type QueryCacheRefreshHandler struct {
	queries *insights.QueryStore
	runner  *insights.Runner
}

// NewQueryCacheRefreshHandler constructs a handler. Both stores must
// be non-nil; the worker process always has them wired by the time
// the registry call happens.
func NewQueryCacheRefreshHandler(qs *insights.QueryStore, runner *insights.Runner) *QueryCacheRefreshHandler {
	return &QueryCacheRefreshHandler{queries: qs, runner: runner}
}

// Handle implements scheduler.ActionHandler.
func (h *QueryCacheRefreshHandler) Handle(ctx context.Context, tenantID uuid.UUID, action scheduler.ScheduledAction) error {
	if h == nil || h.queries == nil || h.runner == nil {
		return nil
	}
	queries, err := h.queries.List(ctx, tenantID)
	if err != nil {
		return err
	}
	var refreshed, failed int
	for _, q := range queries {
		// bypass_cache=false: a fresh cache row short-circuits and we
		// don't pay SQL — exactly the warm-cache invariant we want.
		// The Runner stamps a fresh row whenever it runs SQL, so the
		// cache stays warm for the next dashboard view.
		if _, err := h.runner.RunSaved(ctx, tenantID, q.ID, nil, false); err != nil {
			failed++
			log.Printf("worker: query_cache_refresh tenant=%s query=%s: %v", tenantID, q.ID, err)
			continue
		}
		refreshed++
	}
	log.Printf("worker: query_cache_refresh tenant=%s refreshed=%d failed=%d total=%d",
		tenantID, refreshed, failed, len(queries))
	return nil
}
