package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// tenantPlanLookup is a thin adapter from tenant.PGStore to
// insights.PlanLookup. Kept in services/api so internal/insights
// stays free of an internal/tenant import (the agent tools already
// pull insights, and a back-edge would close a cycle).
type tenantPlanLookup struct {
	store *tenant.PGStore
}

// PlanForTenant resolves the tenant row and returns its plan column.
// Errors are surfaced verbatim so the runner can decide whether to
// treat lookup failures as a deny (currently: lookup failure → no
// gate; the engine-level reporting hard ceiling still applies).
func (l tenantPlanLookup) PlanForTenant(ctx context.Context, tenantID uuid.UUID) (string, error) {
	t, err := l.store.Get(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return t.Plan, nil
}
