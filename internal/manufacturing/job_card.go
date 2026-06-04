package manufacturing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// JobCardStatus enumerates the legal values for job_cards.status. The
// Go state machine (CanTransitionTo) enforces the legal moves so the
// error surface is a typed sentinel rather than a CHECK violation.
const (
	JobCardStatusPending    = "pending"
	JobCardStatusInProgress = "in_progress"
	JobCardStatusCompleted  = "completed"
)

// Sentinel errors for the shop-floor job-card surface.
var (
	// ErrJobCardNotFound is returned by GetJobCard / StartJobCard /
	// CompleteJobCard when the card does not exist for the tenant.
	ErrJobCardNotFound = errors.New("manufacturing: job card not found")

	// ErrJobCardInvalidTransition is returned for an illegal job-card
	// status transition (e.g. completed → in_progress). See
	// JobCard.CanTransitionTo for the matrix.
	ErrJobCardInvalidTransition = errors.New("manufacturing: invalid job card status transition")
)

// JobCard is a shop-floor execution record, one per routing operation
// per work order. Created automatically when a work order is released
// (see ReleaseWorkOrder). status walks pending → in_progress →
// completed; completing the last open card on a work order triggers the
// existing CompleteWorkOrder inventory-move flow.
type JobCard struct {
	TenantID            uuid.UUID `json:"tenant_id"`
	ID                  uuid.UUID `json:"id"`
	WorkOrderID         uuid.UUID `json:"work_order_id"`
	RoutingOperationSeq int       `json:"routing_operation_seq"`
	WorkCenterID        uuid.UUID `json:"work_center_id"`
	Status              string    `json:"status"`

	PlannedStart *time.Time `json:"planned_start,omitempty"`
	PlannedEnd   *time.Time `json:"planned_end,omitempty"`
	ActualStart  *time.Time `json:"actual_start,omitempty"`
	ActualEnd    *time.Time `json:"actual_end,omitempty"`

	OperatorID  *uuid.UUID      `json:"operator_id,omitempty"`
	QtyProduced decimal.Decimal `json:"qty_produced"`
	QtyRejected decimal.Decimal `json:"qty_rejected"`
	Notes       string          `json:"notes,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CanTransitionTo reports whether the job card may move to the supplied
// target status. Legal transitions:
//
//	pending     → in_progress   (operator starts the operation)
//	pending     → completed     (one-step completion for a trivial op)
//	in_progress → completed     (operator finishes the operation)
//	X           → X             (idempotent re-assertion so HTTP /
//	                             KChat retries don't fail)
//
// Backwards moves (completed → in_progress, in_progress → pending) are
// rejected: a completed card may have already triggered the work
// order's inventory moves, so reopening it would desynchronise the
// shop-floor record from the ledger.
func (j JobCard) CanTransitionTo(target string) bool {
	if j.Status == target {
		return true
	}
	switch j.Status {
	case JobCardStatusPending:
		return target == JobCardStatusInProgress || target == JobCardStatusCompleted
	case JobCardStatusInProgress:
		return target == JobCardStatusCompleted
	case JobCardStatusCompleted:
		return false
	default:
		return false
	}
}

// createJobCardsForWorkOrderTx inserts one job card per operation of the
// supplied routing, inside the caller's transaction. Called from
// ReleaseWorkOrder once the active routing has been snapshotted onto
// the work order. Idempotent: the (tenant_id, work_order_id,
// routing_operation_seq) unique constraint plus ON CONFLICT DO NOTHING
// makes a re-release a no-op for cards that already exist.
//
// planned_start / planned_end are seeded from the work order's
// scheduled window so the capacity grid and the shop-floor UI have a
// nominal slot to render before the operator stamps actuals. v1 does
// not spread the window across operations (no finite forward-scheduling
// yet) — every card inherits the same planned window.
func (s *PGStore) createJobCardsForWorkOrderTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, wo *WorkOrder, routing *Routing) error {
	now := s.now()
	for _, op := range routing.Operations {
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO job_cards
			     (tenant_id, id, work_order_id, routing_operation_seq, work_center_id,
			      status, planned_start, planned_end, qty_produced, qty_rejected,
			      created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, 0, 0, $8, $8)
			 ON CONFLICT (tenant_id, work_order_id, routing_operation_seq) DO NOTHING`,
			tenantID, id, wo.ID, op.Sequence, op.WorkCenterID,
			wo.ScheduledStart, wo.ScheduledEnd, now,
		); err != nil {
			return fmt.Errorf("manufacturing: insert job card for op %d: %w", op.Sequence, err)
		}
	}
	return nil
}

