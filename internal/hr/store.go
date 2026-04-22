package hr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// LeaveLedgerEntry is one row in `leave_ledger`: either a positive
// accrual or a negative deduction driven by a posted leave request.
// Rows are append-only (no UPDATE/DELETE) so the SUM projection in
// `leave_balances` is immune to KRecord edits.
type LeaveLedgerEntry struct {
	ID          int64           `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	EmployeeID  uuid.UUID       `json:"employee_id"`
	LeaveType   string          `json:"leave_type"`
	DeltaDays   decimal.Decimal `json:"delta_days"`
	EffectiveOn time.Time       `json:"effective_on"`
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID      `json:"source_id,omitempty"`
	Memo        string          `json:"memo,omitempty"`
	CreatedBy   uuid.UUID       `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Balance is the projected remaining days per employee/leave_type.
type Balance struct {
	EmployeeID  uuid.UUID       `json:"employee_id"`
	LeaveType   string          `json:"leave_type"`
	BalanceDays decimal.Decimal `json:"balance_days"`
}

// ErrDuplicateLeaveSource is surfaced when the partial unique index on
// `leave_ledger (tenant_id, source_ktype, source_id)` rejects a second
// posting for the same leave request — normal on worker retries.
var ErrDuplicateLeaveSource = errors.New("hr: leave ledger already has an entry for this source")

// Store is the narrow persistence surface used by HR agent tools and
// integration tests; the employee/leave_request KRecords themselves
// stay in the generic record.PGStore.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store backed by the shared pool. Callers must
// still establish tenant context (SET LOCAL app.tenant_id) via
// platform.WithTenantTx; the Store does so automatically for its
// mutations.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// AppendLeaveLedger inserts one row. If source_id is set, a duplicate
// (tenant_id, source_ktype, source_id) collides with the partial
// unique index and surfaces as ErrDuplicateLeaveSource so callers can
// treat retries as no-ops.
func (s *Store) AppendLeaveLedger(ctx context.Context, entry LeaveLedgerEntry) (*LeaveLedgerEntry, error) {
	if entry.TenantID == uuid.Nil || entry.EmployeeID == uuid.Nil {
		return nil, errors.New("hr: tenant_id and employee_id required")
	}
	if entry.LeaveType == "" {
		return nil, errors.New("hr: leave_type required")
	}
	if entry.CreatedBy == uuid.Nil {
		return nil, errors.New("hr: created_by required")
	}
	if entry.EffectiveOn.IsZero() {
		entry.EffectiveOn = time.Now().UTC()
	}
	var out LeaveLedgerEntry
	err := platform.WithTenantTx(ctx, s.pool, entry.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO leave_ledger
			    (tenant_id, employee_id, leave_type, delta_days,
			     effective_on, source_ktype, source_id, memo, created_by)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 RETURNING id, created_at`,
			entry.TenantID, entry.EmployeeID, entry.LeaveType, entry.DeltaDays,
			entry.EffectiveOn, nullIfEmpty(entry.SourceKType), entry.SourceID,
			nullIfEmpty(entry.Memo), entry.CreatedBy,
		)
		out = entry
		if err := row.Scan(&out.ID, &out.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateLeaveSource
			}
			return fmt.Errorf("insert leave_ledger: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// LeaveBalance returns the SUM(delta_days) for an employee/leave_type.
func (s *Store) LeaveBalance(ctx context.Context, tenantID, employeeID uuid.UUID, leaveType string) (decimal.Decimal, error) {
	var bal decimal.Decimal
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(delta_days), 0)
			   FROM leave_ledger
			  WHERE tenant_id = $1 AND employee_id = $2 AND leave_type = $3`,
			tenantID, employeeID, leaveType,
		)
		return row.Scan(&bal)
	})
	return bal, err
}

// ListBalances returns every (employee_id, leave_type) balance for
// the tenant. Useful for HR dashboards and the /hr agent surface.
func (s *Store) ListBalances(ctx context.Context, tenantID uuid.UUID) ([]Balance, error) {
	var out []Balance
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT employee_id, leave_type, balance_days
			   FROM leave_balances
			  WHERE tenant_id = $1
			  ORDER BY employee_id, leave_type`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b Balance
			if err := rows.Scan(&b.EmployeeID, &b.LeaveType, &b.BalanceDays); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
