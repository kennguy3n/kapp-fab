package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// CycleCountStatusDraft and friends are the lifecycle states of a
// cycle-count session. Posting is gated on `reconciled`, and `posted`
// is terminal — once posted the session is read-only.
const (
	CycleCountStatusDraft      = "draft"
	CycleCountStatusCounting   = "counting"
	CycleCountStatusReconciled = "reconciled"
	CycleCountStatusPosted     = "posted"
)

// MoveSourceCycleCount is the source_ktype value used for the
// variance inventory_moves emitted at post time. Each line's
// move is keyed on (MoveSourceCycleCount, line.id) so the
// inventory_moves_source_uniq partial index folds retries.
const MoveSourceCycleCount = "inventory.cycle_count"

// Sentinel errors surfaced by the cycle-count store. They map to
// 4xx HTTP responses at the handler layer.
var (
	ErrCycleCountBadStatus      = errors.New("cycle_count: invalid status transition")
	ErrCycleCountNotFound       = errors.New("cycle_count: session not found")
	ErrCycleCountAlreadyPosted  = errors.New("cycle_count: session already posted")
	ErrCycleCountNotReconciled  = errors.New("cycle_count: session must be reconciled before post")
	ErrCycleCountLineFrozen     = errors.New("cycle_count: reopen session before editing lines")
	ErrCycleCountLineNotFound   = errors.New("cycle_count: line not found")
	ErrCycleCountWarehouseEmpty = errors.New("cycle_count: warehouse_id required")
	ErrCycleCountCodeEmpty      = errors.New("cycle_count: code required")
)

// CycleCountSession is the header row.
type CycleCountSession struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	ID          uuid.UUID  `json:"id"`
	Code        string     `json:"code"`
	Description string     `json:"description,omitempty"`
	WarehouseID uuid.UUID  `json:"warehouse_id"`
	Status      string     `json:"status"`
	CreatedBy   uuid.UUID  `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	PostedAt    *time.Time `json:"posted_at,omitempty"`
}

// CycleCountLine is one item counted within a session.
type CycleCountLine struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	ID          uuid.UUID       `json:"id"`
	SessionID   uuid.UUID       `json:"session_id"`
	ItemID      uuid.UUID       `json:"item_id"`
	ExpectedQty decimal.Decimal `json:"expected_qty"`
	CountedQty  decimal.Decimal `json:"counted_qty"`
	Variance    decimal.Decimal `json:"variance"`
	Notes       string          `json:"notes,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// CycleCountFilter narrows ListSessions.
type CycleCountFilter struct {
	Status      string
	WarehouseID uuid.UUID
	Limit       int
}

// CycleCountStore is the persistence + posting surface.
type CycleCountStore struct {
	pool *pgxpool.Pool
	inv  *PGStore
	now  func() time.Time
}

// NewCycleCountStore binds a store to the given pool and inventory
// PGStore (used at post time to write variance moves through the
// canonical RecordMove path so the audit log + outbox events fire).
func NewCycleCountStore(pool *pgxpool.Pool, inv *PGStore) *CycleCountStore {
	return &CycleCountStore{
		pool: pool,
		inv:  inv,
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// WithClock injects a deterministic clock for tests.
func (s *CycleCountStore) WithClock(now func() time.Time) *CycleCountStore {
	if now != nil {
		s.now = now
	}
	return s
}

// ---------------------------------------------------------------------------
// Session CRUD
// ---------------------------------------------------------------------------

// CreateSession inserts a draft session header.
func (s *CycleCountStore) CreateSession(ctx context.Context, in CycleCountSession) (*CycleCountSession, error) {
	if in.TenantID == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id required")
	}
	if strings.TrimSpace(in.Code) == "" {
		return nil, ErrCycleCountCodeEmpty
	}
	if in.WarehouseID == uuid.Nil {
		return nil, ErrCycleCountWarehouseEmpty
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	in.Status = CycleCountStatusDraft
	now := s.now()
	in.CreatedAt = now
	in.UpdatedAt = now

	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO cycle_count_sessions
				(tenant_id, id, code, description, warehouse_id, status,
				 created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			in.TenantID, in.ID, in.Code, in.Description, in.WarehouseID,
			in.Status, in.CreatedBy, in.CreatedAt, in.UpdatedAt,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("cycle_count: create session: %w", err)
	}
	return &in, nil
}

// GetSession returns a single session header.
func (s *CycleCountStore) GetSession(ctx context.Context, tenantID, id uuid.UUID) (*CycleCountSession, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id + id required")
	}
	var out CycleCountSession
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT tenant_id, id, code, description, warehouse_id, status,
			        created_by, created_at, updated_at, posted_at
			   FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&out.TenantID, &out.ID, &out.Code, &out.Description, &out.WarehouseID,
			&out.Status, &out.CreatedBy, &out.CreatedAt, &out.UpdatedAt, &out.PostedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCycleCountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cycle_count: get session: %w", err)
	}
	return &out, nil
}