// GetJobCard fetches a single job card.
func (s *PGStore) GetJobCard(ctx context.Context, tenantID, jobCardID uuid.UUID) (*JobCard, error) {
	if tenantID == uuid.Nil || jobCardID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and job card id required")
	}
	var jc JobCard
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanJobCard(tx.QueryRow(ctx, jobCardSelectColumns+
			` FROM job_cards WHERE tenant_id = $1 AND id = $2`,
			tenantID, jobCardID), &jc)
	})
	if err != nil {
		return nil, err
	}
	return &jc, nil
}

// ListJobCards returns the job cards for a work order, ordered by
// routing operation sequence so the shop-floor UI renders the steps in
// execution order.
func (s *PGStore) ListJobCards(ctx context.Context, tenantID, woID uuid.UUID) ([]JobCard, error) {
	if tenantID == uuid.Nil || woID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and work order id required")
	}
	out := make([]JobCard, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, jobCardSelectColumns+
			` FROM job_cards WHERE tenant_id = $1 AND work_order_id = $2
			  ORDER BY routing_operation_seq`,
			tenantID, woID)
		if err != nil {
			return fmt.Errorf("manufacturing: list job cards: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var jc JobCard
			if err := scanJobCard(rows, &jc); err != nil {
				return err
			}
			out = append(out, jc)
		}
		return rows.Err()
	})
	return out, err
}

