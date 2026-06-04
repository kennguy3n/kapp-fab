package manufacturing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
)

// PGStore persists BOMs and work orders against PostgreSQL. Every
// mutation runs inside dbutil.WithTenantTx so the app.tenant_id GUC
// is set before any RLS-protected table is touched.
//
// The store collaborates with an *inventory.PGStore for the
// completion-time stock moves; a nil inventory store means the work
// order engine still runs but CompleteWorkOrder returns an explicit
// error (used by unit tests that don't need to spin up the
// inventory schema).
type PGStore struct {
	pool      *pgxpool.Pool
	inventory *inventory.PGStore
	now       func() time.Time
}

// NewPGStore wires a PGStore. inv may be nil for tests that don't
// exercise CompleteWorkOrder; every production caller should pass a
// non-nil store.
func NewPGStore(pool *pgxpool.Pool, inv *inventory.PGStore) *PGStore {
	return &PGStore{
		pool:      pool,
		inventory: inv,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// ----- BOM CRUD -----

// CreateBOMInput is the canonical input for CreateBOM. Components
// must be non-empty; component.ScrapPercent is optional.
type CreateBOMInput struct {
	ItemID     uuid.UUID
	Version    string
	OutputQty  decimal.Decimal
	UOM        string
	Notes      string
	Components []BOMComponent
	// Activate, when true, transitions the freshly-inserted BOM
	// from draft to active inside the same transaction as the
	// insert. Atomicity matters: if the HTTP handler ran the
	// activation as a second top-level call (CreateBOM commits →
	// SetBOMStatus runs) any failure of the second step (advisory-
	// lock contention, transient DB error, panic) would strand the
	// BOM in draft, and the client's natural retry would fail the
	// (tenant_id, item_id, version) unique constraint on re-insert
	// and surface a raw 500. Folding the activation into this same
	// tx makes the whole "create-and-activate" step a single
	// commit, so retries are idempotent (either both succeed or
	// neither commits).
	Activate bool
}

// CreateBOM inserts a new BOM plus its component rows in a single
// transaction. If in.Activate is true, the activation is performed
// in the same transaction so create-and-activate is atomic (see
// CreateBOMInput.Activate for the rationale). The single-active-
// row invariant is enforced via the same advisory-lock-then-demote
// dance as SetBOMStatus.
func (s *PGStore) CreateBOM(ctx context.Context, tenantID, actorID uuid.UUID, in CreateBOMInput) (*BOM, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.ItemID == uuid.Nil {
		return nil, fmt.Errorf("%w: item_id required", ErrInvalidInput)
	}
	if in.Version == "" {
		return nil, fmt.Errorf("%w: version required", ErrInvalidInput)
	}
	if in.OutputQty.IsZero() || in.OutputQty.IsNegative() {
		// Mirror PlannedQty's validation contract (see
		// CreateWorkOrder below): surface client-supplied
		// invalid quantities as a 422 via ErrInvalidInput
		// instead of silently coercing to 1. Silent coercion
		// would let a `output_qty: -5` typo land as a BOM with
		// output_qty=1, producing wildly wrong consumption
		// math at completion time (qty per finished unit is
		// computed as planned_qty / output_qty) — exactly the
		// kind of unit-economics bug the user would never
		// detect from the response payload.
		return nil, fmt.Errorf("%w: output_qty must be > 0", ErrInvalidInput)
	}
	if in.UOM == "" {
		in.UOM = "ea"
	}
	if len(in.Components) == 0 {
		return nil, ErrBOMHasNoComponents
	}
	seen := make(map[uuid.UUID]struct{}, len(in.Components))
	for _, c := range in.Components {
		if c.ComponentItemID == in.ItemID {
			return nil, ErrBOMSelfReference
		}
		if c.Qty.IsZero() || c.Qty.IsNegative() {
			return nil, fmt.Errorf("%w: component %s qty must be > 0", ErrInvalidInput, c.ComponentItemID)
		}
		// Detect duplicates in Go rather than letting the
		// (tenant_id, bom_id, component_item_id) PK fire a
		// raw 23505 — that would surface as a 500 with a
		// cryptic constraint name to the HTTP caller. A
		// duplicate is always an authoring mistake (the qty
		// of the same raw material should be summed, not
		// listed twice).
		if _, dup := seen[c.ComponentItemID]; dup {
			return nil, fmt.Errorf("%w: %s", ErrBOMDuplicateComponent, c.ComponentItemID)
		}
		seen[c.ComponentItemID] = struct{}{}
	}

	now := s.now()
	bom := &BOM{
		TenantID:  tenantID,
		ID:        uuid.New(),
		ItemID:    in.ItemID,
		Version:   in.Version,
		Status:    BOMStatusDraft,
		OutputQty: in.OutputQty,
		UOM:       in.UOM,
		Notes:     in.Notes,
		CreatedBy: actorID,
		CreatedAt: now,
		UpdatedAt: now,
	}

	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO boms
			     (tenant_id, id, item_id, version, status, output_qty, uom, notes, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
			bom.TenantID, bom.ID, bom.ItemID, bom.Version, bom.Status, bom.OutputQty, bom.UOM, nullableString(bom.Notes), nullableUUID(bom.CreatedBy), bom.CreatedAt,
		); err != nil {
			return fmt.Errorf("manufacturing: insert bom: %w", err)
		}
		// Per-component insert keeps the SQL readable; the
		// table is rarely longer than a few dozen rows so a
		// COPY is unnecessary.
		for i, c := range in.Components {
			var scrap any
			if c.ScrapPercent != nil {
				scrap = *c.ScrapPercent
			}
			uom := c.UOM
			if uom == "" {
				uom = "ea"
			}
			// sort_order is derived from the position the caller
			// placed this component in the input slice. We used
			// to honour a client-supplied SortOrder and fall back
			// to `i + 1` when it was zero — but Go has no way to
			// tell an "explicit zero" from a "default zero", so
			// any input that started with SortOrder=0 (the very
			// natural 0-indexed convention the frontend used) had
			// its first component silently re-keyed to 1, colli-
			// ding with the second component's explicit 1 and
			// breaking the deterministic ordering. The array IS
			// the ordering — anything else is a foot-gun.
			sort := i + 1
			if _, err := tx.Exec(ctx,
				`INSERT INTO bom_components
				     (tenant_id, bom_id, component_item_id, qty, uom, scrap_percent, sort_order)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				bom.TenantID, bom.ID, c.ComponentItemID, c.Qty, uom, scrap, sort,
			); err != nil {
				return fmt.Errorf("manufacturing: insert component %s: %w", c.ComponentItemID, err)
			}
			c.BOMID = bom.ID
			c.UOM = uom
			c.SortOrder = sort
			bom.Components = append(bom.Components, c)
		}
		if in.Activate {
			// Atomically flip to active in the same tx as the
			// insert. activateBOMInTx takes the advisory lock
			// keyed on (tenant_id, item_id) and demotes any
			// other active row for the item before flipping
			// this BOM — same invariant SetBOMStatus enforces.
			if err := activateBOMInTx(ctx, tx, tenantID, bom.ID, bom.ItemID, s.now); err != nil {
				return err
			}
			bom.Status = BOMStatusActive
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bom, nil
}

// activateBOMInTx serialises BOM activations against the
// (tenant_id, item_id) partial-unique-index invariant inside an
// existing transaction. Shared between CreateBOM(Activate=true)
// and SetBOMStatus so both code paths use the same advisory lock
// + demote sequence — mismatch between the two would re-introduce
// the race that prompted the lock in the first place.
//
// Caller is responsible for verifying that the BOM has at least
// one component before invoking this helper (CreateBOM trivially
// satisfies this via the ErrBOMHasNoComponents check up front;
// SetBOMStatus does an explicit count).
func activateBOMInTx(ctx context.Context, tx pgx.Tx, tenantID, bomID, itemID uuid.UUID, now func() time.Time) error {
	// Serialise concurrent activations for the same item via an
	// advisory xact lock keyed on hashtextextended(tenant_id ||
	// ':' || item_id, 0). See the SetBOMStatus comment for the
	// full rationale; the short version is that the partial unique
	// index alone is necessary but not sufficient — two threads
	// can each demote the (single) currently-active row and race
	// on the final promote, surfacing a raw 23505 to the loser.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(
		            hashtextextended($1::text || ':' || $2::text, 0))`,
		tenantID, itemID,
	); err != nil {
		return fmt.Errorf("manufacturing: acquire activation lock: %w", err)
	}
	// Single timestamp shared across the demote + promote UPDATEs.
	// Calling now() twice would yield two microsecond-different
	// updated_at values on rows that are flipped inside the same
	// advisory-locked transaction; reusing one stamp keeps the
	// before/after pair temporally identical, which matters for any
	// audit query that joins on updated_at to attribute a status
	// flip to a single activation event.
	ts := now()
	if _, err := tx.Exec(ctx,
		`UPDATE boms
		    SET status = 'obsolete', updated_at = $3
		  WHERE tenant_id = $1 AND item_id = $2
		    AND status = 'active' AND id <> $4`,
		tenantID, itemID, ts, bomID,
	); err != nil {
		return fmt.Errorf("manufacturing: demote previous active bom: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE boms SET status = 'active', updated_at = $3
		  WHERE tenant_id = $1 AND id = $2`,
		tenantID, bomID, ts,
	); err != nil {
		return fmt.Errorf("manufacturing: update bom status: %w", err)
	}
	return nil
}

// GetBOM fetches a BOM and its components.
func (s *PGStore) GetBOM(ctx context.Context, tenantID, bomID uuid.UUID) (*BOM, error) {
	if tenantID == uuid.Nil || bomID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and bom id required")
	}
	var bom BOM
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var notes *string
		var createdBy *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, version, status, output_qty, uom, notes, created_by, created_at, updated_at
			   FROM boms
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, bomID,
		).Scan(&bom.TenantID, &bom.ID, &bom.ItemID, &bom.Version, &bom.Status, &bom.OutputQty, &bom.UOM, &notes, &createdBy, &bom.CreatedAt, &bom.UpdatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBOMNotFound
			}
			return fmt.Errorf("manufacturing: select bom: %w", err)
		}
		if notes != nil {
			bom.Notes = *notes
		}
		if createdBy != nil {
			bom.CreatedBy = *createdBy
		}

		rows, err := tx.Query(ctx,
			`SELECT bom_id, component_item_id, qty, uom, scrap_percent, sort_order
			   FROM bom_components
			  WHERE tenant_id = $1 AND bom_id = $2
			  ORDER BY sort_order, component_item_id`,
			tenantID, bomID,
		)
		if err != nil {
			return fmt.Errorf("manufacturing: select components: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c BOMComponent
			var scrap *decimal.Decimal
			if err := rows.Scan(&c.BOMID, &c.ComponentItemID, &c.Qty, &c.UOM, &scrap, &c.SortOrder); err != nil {
				return fmt.Errorf("manufacturing: scan component: %w", err)
			}
			c.ScrapPercent = scrap
			bom.Components = append(bom.Components, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &bom, nil
}

// ListBOMs returns the BOM headers for a tenant, optionally
// filtered by status. Components are NOT loaded; callers that need
// the recipe must follow up with GetBOM.
func (s *PGStore) ListBOMs(ctx context.Context, tenantID uuid.UUID, status string) ([]BOM, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	// Initialise with len==0 instead of `var out []BOM` so JSON encoding
	// produces `[]` rather than `null` when there are no rows — keeps the
	// HTTP response shape consistent with the inventory.ListItems pattern
	// and avoids tripping up downstream consumers that don't guard for
	// null.
	out := make([]BOM, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT tenant_id, id, item_id, version, status, output_qty, uom, COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at, updated_at
		        FROM boms
		       WHERE tenant_id = $1`
		args := []any{tenantID}
		if status != "" {
			q += " AND status = $2"
			args = append(args, status)
		}
		q += " ORDER BY item_id, version"
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("manufacturing: list boms: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b BOM
			if err := rows.Scan(&b.TenantID, &b.ID, &b.ItemID, &b.Version, &b.Status, &b.OutputQty, &b.UOM, &b.Notes, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
				return fmt.Errorf("manufacturing: scan bom: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// SetBOMStatus transitions a BOM between draft / active / obsolete.
// Activating a BOM auto-deactivates any other active row for the
// same item (transitioning that row to obsolete) so the
// boms_active_per_item_uniq partial unique index never collides.
// Activation of a BOM with no components returns
// ErrBOMHasNoComponents. Illegal transitions (e.g. obsolete → active,
// active → draft) return ErrBOMInvalidTransition — see
// BOM.CanTransitionTo for the matrix and rationale.
func (s *PGStore) SetBOMStatus(ctx context.Context, tenantID, bomID uuid.UUID, status string) error {
	switch status {
	case BOMStatusDraft, BOMStatusActive, BOMStatusObsolete:
	default:
		return fmt.Errorf("%w: invalid bom status %q", ErrInvalidInput, status)
	}
	if tenantID == uuid.Nil || bomID == uuid.Nil {
		return errors.New("manufacturing: tenant id and bom id required")
	}

	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Lock the row so a concurrent SetBOMStatus call can't
		// race the state-machine guard below — without FOR UPDATE
		// two callers could both read status='draft', both pass
		// CanTransitionTo, and one of them would land an illegal
		// double transition. The lock is the same scope as the
		// row whose status we're about to flip, so it never
		// escalates to a wider serialisation contention.
		var current BOM
		err := tx.QueryRow(ctx,
			`SELECT item_id, status FROM boms WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, bomID,
		).Scan(&current.ItemID, &current.Status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBOMNotFound
			}
			return fmt.Errorf("manufacturing: lookup bom: %w", err)
		}

		if !current.CanTransitionTo(status) {
			return fmt.Errorf("%w: %s → %s", ErrBOMInvalidTransition, current.Status, status)
		}
		if current.Status == status {
			// Idempotent no-op. Return early so a retry of
			// SetBOMStatus(active) doesn't acquire the
			// advisory lock + re-run the demote/promote
			// dance for no reason.
			return nil
		}

		if status == BOMStatusActive {
			var n int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM bom_components WHERE tenant_id = $1 AND bom_id = $2`,
				tenantID, bomID,
			).Scan(&n); err != nil {
				return fmt.Errorf("manufacturing: count components: %w", err)
			}
			if n == 0 {
				return ErrBOMHasNoComponents
			}
			return activateBOMInTx(ctx, tx, tenantID, bomID, current.ItemID, s.now)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE boms SET status = $1, updated_at = now()
			  WHERE tenant_id = $2 AND id = $3`,
			status, tenantID, bomID,
		); err != nil {
			return fmt.Errorf("manufacturing: update bom status: %w", err)
		}
		return nil
	})
}

// activeBOMForItem returns the unique active BOM for an item. Used
// internally by the work-order engine to resolve the recipe at
// release time.
func (s *PGStore) activeBOMForItem(ctx context.Context, tx pgx.Tx, tenantID, itemID uuid.UUID) (*BOM, error) {
	var bom BOM
	var notes *string
	var createdBy *uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT tenant_id, id, item_id, version, status, output_qty, uom, notes, created_by, created_at, updated_at
		   FROM boms
		  WHERE tenant_id = $1 AND item_id = $2 AND status = 'active'`,
		tenantID, itemID,
	).Scan(&bom.TenantID, &bom.ID, &bom.ItemID, &bom.Version, &bom.Status, &bom.OutputQty, &bom.UOM, &notes, &createdBy, &bom.CreatedAt, &bom.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBOMNotActive
		}
		return nil, fmt.Errorf("manufacturing: lookup active bom: %w", err)
	}
	if notes != nil {
		bom.Notes = *notes
	}
	if createdBy != nil {
		bom.CreatedBy = *createdBy
	}

	rows, err := tx.Query(ctx,
		`SELECT bom_id, component_item_id, qty, uom, scrap_percent, sort_order
		   FROM bom_components
		  WHERE tenant_id = $1 AND bom_id = $2
		  ORDER BY sort_order, component_item_id`,
		tenantID, bom.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select active bom components: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c BOMComponent
		var scrap *decimal.Decimal
		if err := rows.Scan(&c.BOMID, &c.ComponentItemID, &c.Qty, &c.UOM, &scrap, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("manufacturing: scan active bom component: %w", err)
		}
		c.ScrapPercent = scrap
		bom.Components = append(bom.Components, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &bom, nil
}

// ----- Work Order CRUD -----

// CreateWorkOrderInput is the canonical input for CreateWorkOrder.
type CreateWorkOrderInput struct {
	ItemID         uuid.UUID
	WarehouseID    uuid.UUID
	PlannedQty     decimal.Decimal
	ScheduledStart *time.Time
	ScheduledEnd   *time.Time
	Notes          string
}

// CreateWorkOrder inserts a draft work order. The bom_id is left
// NULL until ReleaseWorkOrder snapshots the active BOM at release
// time (so the consumption math is reproducible even after the BOM
// is later marked obsolete or replaced).
func (s *PGStore) CreateWorkOrder(ctx context.Context, tenantID, actorID uuid.UUID, in CreateWorkOrderInput) (*WorkOrder, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.ItemID == uuid.Nil || in.WarehouseID == uuid.Nil {
		return nil, fmt.Errorf("%w: item_id and warehouse_id required", ErrInvalidInput)
	}
	if in.PlannedQty.IsZero() || in.PlannedQty.IsNegative() {
		return nil, fmt.Errorf("%w: planned_qty must be > 0", ErrInvalidInput)
	}

	now := s.now()
	wo := &WorkOrder{
		TenantID:       tenantID,
		ID:             uuid.New(),
		ItemID:         in.ItemID,
		WarehouseID:    in.WarehouseID,
		PlannedQty:     in.PlannedQty,
		Status:         WorkOrderStatusDraft,
		ScheduledStart: in.ScheduledStart,
		ScheduledEnd:   in.ScheduledEnd,
		Notes:          in.Notes,
		CreatedBy:      actorID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO work_orders
			     (tenant_id, id, item_id, warehouse_id, planned_qty, status,
			      scheduled_start, scheduled_end, notes, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)`,
			wo.TenantID, wo.ID, wo.ItemID, wo.WarehouseID, wo.PlannedQty, wo.Status,
			wo.ScheduledStart, wo.ScheduledEnd, nullableString(wo.Notes), nullableUUID(wo.CreatedBy), wo.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("manufacturing: insert work order: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return wo, nil
}

// GetWorkOrder fetches a work order header.
func (s *PGStore) GetWorkOrder(ctx context.Context, tenantID, woID uuid.UUID) (*WorkOrder, error) {
	if tenantID == uuid.Nil || woID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and work order id required")
	}
	var wo WorkOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanWorkOrder(tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, bom_id, routing_id, warehouse_id, planned_qty, actual_qty, status,
			        scheduled_start, scheduled_end, started_at, completed_at,
			        COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
			        created_at, updated_at
			   FROM work_orders
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, woID), &wo)
	})
	if err != nil {
		return nil, err
	}
	return &wo, nil
}

// ListWorkOrders returns work orders for a tenant, optionally
// filtered by status.
func (s *PGStore) ListWorkOrders(ctx context.Context, tenantID uuid.UUID, status string) ([]WorkOrder, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	// Same rationale as ListBOMs above — produce `[]` instead of `null`
	// when there are no rows.
	out := make([]WorkOrder, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT tenant_id, id, item_id, bom_id, routing_id, warehouse_id, planned_qty, actual_qty, status,
		             scheduled_start, scheduled_end, started_at, completed_at,
		             COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
		             created_at, updated_at
		        FROM work_orders
		       WHERE tenant_id = $1`
		args := []any{tenantID}
		if status != "" {
			q += " AND status = $2"
			args = append(args, status)
		}
		q += " ORDER BY created_at DESC"
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("manufacturing: list work orders: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var wo WorkOrder
			if err := scanWorkOrder(rows, &wo); err != nil {
				return err
			}
			out = append(out, wo)
		}
		return rows.Err()
	})
	return out, err
}

// scanWorkOrder is shared between Get and List so the column order is
// only specified once. The pgx.Row interface satisfies both
// QueryRow's Row and the Rows iterator's per-row Scan.
type pgxScanner interface {
	Scan(dest ...any) error
}

func scanWorkOrder(r pgxScanner, wo *WorkOrder) error {
	var bomID *uuid.UUID
	var routingID *uuid.UUID
	var actualQty *decimal.Decimal
	if err := r.Scan(
		&wo.TenantID, &wo.ID, &wo.ItemID, &bomID, &routingID, &wo.WarehouseID, &wo.PlannedQty, &actualQty, &wo.Status,
		&wo.ScheduledStart, &wo.ScheduledEnd, &wo.StartedAt, &wo.CompletedAt,
		&wo.Notes, &wo.CreatedBy, &wo.CreatedAt, &wo.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkOrderNotFound
		}
		return fmt.Errorf("manufacturing: scan work order: %w", err)
	}
	wo.BOMID = bomID
	wo.RoutingID = routingID
	wo.ActualQty = actualQty
	return nil
}

// nullableString returns nil for empty strings so the SQL driver
// writes NULL instead of ” — preserves the "is the column unset?"
// signal on read paths.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableUUID returns nil for the zero UUID so the SQL driver
// writes NULL.
func nullableUUID(u uuid.UUID) any {
	if u == uuid.Nil {
		return nil
	}
	return u
}

// isUniqueViolation returns true iff err is a Postgres
// unique-constraint violation (SQLSTATE 23505). Used to convert a raw
// 23505 into a typed domain error so the HTTP layer can map it to 409.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// isForeignKeyViolation returns true iff err is a Postgres
// foreign-key-constraint violation (SQLSTATE 23503). A routing
// operation referencing a non-existent work center trips this.
func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23503"
	}
	return false
}

// ===========================================================================
// Work centers (Stream 2 — manufacturing depth)
// ===========================================================================

// CreateWorkCenterInput is the canonical input for CreateWorkCenter.
// OperatingHoursPerDay and EfficiencyPercent fall back to sensible
// defaults (8h, 100%) when left zero so a caller can stand up a work
// center with just a name.
type CreateWorkCenterInput struct {
	Name                 string
	CapacityPerHour      decimal.Decimal
	OperatingHoursPerDay decimal.Decimal
	EfficiencyPercent    decimal.Decimal
	Notes                string
}

// CreateWorkCenter inserts an active work center. Returns
// ErrWorkCenterDuplicateName when the (tenant_id, name) unique
// constraint is violated.
func (s *PGStore) CreateWorkCenter(ctx context.Context, tenantID, actorID uuid.UUID, in CreateWorkCenterInput) (*WorkCenter, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.Name == "" {
		return nil, fmt.Errorf("%w: work center name required", ErrInvalidInput)
	}
	if in.CapacityPerHour.IsNegative() {
		return nil, fmt.Errorf("%w: capacity_per_hour must be >= 0", ErrInvalidInput)
	}
	hours := in.OperatingHoursPerDay
	if hours.IsZero() {
		hours = decimal.NewFromInt(8)
	}
	if hours.IsNegative() || hours.GreaterThan(decimal.NewFromInt(24)) {
		return nil, fmt.Errorf("%w: operating_hours_per_day must be in (0, 24]", ErrInvalidInput)
	}
	eff := in.EfficiencyPercent
	if eff.IsZero() {
		eff = decimal.NewFromInt(100)
	}
	if eff.IsNegative() {
		return nil, fmt.Errorf("%w: efficiency_percent must be > 0", ErrInvalidInput)
	}

	now := s.now()
	wc := &WorkCenter{
		TenantID:             tenantID,
		ID:                   uuid.New(),
		Name:                 in.Name,
		CapacityPerHour:      in.CapacityPerHour,
		OperatingHoursPerDay: hours,
		EfficiencyPercent:    eff,
		Status:               WorkCenterStatusActive,
		Notes:                in.Notes,
		CreatedBy:            actorID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO work_centers
			     (tenant_id, id, name, capacity_per_hour, operating_hours_per_day,
			      efficiency_percent, status, notes, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)`,
			wc.TenantID, wc.ID, wc.Name, wc.CapacityPerHour, wc.OperatingHoursPerDay,
			wc.EfficiencyPercent, wc.Status, nullableString(wc.Notes), nullableUUID(wc.CreatedBy), wc.CreatedAt,
		)
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", ErrWorkCenterDuplicateName, wc.Name)
			}
			return fmt.Errorf("manufacturing: insert work center: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return wc, nil
}

// GetWorkCenter fetches a single work center.
func (s *PGStore) GetWorkCenter(ctx context.Context, tenantID, id uuid.UUID) (*WorkCenter, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and work center id required")
	}
	var wc WorkCenter
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return scanWorkCenter(tx.QueryRow(ctx, workCenterSelectColumns+
			` FROM work_centers WHERE tenant_id = $1 AND id = $2`, tenantID, id), &wc)
	})
	if err != nil {
		return nil, err
	}
	return &wc, nil
}

// ListWorkCenters returns the tenant's work centers, optionally
// filtered by status, ordered by name.
func (s *PGStore) ListWorkCenters(ctx context.Context, tenantID uuid.UUID, status string) ([]WorkCenter, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	out := make([]WorkCenter, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := workCenterSelectColumns + ` FROM work_centers WHERE tenant_id = $1`
		args := []any{tenantID}
		if status != "" {
			q += " AND status = $2"
			args = append(args, status)
		}
		q += " ORDER BY name"
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("manufacturing: list work centers: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var wc WorkCenter
			if err := scanWorkCenter(rows, &wc); err != nil {
				return err
			}
			out = append(out, wc)
		}
		return rows.Err()
	})
	return out, err
}

// SetWorkCenterStatus flips a work center between active / maintenance
// / retired. Any of the three is reachable from any other (unlike the
// monotone BOM / routing lifecycles) — a center comes back from
// maintenance, and a retired center can be reinstated — so there is no
// transition matrix to enforce beyond validating the enum.
func (s *PGStore) SetWorkCenterStatus(ctx context.Context, tenantID, id uuid.UUID, status string) error {
	switch status {
	case WorkCenterStatusActive, WorkCenterStatusMaintenance, WorkCenterStatusRetired:
	default:
		return fmt.Errorf("%w: invalid work center status %q", ErrInvalidInput, status)
	}
	if tenantID == uuid.Nil || id == uuid.Nil {
		return errors.New("manufacturing: tenant id and work center id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE work_centers SET status = $3, updated_at = $4
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, status, s.now(),
		)
		if err != nil {
			return fmt.Errorf("manufacturing: update work center status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrWorkCenterNotFound
		}
		return nil
	})
}

const workCenterSelectColumns = `SELECT tenant_id, id, name, capacity_per_hour, operating_hours_per_day,
        efficiency_percent, status, COALESCE(notes, ''),
        COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
        created_at, updated_at`

func scanWorkCenter(r pgxScanner, wc *WorkCenter) error {
	if err := r.Scan(
		&wc.TenantID, &wc.ID, &wc.Name, &wc.CapacityPerHour, &wc.OperatingHoursPerDay,
		&wc.EfficiencyPercent, &wc.Status, &wc.Notes, &wc.CreatedBy, &wc.CreatedAt, &wc.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkCenterNotFound
		}
		return fmt.Errorf("manufacturing: scan work center: %w", err)
	}
	return nil
}

// ===========================================================================
// Routings (Stream 2 — manufacturing depth)
// ===========================================================================

// RoutingOperationInput is one step on a CreateRouting call. Sequence
// is derived from the array position (1-based) — the store assigns it,
// mirroring the BOM-component contract — so the input intentionally
// omits a sequence field.
type RoutingOperationInput struct {
	OperationName    string
	WorkCenterID     uuid.UUID
	SetupTimeMinutes decimal.Decimal
	CycleTimeMinutes decimal.Decimal
	Description      string
}

// CreateRoutingInput is the canonical input for CreateRouting.
type CreateRoutingInput struct {
	ItemID     uuid.UUID
	Version    string
	Notes      string
	Operations []RoutingOperationInput
	// Activate, when true, flips the freshly-created routing from
	// draft to active in the same transaction (auto-demoting any
	// previously-active routing for the item), the same convenience
	// CreateBOM offers.
	Activate bool
}

// CreateRouting inserts a routing and its operations. The operation
// order in the input slice IS the routing's execution order — the
// store assigns sequence = (index + 1). A routing with zero operations
// is rejected (ErrRoutingHasNoOperations) because it would generate no
// job cards and carry no capacity load.
func (s *PGStore) CreateRouting(ctx context.Context, tenantID, actorID uuid.UUID, in CreateRoutingInput) (*Routing, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.ItemID == uuid.Nil {
		return nil, fmt.Errorf("%w: item_id required", ErrInvalidInput)
	}
	if in.Version == "" {
		return nil, fmt.Errorf("%w: version required", ErrInvalidInput)
	}
	if len(in.Operations) == 0 {
		return nil, ErrRoutingHasNoOperations
	}
	for i, op := range in.Operations {
		if op.OperationName == "" {
			return nil, fmt.Errorf("%w: operation %d missing operation_name", ErrInvalidInput, i+1)
		}
		if op.WorkCenterID == uuid.Nil {
			return nil, fmt.Errorf("%w: operation %d missing work_center_id", ErrInvalidInput, i+1)
		}
		if op.SetupTimeMinutes.IsNegative() || op.CycleTimeMinutes.IsNegative() {
			return nil, fmt.Errorf("%w: operation %d times must be >= 0", ErrInvalidInput, i+1)
		}
	}

	now := s.now()
	routing := &Routing{
		TenantID:  tenantID,
		ID:        uuid.New(),
		ItemID:    in.ItemID,
		Version:   in.Version,
		Status:    RoutingStatusDraft,
		Notes:     in.Notes,
		CreatedBy: actorID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO routings
			     (tenant_id, id, item_id, version, status, notes, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
			routing.TenantID, routing.ID, routing.ItemID, routing.Version, routing.Status,
			nullableString(routing.Notes), nullableUUID(routing.CreatedBy), routing.CreatedAt,
		); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: routing version %q already exists for item", ErrInvalidInput, routing.Version)
			}
			return fmt.Errorf("manufacturing: insert routing: %w", err)
		}
		for i, op := range in.Operations {
			seq := i + 1
			if _, err := tx.Exec(ctx,
				`INSERT INTO routing_operations
				     (tenant_id, routing_id, sequence, operation_name, work_center_id,
				      setup_time_minutes, cycle_time_minutes, description)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				tenantID, routing.ID, seq, op.OperationName, op.WorkCenterID,
				op.SetupTimeMinutes, op.CycleTimeMinutes, nullableString(op.Description),
			); err != nil {
				if isForeignKeyViolation(err) {
					return fmt.Errorf("%w: operation %d references a work center that does not exist", ErrInvalidInput, seq)
				}
				return fmt.Errorf("manufacturing: insert routing operation %d: %w", seq, err)
			}
			routing.Operations = append(routing.Operations, RoutingOperation{
				RoutingID:        routing.ID,
				Sequence:         seq,
				OperationName:    op.OperationName,
				WorkCenterID:     op.WorkCenterID,
				SetupTimeMinutes: op.SetupTimeMinutes,
				CycleTimeMinutes: op.CycleTimeMinutes,
				Description:      op.Description,
			})
		}
		if in.Activate {
			if err := activateRoutingInTx(ctx, tx, tenantID, routing.ID, routing.ItemID, s.now); err != nil {
				return err
			}
			routing.Status = RoutingStatusActive
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return routing, nil
}

// activateRoutingInTx serialises routing activations against the
// routings_active_per_item_uniq invariant inside an existing
// transaction, mirroring activateBOMInTx. The advisory lock is keyed
// on a "routing:" prefix so it can't collide with the BOM activation
// lock for the same (tenant, item).
func activateRoutingInTx(ctx context.Context, tx pgx.Tx, tenantID, routingID, itemID uuid.UUID, now func() time.Time) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(
		            hashtextextended('routing:' || $1::text || ':' || $2::text, 0))`,
		tenantID, itemID,
	); err != nil {
		return fmt.Errorf("manufacturing: acquire routing activation lock: %w", err)
	}
	ts := now()
	if _, err := tx.Exec(ctx,
		`UPDATE routings
		    SET status = 'obsolete', updated_at = $3
		  WHERE tenant_id = $1 AND item_id = $2
		    AND status = 'active' AND id <> $4`,
		tenantID, itemID, ts, routingID,
	); err != nil {
		return fmt.Errorf("manufacturing: demote previous active routing: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE routings SET status = 'active', updated_at = $3
		  WHERE tenant_id = $1 AND id = $2`,
		tenantID, routingID, ts,
	); err != nil {
		return fmt.Errorf("manufacturing: update routing status: %w", err)
	}
	return nil
}

// GetRouting fetches a routing and its operations.
func (s *PGStore) GetRouting(ctx context.Context, tenantID, id uuid.UUID) (*Routing, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and routing id required")
	}
	var routing Routing
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := scanRouting(tx.QueryRow(ctx, routingSelectColumns+
			` FROM routings WHERE tenant_id = $1 AND id = $2`, tenantID, id), &routing); err != nil {
			return err
		}
		ops, err := loadRoutingOperations(ctx, tx, tenantID, id)
		if err != nil {
			return err
		}
		routing.Operations = ops
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &routing, nil
}

// ListRoutings returns routing headers for a tenant, optionally
// filtered by status. Operations are NOT loaded; callers that need the
// steps follow up with GetRouting.
func (s *PGStore) ListRoutings(ctx context.Context, tenantID uuid.UUID, status string) ([]Routing, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	out := make([]Routing, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := routingSelectColumns + ` FROM routings WHERE tenant_id = $1`
		args := []any{tenantID}
		if status != "" {
			q += " AND status = $2"
			args = append(args, status)
		}
		q += " ORDER BY item_id, version"
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("manufacturing: list routings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r Routing
			if err := scanRouting(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// SetRoutingStatus transitions a routing between draft / active /
// obsolete. Activating auto-demotes any other active routing for the
// same item (so routings_active_per_item_uniq never collides) and
// requires the routing to have at least one operation. Illegal
// transitions return ErrRoutingInvalidTransition.
func (s *PGStore) SetRoutingStatus(ctx context.Context, tenantID, id uuid.UUID, status string) error {
	switch status {
	case RoutingStatusDraft, RoutingStatusActive, RoutingStatusObsolete:
	default:
		return fmt.Errorf("%w: invalid routing status %q", ErrInvalidInput, status)
	}
	if tenantID == uuid.Nil || id == uuid.Nil {
		return errors.New("manufacturing: tenant id and routing id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var current Routing
		if err := scanRouting(tx.QueryRow(ctx, routingSelectColumns+
			` FROM routings WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, id), &current); err != nil {
			return err
		}
		if !current.CanTransitionTo(status) {
			return fmt.Errorf("%w: %s → %s", ErrRoutingInvalidTransition, current.Status, status)
		}
		if current.Status == status {
			return nil
		}
		if status == RoutingStatusActive {
			var opCount int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM routing_operations WHERE tenant_id = $1 AND routing_id = $2`,
				tenantID, id,
			).Scan(&opCount); err != nil {
				return fmt.Errorf("manufacturing: count routing operations: %w", err)
			}
			if opCount == 0 {
				return ErrRoutingHasNoOperations
			}
			return activateRoutingInTx(ctx, tx, tenantID, id, current.ItemID, s.now)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE routings SET status = $1, updated_at = $4 WHERE tenant_id = $2 AND id = $3`,
			status, tenantID, id, s.now(),
		); err != nil {
			return fmt.Errorf("manufacturing: update routing status: %w", err)
		}
		return nil
	})
}

// activeRoutingForItem returns the unique active routing for an item
// with its operations loaded. Returns ErrRoutingNotActive when the item
// has no active routing — ReleaseWorkOrder treats that as "BOM-only,
// generate no job cards" rather than a hard error.
func (s *PGStore) activeRoutingForItem(ctx context.Context, tx pgx.Tx, tenantID, itemID uuid.UUID) (*Routing, error) {
	var routing Routing
	if err := scanRouting(tx.QueryRow(ctx, routingSelectColumns+
		` FROM routings WHERE tenant_id = $1 AND item_id = $2 AND status = 'active'`,
		tenantID, itemID), &routing); err != nil {
		if errors.Is(err, ErrRoutingNotFound) {
			return nil, ErrRoutingNotActive
		}
		return nil, err
	}
	ops, err := loadRoutingOperations(ctx, tx, tenantID, routing.ID)
	if err != nil {
		return nil, err
	}
	routing.Operations = ops
	return &routing, nil
}

// loadRoutingOperations reads a routing's operations ordered by
// sequence. Shared between GetRouting and activeRoutingForItem.
func loadRoutingOperations(ctx context.Context, tx pgx.Tx, tenantID, routingID uuid.UUID) ([]RoutingOperation, error) {
	rows, err := tx.Query(ctx,
		`SELECT routing_id, sequence, operation_name, work_center_id,
		        setup_time_minutes, cycle_time_minutes, COALESCE(description, '')
		   FROM routing_operations
		  WHERE tenant_id = $1 AND routing_id = $2
		  ORDER BY sequence`,
		tenantID, routingID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select routing operations: %w", err)
	}
	defer rows.Close()
	ops := make([]RoutingOperation, 0)
	for rows.Next() {
		var op RoutingOperation
		if err := rows.Scan(&op.RoutingID, &op.Sequence, &op.OperationName, &op.WorkCenterID,
			&op.SetupTimeMinutes, &op.CycleTimeMinutes, &op.Description); err != nil {
			return nil, fmt.Errorf("manufacturing: scan routing operation: %w", err)
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

const routingSelectColumns = `SELECT tenant_id, id, item_id, version, status, COALESCE(notes, ''),
        COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
        created_at, updated_at`

func scanRouting(r pgxScanner, routing *Routing) error {
	if err := r.Scan(
		&routing.TenantID, &routing.ID, &routing.ItemID, &routing.Version, &routing.Status,
		&routing.Notes, &routing.CreatedBy, &routing.CreatedAt, &routing.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRoutingNotFound
		}
		return fmt.Errorf("manufacturing: scan routing: %w", err)
	}
	return nil
}