// ListSessions returns sessions matching the filter, newest first.
func (s *CycleCountStore) ListSessions(ctx context.Context, tenantID uuid.UUID, f CycleCountFilter) ([]CycleCountSession, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id required")
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var out []CycleCountSession
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, code, description, warehouse_id, status,
			        created_by, created_at, updated_at, posted_at
			   FROM cycle_count_sessions
			  WHERE tenant_id = $1
			    AND ($2 = '' OR status = $2)
			    AND ($3 = '00000000-0000-0000-0000-000000000000'::uuid OR warehouse_id = $3)
			  ORDER BY created_at DESC
			  LIMIT $4`,
			tenantID, f.Status, f.WarehouseID, limit,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r CycleCountSession
			if err := rows.Scan(
				&r.TenantID, &r.ID, &r.Code, &r.Description, &r.WarehouseID,
				&r.Status, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &r.PostedAt,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("cycle_count: list sessions: %w", err)
	}
	return out, nil
}

// UpdateSession patches mutable fields (code, description,
// warehouse_id) and optionally transitions status. Posted sessions
// are immutable. The warehouse_id is treated as a "patch if provided"
// field: a zero-valued (uuid.Nil) WarehouseID in the request means
// "keep the current warehouse", so callers that don't need to move
// the session between warehouses can omit it. Re-assigning a
// non-empty draft to a different warehouse is allowed but invalidates
// previously seeded expected quantities; callers that change the
// warehouse should re-seed before posting.
func (s *CycleCountStore) UpdateSession(ctx context.Context, in CycleCountSession) (*CycleCountSession, error) {
	if in.TenantID == uuid.Nil || in.ID == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id + id required")
	}
	// Mirror CreateSession's upfront Code validation so the operator
	// sees the canonical sentinel error rather than a raw Postgres
	// constraint violation from cycle_count_sessions_code_not_blank_chk.
	// The DB CHECK stays in place as defence-in-depth for direct
	// SQL writes, but the user-facing surface should never reach it.
	if strings.TrimSpace(in.Code) == "" {
		return nil, ErrCycleCountCodeEmpty
	}
	now := s.now()
	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var current CycleCountSession
		if err := tx.QueryRow(ctx,
			`SELECT status, warehouse_id FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			in.TenantID, in.ID,
		).Scan(&current.Status, &current.WarehouseID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCycleCountNotFound
			}
			return err
		}
		if current.Status == CycleCountStatusPosted {
			return ErrCycleCountAlreadyPosted
		}
		status := in.Status
		if status == "" {
			status = current.Status
		}
		if !isKnownCycleCountStatus(status) {
			return ErrCycleCountBadStatus
		}
		if !canTransitionCycleCount(current.Status, status) {
			return ErrCycleCountBadStatus
		}
		// "Patch if provided": a Nil WarehouseID in the request
		// means the caller did not include `warehouse_id` in the
		// PATCH body, so we preserve the row's current value
		// instead of zeroing it out (the column is NOT NULL).
		warehouseID := in.WarehouseID
		if warehouseID == uuid.Nil {
			warehouseID = current.WarehouseID
		}
		// Re-pointing the warehouse on a reconciled session is
		// invalid: the expected_qty on every line was seeded from
		// the previous warehouse, but UpsertLine /
		// SeedExpectedFromStock both reject while reconciled, so
		// the operator cannot bring the line set into agreement
		// with the new warehouse without first reopening to
		// `counting`. Allowing the change would let an API caller
		// freeze a session pointing at warehouse A's seeded
		// expected_qty, swap the pointer to warehouse B, then post
		// — emitting variance moves against B from quantities that
		// describe A. The frontend already always sends the
		// existing warehouse_id; this guard closes the direct-API
		// path. Re-pointing remains free while the session is in
		// `draft` or `counting` (callers are expected to re-seed
		// afterwards; that's now documented on the function).
		if current.Status == CycleCountStatusReconciled && warehouseID != current.WarehouseID {
			return ErrCycleCountBadStatus
		}
		_, err := tx.Exec(ctx,
			`UPDATE cycle_count_sessions
			    SET code = $1, description = $2, warehouse_id = $3,
			        status = $4, updated_at = $5
			  WHERE tenant_id = $6 AND id = $7`,
			in.Code, in.Description, warehouseID, status, now, in.TenantID, in.ID,
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetSession(ctx, in.TenantID, in.ID)
}