// StartJobCard transitions a pending job card to in_progress, stamps
// actual_start, and records the operator who started it.
func (s *PGStore) StartJobCard(ctx context.Context, tenantID, jobCardID, operatorID uuid.UUID) (*JobCard, error) {
	if tenantID == uuid.Nil || jobCardID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and job card id required")
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		jc, err := lockJobCard(ctx, tx, tenantID, jobCardID)
		if err != nil {
			return err
		}
		if !jc.CanTransitionTo(JobCardStatusInProgress) {
			return fmt.Errorf("%w: %s → in_progress", ErrJobCardInvalidTransition, jc.Status)
		}
		if jc.Status == JobCardStatusInProgress {
			return nil
		}
		_, err = tx.Exec(ctx,
			`UPDATE job_cards
			    SET status = 'in_progress',
			        actual_start = COALESCE(actual_start, $3),
			        operator_id = COALESCE(operator_id, $4),
			        updated_at = $3
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, jobCardID, s.now(), nullableUUID(operatorID),
		)
		if err != nil {
			return fmt.Errorf("manufacturing: start job card: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetJobCard(ctx, tenantID, jobCardID)
}

// CompleteJobCardInput captures the operator-reported yield for a
// single shop-floor operation.
type CompleteJobCardInput struct {
	OperatorID  uuid.UUID
	QtyProduced decimal.Decimal
	QtyRejected decimal.Decimal
	Notes       string
}

// CompleteJobCard transitions a job card to completed, stamps
// actual_end + the reported yield, and — when it is the last open card
// on the work order — triggers the existing CompleteWorkOrder flow so
// the finished-goods receipt and component-consumption inventory moves
// are emitted exactly as they would be for a BOM-only work order.
//
// The work-order completion runs in its own transaction AFTER the
// job-card update commits, and CompleteWorkOrder is idempotent (the
// inventory_moves_source_uniq index makes a replay a no-op). So if the
// auto-completion fails (e.g. ErrWorkOrderInsufficientStock), the
// operator pre-receipts the missing material and re-completes the same
// (already-completed) card to retry the work-order completion without
// double-emitting any move.
func (s *PGStore) CompleteJobCard(ctx context.Context, tenantID, jobCardID uuid.UUID, in CompleteJobCardInput) (*JobCard, error) {
	if tenantID == uuid.Nil || jobCardID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and job card id required")
	}
	if in.QtyProduced.IsNegative() || in.QtyRejected.IsNegative() {
		return nil, fmt.Errorf("%w: qty_produced and qty_rejected must be >= 0", ErrInvalidInput)
	}

	var woID uuid.UUID
	var woStatus string
	var openRemaining int
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		jc, err := lockJobCard(ctx, tx, tenantID, jobCardID)
		if err != nil {
			return err
		}
		if !jc.CanTransitionTo(JobCardStatusCompleted) {
			return fmt.Errorf("%w: %s → completed", ErrJobCardInvalidTransition, jc.Status)
		}
		woID = jc.WorkOrderID

		if jc.Status != JobCardStatusCompleted {
			// First completion: flip status, stamp actuals and
			// the reported yield. A re-completion (idempotent
			// retry of the auto-WO-completion path) skips the
			// UPDATE so a retry can't silently overwrite the
			// originally-journalled yield.
			if _, err := tx.Exec(ctx,
				`UPDATE job_cards
				    SET status = 'completed',
				        actual_start = COALESCE(actual_start, $3),
				        actual_end = COALESCE(actual_end, $3),
				        operator_id = COALESCE(operator_id, $4),
				        qty_produced = $5,
				        qty_rejected = $6,
				        notes = COALESCE($7, notes),
				        updated_at = $3
				  WHERE tenant_id = $1 AND id = $2`,
				tenantID, jobCardID, s.now(), nullableUUID(in.OperatorID),
				in.QtyProduced, in.QtyRejected, nullableString(in.Notes),
			); err != nil {
				return fmt.Errorf("manufacturing: complete job card: %w", err)
			}
		}

		// Count cards on the same work order that are not yet
		// completed (this card is already completed above, so it
		// is excluded by the status filter). When zero, this was
		// the last open card and the work order is ready to close
		// out.
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM job_cards
			  WHERE tenant_id = $1 AND work_order_id = $2 AND status <> 'completed'`,
			tenantID, woID,
		).Scan(&openRemaining); err != nil {
			return fmt.Errorf("manufacturing: count open job cards: %w", err)
		}

		if err := tx.QueryRow(ctx,
			`SELECT status FROM work_orders WHERE tenant_id = $1 AND id = $2`,
			tenantID, woID,
		).Scan(&woStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrWorkOrderNotFound
			}
			return fmt.Errorf("manufacturing: lookup work order for job card: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Trigger the work-order completion outside the job-card tx. Only
	// do so when every card is completed AND the work order is still
	// in a state that can transition to completed — otherwise a
	// later card-completion on an already-completed/closed work order
	// would surface a spurious ErrWorkOrderInvalidTransition.
	if openRemaining == 0 &&
		(woStatus == WorkOrderStatusReleased || woStatus == WorkOrderStatusInProgress) {
		if _, err := s.CompleteWorkOrder(ctx, tenantID, woID, in.OperatorID, CompleteWorkOrderInput{
			ActualQty: in.QtyProduced,
		}); err != nil {
			return nil, fmt.Errorf("manufacturing: auto-complete work order %s from final job card: %w", woID, err)
		}
	}
	return s.GetJobCard(ctx, tenantID, jobCardID)
}

// jobCardSelectColumns is the shared column list for Get / List so the
// scan order is specified once. Leading "SELECT " is included so call
// sites can append the FROM clause directly.
const jobCardSelectColumns = `SELECT tenant_id, id, work_order_id, routing_operation_seq, work_center_id,
        status, planned_start, planned_end, actual_start, actual_end,
        operator_id, qty_produced, qty_rejected, COALESCE(notes, ''),
        created_at, updated_at`

// lockJobCard reads a job card FOR UPDATE so the state-machine guard in
// Start / Complete can't race a concurrent transition.
func lockJobCard(ctx context.Context, tx pgx.Tx, tenantID, jobCardID uuid.UUID) (*JobCard, error) {
	var jc JobCard
	if err := scanJobCard(tx.QueryRow(ctx, jobCardSelectColumns+
		` FROM job_cards WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		tenantID, jobCardID), &jc); err != nil {
		return nil, err
	}
	return &jc, nil
}

// scanJobCard maps a row in jobCardSelectColumns order onto a JobCard.
// Shared between Get / List / lockJobCard so the column order lives in
// exactly one place.
func scanJobCard(r pgxScanner, jc *JobCard) error {
	var operatorID *uuid.UUID
	if err := r.Scan(
		&jc.TenantID, &jc.ID, &jc.WorkOrderID, &jc.RoutingOperationSeq, &jc.WorkCenterID,
		&jc.Status, &jc.PlannedStart, &jc.PlannedEnd, &jc.ActualStart, &jc.ActualEnd,
		&operatorID, &jc.QtyProduced, &jc.QtyRejected, &jc.Notes,
		&jc.CreatedAt, &jc.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrJobCardNotFound
		}
		return fmt.Errorf("manufacturing: scan job card: %w", err)
	}
	jc.OperatorID = operatorID
	return nil
}
