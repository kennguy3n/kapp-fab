package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ReportScheduleFormat enumerates the rendering options the worker
// supports for a scheduled report run. Both round-trip through the
// migration's CHECK constraint.
const (
	ReportScheduleFormatCSV = "csv"
	ReportScheduleFormatPDF = "pdf"
)

// ActionTypeReportSchedule is the scheduled_actions.action_type the
// tenant wizard seeds. The handler iterates report_schedules per
// tenant and runs each schedule whose cron expression is due.
const ActionTypeReportSchedule = "report_schedule"

// DefaultReportScheduleIntervalSeconds is the cadence the wizard
// seeds the dispatcher with — five minutes. Per-row eligibility is
// gated on the schedule's cron expression vs. last_run_at, so
// running more often than once per minute is wasted SQL but not
// duplicate runs.
const DefaultReportScheduleIntervalSeconds = 300

// ReportSchedule mirrors a row of the report_schedules table.
// Recipients is a JSON array of email addresses; the worker fans
// out one email per recipient (no Bcc — admins can grep the
// audit log per-recipient if a delivery fails).
type ReportSchedule struct {
	TenantID       uuid.UUID  `json:"tenant_id"`
	ID             uuid.UUID  `json:"id"`
	ReportID       uuid.UUID  `json:"report_id"`
	Name           string     `json:"name"`
	CronExpression string     `json:"cron_expression"`
	Format         string     `json:"format"`
	Recipients     []string   `json:"recipients"`
	Enabled        bool       `json:"enabled"`
	LastRunAt      *time.Time `json:"last_run_at,omitempty"`
	LastStatus     string     `json:"last_status,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// scheduleCronParser uses the standard 5-field cron expression
// (matching internal/scheduler) so a schedule string round-trips
// between the report scheduler and the platform scheduler.
var scheduleCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Validate rejects schedules that would otherwise fail at run time.
// Cheaper to surface the error at PUT time than wait for the
// worker to choke on a malformed cron expression.
func (s *ReportSchedule) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("%w: name required", ErrScheduleInvalidInput)
	}
	if s.ReportID == uuid.Nil {
		return fmt.Errorf("%w: report_id required", ErrScheduleInvalidInput)
	}
	if _, err := scheduleCronParser.Parse(s.CronExpression); err != nil {
		return fmt.Errorf("%w: invalid cron expression: %v", ErrScheduleInvalidInput, err)
	}
	switch s.Format {
	case ReportScheduleFormatCSV, ReportScheduleFormatPDF:
	default:
		return fmt.Errorf("%w: unsupported format %q (want csv or pdf)", ErrScheduleInvalidInput, s.Format)
	}
	if len(s.Recipients) == 0 {
		return fmt.Errorf("%w: at least one recipient is required", ErrScheduleInvalidInput)
	}
	for _, r := range s.Recipients {
		if r == "" {
			return fmt.Errorf("%w: empty recipient address", ErrScheduleInvalidInput)
		}
	}
	return nil
}

// ScheduleStore persists ReportSchedule rows.
type ScheduleStore struct {
	pool *pgxpool.Pool
}

// NewScheduleStore wires the store to the shared pool.
func NewScheduleStore(pool *pgxpool.Pool) *ScheduleStore {
	return &ScheduleStore{pool: pool}
}

// Sentinel errors for the API layer.
var (
	// ErrScheduleNotFound is returned when a schedule id does not
	// resolve under the active tenant.
	ErrScheduleNotFound = errors.New("reporting: schedule not found")
	// ErrScheduleInvalidInput wraps every validation / argument
	// failure so the API layer can translate to 400 Bad Request
	// rather than leaking a generic 500.
	ErrScheduleInvalidInput = errors.New("reporting: invalid schedule input")
)

// Create inserts a new report schedule row.
func (s *ScheduleStore) Create(ctx context.Context, in ReportSchedule) (*ReportSchedule, error) {
	if in.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrScheduleInvalidInput)
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	recipientsJSON, err := json.Marshal(in.Recipients)
	if err != nil {
		return nil, fmt.Errorf("reporting: marshal recipients: %w", err)
	}
	out := in
	err = dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if in.CreatedBy != nil {
			createdBy = *in.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO report_schedules
			     (tenant_id, id, report_id, name, cron_expression, format, recipients, enabled, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 RETURNING created_at, updated_at`,
			in.TenantID, in.ID, in.ReportID, in.Name, in.CronExpression, in.Format,
			recipientsJSON, in.Enabled, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("reporting: create schedule: %w", err)
	}
	return &out, nil
}

