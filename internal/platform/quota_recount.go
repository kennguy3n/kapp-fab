package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeRecordCountRecount is the scheduled_actions.action_type the
// daily reconciliation handler registers under. The wizard seeds one
// row per tenant with a 24h cadence so any drift between
// tenant_record_counts and the source-of-truth krecords scan is detected
// and corrected once per day, regardless of API traffic.
//
// Why a daily reconcile when the counter is maintained transactionally?
//
//   - Direct-SQL repair scripts (operator runs an emergency UPDATE
//     against krecords without going through internal/record/store.go)
//     bypass the transactional bump and would otherwise leave the
//     counter permanently out of sync.
//   - A future code path that writes to krecords without using the
//     record store (e.g. a backup-restore tool, a cell-migration
//     script) cannot be prevented by code review alone — the daily
//     reconcile is the defense-in-depth that catches a regression
//     before it shows up as a billing dispute.
//   - The reconcile is also the catch-up mechanism for the
//     migrations/000067 backfill window on freshly-deployed
//     installations: the backfill is exact at migration time, but
//     the daily handler is what keeps it exact afterwards.
const ActionTypeRecordCountRecount = "tenant_record_count_recount"

// RecordCountReconciler implements scheduler.ActionHandler. Per tenant
// tick, runs the same `SELECT count(*) FROM krecords WHERE tenant_id =
// $1 AND status != 'deleted'` that the old O(n) CheckRecordCount used
// and writes the result into tenant_record_counts. This is the only
// authoritative drift correction in the system; everything else relies
// on the transactional bump.
//
// The handler is stateless beyond its pool reference — exactly mirrors
// the UsageSnapshotHandler / RetentionSweeper pattern other
// scheduler.ActionHandler implementations follow.
type RecordCountReconciler struct {
	pool *pgxpool.Pool
	// metrics is optional; when set, the reconciler emits a gauge
	// `kapp_record_count_drift` labelled by tenant_id whenever the
	// observed count differs from the stored counter. Surfaces silent
	// regressions in the bump path as a Prometheus alert long before
	// they manifest as an under-billed tenant.
	metrics *MetricsRegistry
}

// NewRecordCountReconciler binds the handler to the shared pool.
func NewRecordCountReconciler(pool *pgxpool.Pool) *RecordCountReconciler {
	return &RecordCountReconciler{pool: pool}
}

// WithMetrics opts the reconciler into emitting drift telemetry. Pass
// the process-wide registry built in services/worker so the gauge is
// scraped by the same /metrics endpoint as the rest of the worker's
// instrumentation. Returns the receiver so this can be chained from
// the constructor in worker wiring.
func (h *RecordCountReconciler) WithMetrics(reg *MetricsRegistry) *RecordCountReconciler {
	h.metrics = reg
	return h
}

// Handle implements scheduler.ActionHandler. Counts active krecords
// for the tenant and writes the absolute value into
// tenant_record_counts. The write is an UPSERT (matching the bump
// helper) so a brand-new tenant whose counter row does not yet exist
// is correctly seeded by the reconcile, not by the next krecord insert.
//
// Drift is reported through metrics (if wired) and through the
// returned error: nil means "in sync or fixed", a non-nil error means
// the reconcile itself failed (DB error). The handler does NOT return
// an error on observed drift — that is the expected case the handler
// is here to fix.
func (h *RecordCountReconciler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.pool == nil {
		return errors.New("platform: record-count reconciler not wired")
	}
	if tenantID == uuid.Nil {
		return errors.New("platform: record-count reconcile: tenant id required")
	}
	var (
		observed int64
		stored   int64
		hadRow   bool
	)
	err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Authoritative scan — same WHERE clause the old
		// CheckRecordCount used so the reconcile is byte-for-byte
		// equivalent to what the source-of-truth query would have
		// returned at the moment of the snapshot.
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM krecords
			  WHERE tenant_id = $1 AND status != 'deleted'`,
			tenantID,
		).Scan(&observed); err != nil {
			return fmt.Errorf("platform: record-count reconcile scan: %w", err)
		}
		// Read the current stored value (if any) so the drift gauge
		// reflects the delta we are about to correct, not a misleading
		// "post-correction zero".
		err := tx.QueryRow(ctx,
			`SELECT record_count FROM tenant_record_counts
			  WHERE tenant_id = $1`,
			tenantID,
		).Scan(&stored)
		switch {
		case err == nil:
			hadRow = true
		case errors.Is(err, pgx.ErrNoRows):
			hadRow = false
			stored = 0
		default:
			return fmt.Errorf("platform: record-count reconcile read: %w", err)
		}
		// UPSERT the authoritative value. Idempotent: if observed
		// matches stored, this writes the same value back with a
		// refreshed updated_at — cheap, and the refreshed timestamp
		// is itself useful telemetry ("we know this row was checked
		// at time T").
		_, err = tx.Exec(ctx,
			`INSERT INTO tenant_record_counts (tenant_id, record_count, updated_at)
			 VALUES ($1, $2, now())
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET record_count = EXCLUDED.record_count,
			       updated_at   = now()`,
			tenantID, observed,
		)
		if err != nil {
			return fmt.Errorf("platform: record-count reconcile upsert: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if h.metrics != nil {
		// Drift is observed - stored. Positive means the counter
		// lagged real growth; negative means the counter was high
		// (e.g. a delete path missed its decrement, or a hand-edited
		// row was un-deleted directly). Emitting both signs as a
		// signed delta keeps the alert simple ("|drift| > 0 for >24h
		// = paged").
		var drift float64
		if hadRow {
			drift = float64(observed - stored)
		} else {
			// No prior row → "drift" is the entire backfill. Report
			// as observed so a fleet-wide first-tick spike is
			// distinguishable from an in-flight regression.
			drift = float64(observed)
		}
		// Signed delta — positive when the counter lagged real growth
		// (the bump path missed an insert), negative when the counter
		// was too high (the bump path missed a delete, or a row was
		// hand-edited un-deleted). Alerts should fire on |drift| > 0
		// — use Prometheus abs() in the rule expression rather than
		// pre-aggregating here so the dashboard preserves the sign,
		// which is useful for triage ("missed insert vs missed
		// delete").
		h.metrics.Gauge(
			"kapp_record_count_drift",
			"Signed drift (observed - stored) between tenant_record_counts.record_count and the krecords scan at the last reconciliation tick. Positive: counter under-counted; negative: counter over-counted. Use abs() for alert thresholds.",
			"tenant_id",
		).Set(drift, tenantID.String())
		h.metrics.Gauge(
			"kapp_record_count_reconciled_at_seconds",
			"Unix timestamp of the last successful tenant_record_counts reconciliation per tenant.",
			"tenant_id",
		).Set(float64(time.Now().UTC().Unix()), tenantID.String())
	}
	return nil
}
