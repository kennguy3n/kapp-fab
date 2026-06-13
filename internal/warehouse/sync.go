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
// recorded run. Used by the scheduler (TriggerSchedule), which runs in
// the worker process where blocking for the full export is fine. The
// export is all-or-nothing within a run: on failure the run is marked
// 'error', but the watermarks of any sources that DID land are still
// persisted so a re-run resumes from the furthest durable point rather
// than re-reading everything.
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

	run, err := h.startRun(ctx, tenantID, cfg, trigger)
	if err != nil {
		return nil, err
	}
	return run, h.complete(ctx, tenantID, cfg, run)
}

// StartManualRun acquires the per-config run lock, records a 'running'
// run row, and returns the run plus a finish closure that drives the
// export to completion and releases the lock. It lets the API "run now"
// surface return immediately (202 + the running run) and execute the
// possibly-long export on a context detached from the HTTP request, so
// a large sync is not bound by gateway/load-balancer request timeouts.
// The history endpoint is the poll surface: the returned run carries
// the id, and its terminal status/row counts land on the same row when
// finish completes. ErrRunInProgress is returned (without starting a
// run) when another run already holds the config's lock.
//
// The caller MUST invoke finish exactly once (it owns releasing the
// lock); finish bounds itself with an overall deadline so a hung
// destination can never pin the advisory lock indefinitely.
func (h *SyncHandler) StartManualRun(ctx context.Context, tenantID uuid.UUID, cfg *Config) (run *Run, finish func(context.Context) error, err error) {
	release, ok, err := h.configs.TryLockRun(ctx, cfg.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("warehouse: lock run: %w", err)
	}
	if !ok {
		return nil, nil, ErrRunInProgress
	}
	run, err = h.startRun(ctx, tenantID, cfg, TriggerManual)
	if err != nil {
		release()
		return nil, nil, err
	}
	finish = func(ctx context.Context) error {
		defer release()
		// Bound the detached run: each source still gets its full
		// per-source copy budget plus one slot of slack for destination
		// setup, but the whole run cannot outlive that ceiling and strand
		// the lock.
		budget := time.Duration(len(cfg.Sources)+1) * h.exporter.timeout
		ctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		return h.complete(ctx, tenantID, cfg, run)
	}
	return run, finish, nil
}

// startRun records the 'running' row that both the synchronous and
// detached paths build on.
func (h *SyncHandler) startRun(ctx context.Context, tenantID uuid.UUID, cfg *Config, trigger string) (*Run, error) {
	run, err := h.runs.Start(ctx, tenantID, cfg.ID, cfg.Mode, trigger, h.now())
	if err != nil {
		return nil, fmt.Errorf("warehouse: start run: %w", err)
	}
	return run, nil
}

// complete drives the export for an already-started run, finalizes the
// run row, and advances the config watermarks. On export failure the
// run is marked 'error' and the failing error is returned, but the
// watermarks of the sources that DID land are still persisted first.
func (h *SyncHandler) complete(ctx context.Context, tenantID uuid.UUID, cfg *Config, run *Run) error {
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
		return fmt.Errorf("warehouse: finish run: %w", finErr)
	}
	if wmErr := h.configs.SaveWatermarks(ctx, tenantID, cfg.ID, res.Watermarks, finished, status, errMsg); wmErr != nil {
		return fmt.Errorf("warehouse: save watermarks: %w", wmErr)
	}
	return exportErr
}

// sumRows totals the per-source row counts for the run summary.
func sumRows(bySource map[string]int64) int64 {
	var total int64
	for _, n := range bySource {
		total += n
	}
	return total
}