// Update replaces a schedule's editable columns.
func (s *ScheduleStore) Update(ctx context.Context, in ReportSchedule) (*ReportSchedule, error) {
	if in.TenantID == uuid.Nil || in.ID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and schedule id required", ErrScheduleInvalidInput)
	}
	if err := in.Validate(); err != nil {
		return nil, err
	}
	recipientsJSON, err := json.Marshal(in.Recipients)
	if err != nil {
		return nil, err
	}
	out := in
	err = dbutil.WithTenantTx(ctx, s.pool, in.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE report_schedules
			    SET report_id = $3, name = $4, cron_expression = $5, format = $6,
			        recipients = $7, enabled = $8, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			in.TenantID, in.ID, in.ReportID, in.Name, in.CronExpression, in.Format,
			recipientsJSON, in.Enabled,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrScheduleNotFound
		}
		return tx.QueryRow(ctx,
			`SELECT created_at, updated_at
			   FROM report_schedules WHERE tenant_id = $1 AND id = $2`,
			in.TenantID, in.ID,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get loads a single schedule.
func (s *ScheduleStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*ReportSchedule, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id and schedule id required", ErrScheduleInvalidInput)
	}
	var out ReportSchedule
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.scanRow(ctx, tx, &out,
			`SELECT tenant_id, id, report_id, name, cron_expression, format,
			        recipients, enabled, last_run_at, last_status, last_error,
			        created_by, created_at, updated_at
			   FROM report_schedules WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// List returns every schedule for a tenant.
func (s *ScheduleStore) List(ctx context.Context, tenantID uuid.UUID) ([]ReportSchedule, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant id required", ErrScheduleInvalidInput)
	}
	out := make([]ReportSchedule, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, report_id, name, cron_expression, format,
			        recipients, enabled, last_run_at, last_status, last_error,
			        created_by, created_at, updated_at
			   FROM report_schedules WHERE tenant_id = $1 ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ReportSchedule
			if err := s.scanInto(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a schedule. Returns ErrScheduleNotFound when the id
// is unknown so the API can emit a 404.
func (s *ScheduleStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return fmt.Errorf("%w: tenant id and schedule id required", ErrScheduleInvalidInput)
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM report_schedules WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrScheduleNotFound
		}
		return nil
	})
}

// ListDue returns the enabled schedules whose next cron fire is at
// or before `now`. Compared against `last_run_at` if set; otherwise
// the schedule fires immediately. The query stays under tenant RLS
// so the worker iterates one tenant at a time.
func (s *ScheduleStore) ListDue(ctx context.Context, tenantID uuid.UUID, now time.Time) ([]ReportSchedule, error) {
	all, err := s.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	due := make([]ReportSchedule, 0, len(all))
	for _, r := range all {
		if !r.Enabled {
			continue
		}
		sched, err := scheduleCronParser.Parse(r.CronExpression)
		if err != nil {
			continue
		}
		anchor := r.LastRunAt
		if anchor == nil {
			due = append(due, r)
			continue
		}
		if !sched.Next(*anchor).After(now) {
			due = append(due, r)
		}
	}
	return due, nil
}

// MarkRun records the outcome of a scheduled run. status is one of
// "success" or "error"; errMsg is non-empty only on failure.
func (s *ScheduleStore) MarkRun(ctx context.Context, tenantID, id uuid.UUID, ranAt time.Time, status, errMsg string) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE report_schedules
			    SET last_run_at = $3, last_status = $4, last_error = $5, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id, ranAt, status, errMsg,
		)
		return err
	})
}

// scanRow runs a single-row Scan against tx using the row scanner
// helper so Get and List share the same field ordering.
func (s *ScheduleStore) scanRow(ctx context.Context, tx pgx.Tx, out *ReportSchedule, sql string, args ...any) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrScheduleNotFound
	}
	return s.scanInto(rows, out)
}

// scanInto unmarshals the canonical column ordering into a
// ReportSchedule. The caller owns the rows iterator.
func (s *ScheduleStore) scanInto(rows pgx.Rows, out *ReportSchedule) error {
	var (
		recipients []byte
		lastRunAt  *time.Time
		lastStatus *string
		lastError  *string
		createdBy  *uuid.UUID
	)
	if err := rows.Scan(
		&out.TenantID, &out.ID, &out.ReportID, &out.Name, &out.CronExpression,
		&out.Format, &recipients, &out.Enabled, &lastRunAt, &lastStatus, &lastError,
		&createdBy, &out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return err
	}
	out.LastRunAt = lastRunAt
	if lastStatus != nil {
		out.LastStatus = *lastStatus
	}
	if lastError != nil {
		out.LastError = *lastError
	}
	out.CreatedBy = createdBy
	if len(recipients) > 0 {
		if err := json.Unmarshal(recipients, &out.Recipients); err != nil {
			return fmt.Errorf("reporting: decode recipients: %w", err)
		}
	}
	return nil
}
