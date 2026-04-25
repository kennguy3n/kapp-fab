package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeUsageSnapshot is the scheduled_actions.action_type the
// daily snapshot worker registers under. The wizard seeds one row
// per tenant with a 24h cadence so storage_bytes and krecord_count
// are sampled every day even if no API traffic flows through the
// metering middleware.
const ActionTypeUsageSnapshot = "tenant_usage_snapshot"

// UsageSnapshotHandler is the scheduler.ActionHandler that writes
// the tenant's current storage and record-count footprint into
// tenant_usage. API-call counters are still incremented per-request
// by the metering middleware; this handler is the daily reconciler
// for the absolute counters.
//
// The handler is stateless — the metering store handles the actual
// SUM/COUNT queries and upsert. We just hand it the right tenant
// id from the scheduler dispatch.
type UsageSnapshotHandler struct {
	metering *MeteringStore
}

// NewUsageSnapshotHandler binds a handler to the metering store.
func NewUsageSnapshotHandler(m *MeteringStore) *UsageSnapshotHandler {
	return &UsageSnapshotHandler{metering: m}
}

// Handle implements scheduler.ActionHandler. Emits one storage and
// one krecord snapshot row for the dispatched tenant. Failures are
// reported individually so a missing files table on a brand-new
// tenant does not block the krecord snapshot from landing.
func (h *UsageSnapshotHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.metering == nil {
		return errors.New("tenant: usage snapshot handler not wired")
	}
	var errs []error
	if err := h.metering.SnapshotStorageBytes(ctx, tenantID); err != nil {
		errs = append(errs, fmt.Errorf("tenant: usage snapshot storage: %w", err))
	}
	if err := h.metering.SnapshotKRecordCount(ctx, tenantID); err != nil {
		errs = append(errs, fmt.Errorf("tenant: usage snapshot krecords: %w", err))
	}
	return errors.Join(errs...)
}