// DeleteSession removes a draft session and its lines. Posted /
// reconciled sessions are not removable.
func (s *CycleCountStore) DeleteSession(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return errors.New("cycle_count: tenant id + id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, id,
		).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCycleCountNotFound
			}
			return err
		}
		if status != CycleCountStatusDraft {
			return ErrCycleCountBadStatus
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		return err
	})
}

// ---------------------------------------------------------------------------
// Line CRUD
// ---------------------------------------------------------------------------

// UpsertLine inserts or updates a count line. The `variance` column
// is DB-computed (STORED), so callers only set expected/counted.
//
// The store branches on whether the caller supplied an explicit
// line id:
//
//   - No id (add-line flow): the row is keyed on the
//     `cycle_count_lines_session_item_uniq` index over (tenant_id,
//     session_id, item_id). If a line already exists for that
//     (session, item) — typically because SeedExpectedFromStock
//     created it — the INSERT folds into a DO UPDATE on the existing
//     row, returning its id. This makes the add-line UI workflow
//     idempotent for re-counts of the same item without raising a
//     raw unique-violation 500.
//   - Explicit id (edit-line flow): the row is keyed on the primary
//     key (tenant_id, id) and the existing row's columns are
//     refreshed. The edit path does NOT touch `expected_qty` — it is
//     owned by SeedExpectedFromStock so a stale `in.ExpectedQty` from
//     a frontend snapshot that pre-dates a concurrent re-seed cannot
//     overwrite the fresh expected_qty back to the old value. To
//     refresh expected_qty after data drift the operator must
//     explicitly re-seed.
//
// `RETURNING id` lets the caller see the row id the database
// settled on after the ON CONFLICT alternative path, which can
// differ from the freshly-generated UUID the caller initially
// passed in.
func (s *CycleCountStore) UpsertLine(ctx context.Context, in CycleCountLine) (*CycleCountLine, error) {
	if in.TenantID == uuid.Nil || in.SessionID == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id + session id required")
	}
	if in.ItemID == uuid.Nil {
		return nil, errors.New("cycle_count: item_id required")
	}
	callerSuppliedID := in.ID != uuid.Nil
	if !callerSuppliedID {
		in.ID = uuid.New()
	}
	now := s.now()
	in.CreatedAt = now
	in.UpdatedAt = now

	err := dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			in.TenantID, in.SessionID,
		).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCycleCountNotFound
			}
			return err
		}
		if status == CycleCountStatusPosted {
			return ErrCycleCountAlreadyPosted
		}
		// Once a session has advanced to `reconciled` its lines are
		// frozen relative to a posting attempt. PostSession now reads
		// the lines inside the same FOR UPDATE tx as the status flip,
		// so a concurrent line edit cannot interleave with an
		// in-flight post. The freeze is still load-bearing for the
		// "counted vs posted" invariant the auditor relies on: the
		// `reconciled` status declares "the line set the operator
		// signed off on", and silently mutating a line under that
		// status would let the audit trail show a different value
		// than the move that posts to the ledger. The state machine
		// allows `reconciled -> counting`, so an operator who needs
		// to amend a count must explicitly reopen the session first.
		if status == CycleCountStatusReconciled {
			return ErrCycleCountLineFrozen
		}
		// The two flows (add-line and edit-line) differ in their
		// conflict target and in whether `expected_qty` is allowed
		// to update an existing row.
		//
		//   * add-line (no caller-supplied id) keys on
		//     (tenant_id, session_id, item_id) so the row created
		//     by SeedExpectedFromStock is reused. We must not let
		//     a fresh `in.ExpectedQty` clobber the seeded value, so
		//     the DO UPDATE refreshes only counted_qty + notes.
		//   * edit-line (explicit id) keys on (tenant_id, id) and
		//     touches counted_qty + notes only — same rationale.
		//
		// Both branches still INSERT `in.ExpectedQty` for the
		// genuine first-time-insert path (a manually added line
		// that has no seed row yet); the DO UPDATE branch is the
		// one that ignores it.
		var query string
		if callerSuppliedID {
			query = `INSERT INTO cycle_count_lines
					(tenant_id, id, session_id, item_id, expected_qty,
					 counted_qty, notes, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 ON CONFLICT (tenant_id, id) DO UPDATE SET
					counted_qty = EXCLUDED.counted_qty,
					notes       = EXCLUDED.notes,
					updated_at  = EXCLUDED.updated_at
				 RETURNING id`
		} else {
			query = `INSERT INTO cycle_count_lines
					(tenant_id, id, session_id, item_id, expected_qty,
					 counted_qty, notes, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 ON CONFLICT (tenant_id, session_id, item_id) DO UPDATE SET
					counted_qty = EXCLUDED.counted_qty,
					notes       = EXCLUDED.notes,
					updated_at  = EXCLUDED.updated_at
				 RETURNING id`
		}
		var settledID uuid.UUID
		if err := tx.QueryRow(ctx, query,
			in.TenantID, in.ID, in.SessionID, in.ItemID, in.ExpectedQty,
			in.CountedQty, in.Notes, in.CreatedAt, in.UpdatedAt,
		).Scan(&settledID); err != nil {
			return err
		}
		in.ID = settledID
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cycle_count: upsert line: %w", err)
	}
	return s.getLine(ctx, in.TenantID, in.ID)
}

