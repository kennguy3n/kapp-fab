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
}

// CreateBOM inserts a new BOM (status='draft') plus its component
// rows in a single transaction. The single-active-row invariant is
// not exercised here — callers must call SetBOMStatus to activate
// the BOM after CreateBOM returns successfully.
func (s *PGStore) CreateBOM(ctx context.Context, tenantID, actorID uuid.UUID, in CreateBOMInput) (*BOM, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.ItemID == uuid.Nil {
		return nil, errors.New("manufacturing: item_id required")
	}
	if in.Version == "" {
		return nil, errors.New("manufacturing: version required")
	}
	if in.OutputQty.IsZero() || in.OutputQty.IsNegative() {
		in.OutputQty = decimal.NewFromInt(1)
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
			return nil, fmt.Errorf("manufacturing: component %s qty must be > 0", c.ComponentItemID)
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bom, nil
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
// ErrBOMHasNoComponents.
func (s *PGStore) SetBOMStatus(ctx context.Context, tenantID, bomID uuid.UUID, status string) error {
	switch status {
	case BOMStatusDraft, BOMStatusActive, BOMStatusObsolete:
	default:
		return fmt.Errorf("manufacturing: invalid bom status %q", status)
	}
	if tenantID == uuid.Nil || bomID == uuid.Nil {
		return errors.New("manufacturing: tenant id and bom id required")
	}

	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var itemID uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT item_id FROM boms WHERE tenant_id = $1 AND id = $2`,
			tenantID, bomID,
		).Scan(&itemID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBOMNotFound
			}
			return fmt.Errorf("manufacturing: lookup bom: %w", err)
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
			// Demote any other active row for the same
			// item so the partial unique index can land
			// the new activation without a 23505.
			if _, err := tx.Exec(ctx,
				`UPDATE boms
				    SET status = 'obsolete', updated_at = now()
				  WHERE tenant_id = $1 AND item_id = $2
				    AND status = 'active' AND id <> $3`,
				tenantID, itemID, bomID,
			); err != nil {
				return fmt.Errorf("manufacturing: demote previous active bom: %w", err)
			}
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
		return nil, errors.New("manufacturing: item_id and warehouse_id required")
	}
	if in.PlannedQty.IsZero() || in.PlannedQty.IsNegative() {
		return nil, errors.New("manufacturing: planned_qty must be > 0")
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
			`SELECT tenant_id, id, item_id, bom_id, warehouse_id, planned_qty, actual_qty, status,
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
		q := `SELECT tenant_id, id, item_id, bom_id, warehouse_id, planned_qty, actual_qty, status,
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
	var actualQty *decimal.Decimal
	if err := r.Scan(
		&wo.TenantID, &wo.ID, &wo.ItemID, &bomID, &wo.WarehouseID, &wo.PlannedQty, &actualQty, &wo.Status,
		&wo.ScheduledStart, &wo.ScheduledEnd, &wo.StartedAt, &wo.CompletedAt,
		&wo.Notes, &wo.CreatedBy, &wo.CreatedAt, &wo.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkOrderNotFound
		}
		return fmt.Errorf("manufacturing: scan work order: %w", err)
	}
	wo.BOMID = bomID
	wo.ActualQty = actualQty
	return nil
}

// nullableString returns nil for empty strings so the SQL driver
// writes NULL instead of '' — preserves the "is the column unset?"
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
