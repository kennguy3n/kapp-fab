package warehouse

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeWarehouseSync is the scheduled_actions.action_type the
// warehouse bridge registers under. Tenant onboarding seeds one row of
// this type for plans that include the insights/external feature; the
// handler fans out across that tenant's due sync configs each tick.
// Mirrors reporting.ActionTypeReportSchedule.
const ActionTypeWarehouseSync = "warehouse_sync"

// DefaultWarehouseSyncIntervalSeconds is the cadence the wizard seeds —
// five minutes, matching the report scheduler. Per-config eligibility
// is gated on each config's cron expression vs. last_run_at, so ticking
// more often than a config's own cadence is wasted SQL, never a
// duplicate run.
const DefaultWarehouseSyncIntervalSeconds = 300

// SyncHandler is the scheduler.ActionHandler for the warehouse bridge.
// On each tenant tick it runs every due sync config: starts a run row,
// drives the export engine, finalizes the run, and advances the config
// watermarks — recording per-config failures without starving the
// rest.
type SyncHandler struct {
	configs  *ConfigStore
	runs     *RunStore
	exporter *Exporter
	now      func() time.Time
}

// NewSyncHandler wires the handler from its stores and the export
// engine.
func NewSyncHandler(configs *ConfigStore, runs *RunStore, exporter *Exporter) *SyncHandler {
	return &SyncHandler{
		configs:  configs,
		runs:     runs,
		exporter: exporter,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Handle is the scheduler entry point. Errors from one config are
// recorded on its run + config rows and do not abort the tenant's
// remaining configs.
func (h *SyncHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	due, err := h.configs.ListDue(ctx, tenantID, h.now())
	if err != nil {
		return fmt.Errorf("warehouse: list due: %w", err)
	}
	for i := range due {
		cfg := due[i]
		if _, runErr := h.RunConfig(ctx, tenantID, &cfg, TriggerSchedule); runErr != nil {
			// ErrRunInProgress means a manual run (or a prior tick that
			// overran) already holds this config's run lock; skipping is
			// correct, not a failure. Any other failure is already
			// persisted on the run + config by RunConfig; continue so one
			// broken sync doesn't starve the rest.
			continue
		}
	}
	return nil
}

// RunConfig executes a single sync config end to end and returns the
// recorded run. Shared by the scheduler (TriggerSchedule) and the
// "run now" API handler (TriggerManual). The export is all-or-nothing
// within a run: on failure the run is marked 'error', but the
// watermarks of any sources that DID land are still persisted so a
// re-run resumes from the furthest durable point rather than re-reading
// everything.
func (h *SyncHandler) RunConfig(ctx context.Context, tenantID uuid.UUID, cfg *Config, trigger string) (*Run, error) {
	// Serialize against any other run of this same config (a colliding
	// scheduler tick or a concurrent "run now"). The lock is held for
	// the whole run and auto-releases if the worker dies.
	release, ok, err := h.configs.TryLockRun(ctx, cfg.ID)
	if err != nil {
		return nil, fmt.Errorf("warehouse: lock run: %w", err)
	}
	if !ok {
		return nil, ErrRunInProgress
	}
	defer release()

	started := h.now()
	run, err := h.runs.Start(ctx, tenantID, cfg.ID, cfg.Mode, trigger, started)
	if err != nil {
		return nil, fmt.Errorf("warehouse: start run: %w", err)
	}

	res, exportErr := h.exporter.Export(ctx, cfg)
	finished := h.now()

	run.Details = res.RowsBySource
	run.TablesExported = len(res.RowsBySource)
	run.RowsExported = sumRows(res.RowsBySource)

	status := StatusSuccess
	errMsg := ""
	if exportErr != nil {
		status = StatusError
		errMsg = exportErr.Error()
	}

	if finErr := h.runs.Finish(ctx, run, status, finished, errMsg); finErr != nil {
		return run, fmt.Errorf("warehouse: finish run: %w", finErr)
	}
	if wmErr := h.configs.SaveWatermarks(ctx, tenantID, cfg.ID, res.Watermarks, finished, status, errMsg); wmErr != nil {
		return run, fmt.Errorf("warehouse: save watermarks: %w", wmErr)
	}
	if exportErr != nil {
		return run, exportErr
	}
	return run, nil
}

// sumRows totals the per-source row counts for the run summary.
func sumRows(bySource map[string]int64) int64 {
	var total int64
	for _, n := range bySource {
		total += n
	}
	return total
}