// getLine reloads a line so the caller sees the DB-computed variance.
func (s *CycleCountStore) getLine(ctx context.Context, tenantID, id uuid.UUID) (*CycleCountLine, error) {
	var out CycleCountLine
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT tenant_id, id, session_id, item_id, expected_qty,
			        counted_qty, variance, notes, created_at, updated_at
			   FROM cycle_count_lines
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&out.TenantID, &out.ID, &out.SessionID, &out.ItemID, &out.ExpectedQty,
			&out.CountedQty, &out.Variance, &out.Notes, &out.CreatedAt, &out.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCycleCountLineNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cycle_count: load line: %w", err)
	}
	return &out, nil
}

// DeleteLine removes a line.
func (s *CycleCountStore) DeleteLine(ctx context.Context, tenantID, sessionID, lineID uuid.UUID) error {
	if tenantID == uuid.Nil || sessionID == uuid.Nil || lineID == uuid.Nil {
		return errors.New("cycle_count: tenant id + session id + line id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, sessionID,
		).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCycleCountNotFound
			}
			return err
		}
		if status == CycleCountStatusPosted {
			return ErrCycleCountAlreadyPosted
		}
		// Mirror UpsertLine: lines are frozen while reconciled so a
		// concurrent post can't observe a half-deleted line set.
		if status == CycleCountStatusReconciled {
			return ErrCycleCountLineFrozen
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM cycle_count_lines
			  WHERE tenant_id = $1 AND session_id = $2 AND id = $3`,
			tenantID, sessionID, lineID,
		)
		return err
	})
}

// ListLines returns all lines for a session.
func (s *CycleCountStore) ListLines(ctx context.Context, tenantID, sessionID uuid.UUID) ([]CycleCountLine, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id + session id required")
	}
	var out []CycleCountLine
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, session_id, item_id, expected_qty,
			        counted_qty, variance, notes, created_at, updated_at
			   FROM cycle_count_lines
			  WHERE tenant_id = $1 AND session_id = $2
			  ORDER BY created_at ASC`,
			tenantID, sessionID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r CycleCountLine
			if err := rows.Scan(
				&r.TenantID, &r.ID, &r.SessionID, &r.ItemID, &r.ExpectedQty,
				&r.CountedQty, &r.Variance, &r.Notes, &r.CreatedAt, &r.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("cycle_count: list lines: %w", err)
	}
	return out, nil
}

