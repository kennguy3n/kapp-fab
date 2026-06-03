package billing

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// UsageSyncHandler is the scheduler.ActionHandler the worker
// registers under tenant.ActionTypeBillingUsageSync. Once a day per
// tenant it (a) pushes the current-period metered usage to Stripe and
// (b) enforces trial-grace expiry. Both steps are independently
// no-ops for free / unsubscribed tenants, so seeding the action for
// every tenant is safe.
//
// The action_type string lives in the tenant package (alongside the
// other scheduled-action constants) because the wizard — which seeds
// the row — cannot import billing without creating an import cycle.
type UsageSyncHandler struct {
	svc *Service
}

// NewUsageSyncHandler binds the handler to the billing service.
func NewUsageSyncHandler(svc *Service) *UsageSyncHandler {
	return &UsageSyncHandler{svc: svc}
}

// Handle implements scheduler.ActionHandler. Usage sync and trial-
// expiry enforcement are reported independently (errors.Join) so a
// Stripe outage on the usage push does not prevent a trial-expiry
// suspension from landing, and vice-versa.
func (h *UsageSyncHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.svc == nil {
		return errors.New("billing: usage sync handler not wired")
	}
	var errs []error
	if err := h.svc.SyncUsage(ctx, tenantID); err != nil {
		errs = append(errs, err)
	}
	if err := h.svc.EnforceTrialExpiry(ctx, tenantID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// compile-time assertion the handler satisfies the scheduler
// interface so a signature drift is caught at build time.
var _ scheduler.ActionHandler = (*UsageSyncHandler)(nil)
