package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
)

// UpsertPeriod creates or updates a fiscal period row. Callers use this
// to declare the period boundaries (e.g. calendar month) before locking.
// period_start / period_end are date-only; the caller should normalise
// the times to midnight UTC.
func (s *PGStore) UpsertPeriod(ctx context.Context, p FiscalPeriod) (*FiscalPeriod, error) {
	if p.TenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if p.PeriodStart.IsZero() || p.PeriodEnd.IsZero() {
		return nil, errors.New("ledger: period_start and period_end required")
	}
	if p.PeriodEnd.Before(p.PeriodStart) {
		return nil, errors.New("ledger: period_end before period_start")
	}
	out := p
	err := dbutil.WithTenantTx(ctx, s.pool, p.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO fiscal_periods
			     (tenant_id, period_start, period_end, locked, locked_at, locked_by)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (tenant_id, period_start) DO UPDATE SET
			     period_end = EXCLUDED.period_end,
			     locked = EXCLUDED.locked,
			     locked_at = EXCLUDED.locked_at,
			     locked_by = EXCLUDED.locked_by`,
			p.TenantID, p.PeriodStart, p.PeriodEnd, p.Locked, p.LockedAt, p.LockedBy,
		)
		if err != nil {
			return fmt.Errorf("ledger: upsert fiscal period: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// LockPeriod marks the fiscal period whose period_start matches the
// supplied date as locked. The caller is attributed in locked_by and
// the lock time is stamped via the store's clock. Emits
// `finance.period.locked` and logs an audit entry — atomic with the
// UPDATE so downstream consumers never see a lock without the event.
func (s *PGStore) LockPeriod(ctx context.Context, tenantID uuid.UUID, periodStart time.Time, actorID uuid.UUID) (*FiscalPeriod, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("ledger: actor id required")
	}
	lockedAt := s.now()
	var out FiscalPeriod
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE fiscal_periods
			    SET locked = TRUE, locked_at = $3, locked_by = $4
			  WHERE tenant_id = $1 AND period_start = $2
			  RETURNING tenant_id, period_start, period_end, locked, locked_at, locked_by`,
			tenantID, periodStart, lockedAt, actorID,
		).Scan(
			&out.TenantID, &out.PeriodStart, &out.PeriodEnd,
			&out.Locked, &out.LockedAt, &out.LockedBy,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPeriodNotFound
			}
			return fmt.Errorf("ledger: lock period: %w", err)
		}

		if s.publisher != nil {
			if err := s.publisher.EmitTx(ctx, tx, events.Event{
				TenantID: tenantID,
				Type:     "finance.period.locked",
				Payload:  jsonMustMarshal(map[string]any{"period_start": periodStart, "period_end": out.PeriodEnd, "actor": actorID}),
			}); err != nil {
				return err
			}
		}
		if s.auditor != nil {
			actor := actorID
			if err := s.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:  tenantID,
				ActorID:   &actor,
				ActorKind: audit.ActorUser,
				Action:    "finance.period.locked",
				After:     jsonMustMarshal(map[string]any{"period_start": periodStart, "period_end": out.PeriodEnd}),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// IsPeriodLocked reports whether the supplied date falls inside a locked
// fiscal period for the tenant. Used by PostJournalEntry to reject
// postings into closed books.
func (s *PGStore) IsPeriodLocked(ctx context.Context, tenantID uuid.UUID, date time.Time) (bool, error) {
	if tenantID == uuid.Nil {
		return false, errors.New("ledger: tenant id required")
	}
	var locked bool
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		locked, err = isPeriodLockedTx(ctx, tx, tenantID, date)
		return err
	})
	if err != nil {
		return false, err
	}
	return locked, nil
}

// ListPeriods returns every fiscal period for the tenant ordered by
// period_start ASC. Small N (one row per month); no pagination needed.
func (s *PGStore) ListPeriods(ctx context.Context, tenantID uuid.UUID) ([]FiscalPeriod, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("ledger: tenant id required")
	}
	out := make([]FiscalPeriod, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, period_start, period_end, locked, locked_at, locked_by
			 FROM fiscal_periods WHERE tenant_id = $1
			 ORDER BY period_start`,
			tenantID,
		)
		if err != nil {
			return fmt.Errorf("ledger: list periods: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p FiscalPeriod
			if err := rows.Scan(
				&p.TenantID, &p.PeriodStart, &p.PeriodEnd,
				&p.Locked, &p.LockedAt, &p.LockedBy,
			); err != nil {
				return fmt.Errorf("ledger: scan period: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isPeriodLockedTx is the in-tx helper PostJournalEntry calls so the
// check and the subsequent INSERT share the same transaction — no
// TOCTOU window between "period is unlocked" and "entry is posted".
func isPeriodLockedTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, date time.Time) (bool, error) {
	var locked bool
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(bool_or(locked), FALSE)
		 FROM fiscal_periods
		 WHERE tenant_id = $1
		   AND period_start <= $2::date
		   AND period_end   >= $2::date`,
		tenantID, date,
	).Scan(&locked)
	if err != nil {
		return false, fmt.Errorf("ledger: check period lock: %w", err)
	}
	return locked, nil
}