// SeedExpectedFromStock fills `expected_qty` on every existing line
// (or inserts new lines for every item with non-zero stock in the
// session's warehouse) by reading the current stock_levels view.
// Idempotent — re-running refreshes expected_qty against the latest
// stock_levels.
func (s *CycleCountStore) SeedExpectedFromStock(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return errors.New("cycle_count: tenant id + session id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var session CycleCountSession
		if err := tx.QueryRow(ctx,
			`SELECT warehouse_id, status FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, sessionID,
		).Scan(&session.WarehouseID, &session.Status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCycleCountNotFound
			}
			return err
		}
		if session.Status == CycleCountStatusPosted {
			return ErrCycleCountAlreadyPosted
		}
		// Reconciled sessions are line-frozen: PostSession reads the
		// lines inside the same FOR UPDATE tx as the status flip, so
		// the freeze is load-bearing for audit-trail integrity (the
		// reconciled line set is exactly what posts to the ledger).
		// Mirror the UpsertLine guard so a seed cannot bump a
		// reconciled session's expected_qty — the operator must
		// reopen the session to counting first.
		if session.Status == CycleCountStatusReconciled {
			return ErrCycleCountLineFrozen
		}
		rows, err := tx.Query(ctx,
			`SELECT item_id, qty FROM stock_levels
			  WHERE tenant_id = $1 AND warehouse_id = $2`,
			tenantID, session.WarehouseID,
		)
		if err != nil {
			return err
		}
		type stockRow struct {
			ItemID uuid.UUID
			Qty    decimal.Decimal
		}
		var stock []stockRow
		for rows.Next() {
			var r stockRow
			if err := rows.Scan(&r.ItemID, &r.Qty); err != nil {
				rows.Close()
				return err
			}
			stock = append(stock, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := s.now()
		for _, r := range stock {
			// Conflict on (tenant_id, session_id, item_id) — the
			// unique index added in migration 000065. We must NOT
			// conflict on (tenant_id, id) because the candidate id
			// is freshly generated on every call and would never
			// collide, so re-seeding would insert one extra row per
			// item per call. On conflict refresh `expected_qty` so
			// re-seeding picks up concurrent inventory moves that
			// landed between create and seed time. `counted_qty` is
			// preserved because the operator may have already
			// entered values for some lines before realising they
			// wanted a fresh seed.
			if _, err := tx.Exec(ctx,
				`INSERT INTO cycle_count_lines
					(tenant_id, id, session_id, item_id, expected_qty,
					 counted_qty, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5, 0, $6, $7)
				 ON CONFLICT (tenant_id, session_id, item_id) DO UPDATE SET
					expected_qty = EXCLUDED.expected_qty,
					updated_at   = EXCLUDED.updated_at`,
				tenantID, uuid.New(), sessionID, r.ItemID, r.Qty, now, now,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Post
// ---------------------------------------------------------------------------

// PostSession walks every line with non-zero variance and writes
// one inventory_move per line. Variance > 0 adds stock; variance < 0
// removes stock. Each move is keyed on (MoveSourceCycleCount,
// line.id) so the inventory_moves_source_uniq partial index folds
// retries into no-ops. After the moves are written the session
// status is set to `posted` and `posted_at` is stamped.
//
// All four steps — locking the session header, reading lines,
// writing variance moves, and flipping the status — run inside a
// single tenant-scoped transaction. The session row is held
// `FOR UPDATE` for the duration of the post, which serialises against
// every line-mutating path (UpsertLine, DeleteLine,
// SeedExpectedFromStock all take the same lock) and against
// UpdateSession's status transitions. That eliminates the previous
// race window where moves committed in their own transactions before
// the status flip — a concurrent reconciled→counting transition could
// leave the ledger with a stale move keyed on
// (MoveSourceCycleCount, line.id) which would later be skipped as a
// duplicate when the operator re-counted and re-posted, leaving
// stock_levels diverged from the persisted counted_qty.
func (s *CycleCountStore) PostSession(ctx context.Context, tenantID, sessionID, actorID uuid.UUID) (*CycleCountSession, error) {
	if tenantID == uuid.Nil || sessionID == uuid.Nil {
		return nil, errors.New("cycle_count: tenant id + session id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("cycle_count: actor required")
	}

	// Fast path: an already-posted session is idempotent — return
	// the snapshot without re-opening the tx. The single-tx path
	// below handles the genuine reconciled→posted transition.
	if existing, err := s.GetSession(ctx, tenantID, sessionID); err != nil {
		return nil, err
	} else if existing.Status == CycleCountStatusPosted {
		return existing, nil
	}

	now := s.now()
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		var warehouseID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT status, warehouse_id FROM cycle_count_sessions
			  WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
			tenantID, sessionID,
		).Scan(&status, &warehouseID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCycleCountNotFound
			}
			return err
		}
		if status == CycleCountStatusPosted {
			// Lost the race to a concurrent post that completed
			// between our fast-path GetSession and the FOR UPDATE
			// lock. Treat as success; the caller will re-fetch.
			return nil
		}
		if status != CycleCountStatusReconciled {
			return ErrCycleCountNotReconciled
		}

		// Read lines inside the same tx — they cannot change under
		// us because every line-mutating method takes the session
		// FOR UPDATE lock we now hold. Only the columns the
		// variance-move writer needs are selected; the full
		// CycleCountLine projection lives in ListLines for read
		// surfaces.
		rows, err := tx.Query(ctx,
			`SELECT id, item_id, variance
			   FROM cycle_count_lines
			  WHERE tenant_id = $1 AND session_id = $2
			  ORDER BY created_at ASC`,
			tenantID, sessionID,
		)
		if err != nil {
			return fmt.Errorf("cycle_count: read lines for post: %w", err)
		}
		type postLine struct {
			ID       uuid.UUID
			ItemID   uuid.UUID
			Variance decimal.Decimal
		}
		var lines []postLine
		for rows.Next() {
			var ln postLine
			if err := rows.Scan(&ln.ID, &ln.ItemID, &ln.Variance); err != nil {
				rows.Close()
				return fmt.Errorf("cycle_count: scan line for post: %w", err)
			}
			lines = append(lines, ln)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("cycle_count: iterate lines for post: %w", err)
		}

		for i := range lines {
			ln := &lines[i]
			if ln.Variance.IsZero() {
				continue
			}
			lineID := ln.ID
			move := Move{
				TenantID:    tenantID,
				ItemID:      ln.ItemID,
				WarehouseID: warehouseID,
				Qty:         ln.Variance,
				SourceKType: MoveSourceCycleCount,
				SourceID:    &lineID,
				MovedAt:     now,
				CreatedBy:   actorID,
			}
			if _, err := s.inv.RecordMoveTx(ctx, tx, move); err != nil {
				if errors.Is(err, ErrDuplicateSourceMove) {
					// In the single-tx design a partial commit
					// (moves landed, status flip didn't) cannot
					// happen — either every write in the tx
					// reaches the WAL together or none of them
					// do. The remaining way to hit this branch
					// is an out-of-band writer that recorded an
					// inventory_move with the same
					// (source_ktype, source_id) tuple as one of
					// our cycle-count lines, e.g. a manual SQL
					// repair script. The defensive `continue`
					// preserves idempotence in that case: the
					// downstream ledger already has the variance
					// recorded so we should not abort the post.
					continue
				}
				return fmt.Errorf("cycle_count: record variance move for line %s: %w", ln.ID, err)
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE cycle_count_sessions
			    SET status = $1, posted_at = $2, updated_at = $2
			  WHERE tenant_id = $3 AND id = $4`,
			CycleCountStatusPosted, now, tenantID, sessionID,
		); err != nil {
			return fmt.Errorf("cycle_count: mark session posted: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetSession(ctx, tenantID, sessionID)
}

// ---------------------------------------------------------------------------
// State machine helpers
// ---------------------------------------------------------------------------

func isKnownCycleCountStatus(s string) bool {
	switch s {
	case CycleCountStatusDraft, CycleCountStatusCounting,
		CycleCountStatusReconciled, CycleCountStatusPosted:
		return true
	}
	return false
}

// canTransitionCycleCount enforces the lifecycle FSM that
// UpdateSession is allowed to drive:
//
//	draft       -> counting
//	counting    -> reconciled | draft   (advance or reopen)
//	reconciled  -> counting             (reopen only)
//	posted      -> (terminal, rejected upstream by UpdateSession)
//
// The reconciled -> posted edge is *not* in this table: posting is
// handled exclusively by PostSession so the variance-moves write
// + status flip happen as one optimistic-locked operation. Any
// caller that asks UpdateSession for status=posted is therefore
// rejected with ErrCycleCountBadStatus, which is the intended
// behaviour. The from==to identity short-circuit returns true so
// PATCHes that don't actually change status (e.g. a code/description
// edit) are no-ops on the status column.
func canTransitionCycleCount(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case CycleCountStatusDraft:
		return to == CycleCountStatusCounting
	case CycleCountStatusCounting:
		return to == CycleCountStatusReconciled || to == CycleCountStatusDraft
	case CycleCountStatusReconciled:
		return to == CycleCountStatusCounting
	}
	return false
}
