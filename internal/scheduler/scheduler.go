// Package scheduler executes per-tenant background jobs stored in
// scheduled_actions. Handlers register against an action_type string
// and are dispatched by a single worker loop that polls the DB for
// due rows under FOR UPDATE SKIP LOCKED so multiple worker replicas
// can cooperate safely.
//
// The package owns only the transport: cadence, persistence, error
// handling, and next-run advancement. The business logic for each
// action_type lives in the consuming package (e.g. the SLA breach
// sweeper in services/worker, the recurring-invoice generator in
// internal/finance).
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// ScheduledAction mirrors one row of scheduled_actions. CronExpr and
// IntervalSeconds are mutually-exclusive in practice — the table CHECK
// constraint enforces at least one is set, and AdvanceNextRun prefers
// cron_expr when both are present.
type ScheduledAction struct {
	TenantID        uuid.UUID       `json:"tenant_id"`
	ID              uuid.UUID       `json:"id"`
	ActionType      string          `json:"action_type"`
	CronExpr        string          `json:"cron_expr,omitempty"`
	IntervalSeconds int             `json:"interval_seconds,omitempty"`
	NextRunAt       time.Time       `json:"next_run_at"`
	LastRunAt       *time.Time      `json:"last_run_at,omitempty"`
	Payload         json.RawMessage `json:"payload"`
	Enabled         bool            `json:"enabled"`
	CreatedBy       *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// ActionHandler is the dispatch target for a single action_type.
// Handlers run after the scheduler has locked the row and before the
// row's next_run_at is advanced; returning an error skips the advance
// so the action retries on the next poll tick.
type ActionHandler interface {
	Handle(ctx context.Context, tenantID uuid.UUID, action ScheduledAction) error
}

// HandlerFunc adapts an ordinary function to the ActionHandler
// interface — matches the http.HandlerFunc pattern.
type HandlerFunc func(ctx context.Context, tenantID uuid.UUID, action ScheduledAction) error

// Handle delegates to the wrapped function.
func (f HandlerFunc) Handle(ctx context.Context, tenantID uuid.UUID, a ScheduledAction) error {
	return f(ctx, tenantID, a)
}

// Registry maps action_type → ActionHandler. Handlers are registered
// once at process start before the scheduler loop begins polling;
// Register is safe to call concurrently with Lookup so callers can
// register handlers on demand (e.g. from tests) without racy init.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]ActionHandler
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]ActionHandler{}}
}

// Register binds a handler to an action_type. Re-registering the same
// name replaces the prior handler — useful for tests that swap a
// handler in and out.
func (r *Registry) Register(actionType string, h ActionHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[actionType] = h
}

// Lookup returns the handler for the action_type, or (nil, false) if
// none is registered.
func (r *Registry) Lookup(actionType string) (ActionHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[actionType]
	return h, ok
}

// cronParser enforces 5-field standard cron syntax (minute hour dom
// month dow). Callers can store "@every 5s"-style expressions in
// interval_seconds instead to keep cron_expr reserved for true
// calendar-driven schedules.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// Store owns the scheduled_actions CRUD and the poll query. The poll
// path uses the admin pool (BYPASSRLS) because it legitimately spans
// tenants; per-tenant writes (AdvanceNextRun, Upsert) run under
// dbutil.WithTenantTx so RLS stays enforced for mutations.
type Store struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
	now       func() time.Time
}

// NewStore builds a Store. adminPool may be nil — PollDue then
// short-circuits with a warning, matching the stock-alert sweeper's
// degraded-mode contract.
func NewStore(pool, adminPool *pgxpool.Pool) *Store {
	return &Store{
		pool:      pool,
		adminPool: adminPool,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

// Upsert inserts a new action or replaces one with the same (tenant,
// id). Callers that care about insert-vs-update semantics should
// check the returned row's UpdatedAt vs CreatedAt themselves. Every
// write runs under WithTenantTx so the RLS policy blocks cross-tenant
// writes even if the caller passed a mismatched tenant_id.
func (s *Store) Upsert(ctx context.Context, a ScheduledAction) (*ScheduledAction, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("scheduler: store not wired")
	}
	if a.TenantID == uuid.Nil {
		return nil, errors.New("scheduler: tenant id required")
	}
	if a.ActionType == "" {
		return nil, errors.New("scheduler: action_type required")
	}
	if a.CronExpr == "" && a.IntervalSeconds <= 0 {
		return nil, errors.New("scheduler: cron_expr or interval_seconds required")
	}
	if a.CronExpr != "" {
		if _, err := cronParser.Parse(a.CronExpr); err != nil {
			return nil, fmt.Errorf("scheduler: invalid cron_expr %q: %w", a.CronExpr, err)
		}
	}
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if len(a.Payload) == 0 {
		a.Payload = json.RawMessage(`{}`)
	}
	if a.NextRunAt.IsZero() {
		a.NextRunAt = s.now()
	}
	out := a
	err := dbutil.WithTenantTx(ctx, s.pool, a.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if a.CreatedBy != nil {
			createdBy = *a.CreatedBy
		}
		var cronExpr any
		if a.CronExpr != "" {
			cronExpr = a.CronExpr
		}
		var interval any
		if a.IntervalSeconds > 0 {
			interval = a.IntervalSeconds
		}
		return tx.QueryRow(ctx,
			`INSERT INTO scheduled_actions
			     (tenant_id, id, action_type, cron_expr, interval_seconds,
			      next_run_at, payload, enabled, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 ON CONFLICT (tenant_id, id) DO UPDATE SET
			     action_type = EXCLUDED.action_type,
			     cron_expr = EXCLUDED.cron_expr,
			     interval_seconds = EXCLUDED.interval_seconds,
			     next_run_at = EXCLUDED.next_run_at,
			     payload = EXCLUDED.payload,
			     enabled = EXCLUDED.enabled,
			     updated_at = now()
			 RETURNING created_at, updated_at`,
			a.TenantID, a.ID, a.ActionType, cronExpr, interval,
			a.NextRunAt, a.Payload, a.Enabled, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("scheduler: upsert: %w", err)
	}
	return &out, nil
}

// Disable flips enabled=false so the row stops being polled without
// deleting it — preserves the action's history for debugging. Used by
// the setup wizard when a tenant opts out of the default sweeps.
func (s *Store) Disable(ctx context.Context, tenantID, id uuid.UUID) error {
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE scheduled_actions
			    SET enabled = FALSE, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id)
		return err
	})
}

// PollDue claims up to batchSize due rows with FOR UPDATE SKIP LOCKED
// inside a single transaction, tentatively advances each row's
// next_run_at to the next fire time, commits, then returns the
// claimed slice so the caller can dispatch. Advancing next_run_at
// before releasing the row prevents a sibling worker from re-polling
// the same action while the current handler is running — two workers
// polling concurrently each pick up disjoint subsets because FOR
// UPDATE + SKIP LOCKED handles the concurrent-select case, and
// updating next_run_at handles the "second poll arrives after the
// first commits but before the handler completes" case.
//
// If a handler subsequently errors the row is NOT reset — the
// handler's retry policy belongs in its payload. AdvanceNextRun
// still exists for callers that need to override the tentative next
// (e.g. recurring-invoice generator advancing past a skipped period).
//
// Returns an empty slice when no rows are due; nil error in that case.
func (s *Store) PollDue(ctx context.Context, batchSize int) ([]ScheduledAction, error) {
	if s == nil {
		return nil, errors.New("scheduler: store not wired")
	}
	if s.adminPool == nil {
		// Running without admin pool — poll query would default-deny
		// under RLS. Mirror the stock alert worker's degraded-mode
		// contract: skip silently and let the caller log.
		return nil, nil
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	tx, err := s.adminPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("scheduler: begin poll tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, action_type,
		        COALESCE(cron_expr, ''), COALESCE(interval_seconds, 0),
		        next_run_at, last_run_at, payload, enabled,
		        created_by, created_at, updated_at
		   FROM scheduled_actions
		  WHERE enabled
		    AND next_run_at <= now()
		  ORDER BY next_run_at ASC
		  FOR UPDATE SKIP LOCKED
		  LIMIT $1`,
		batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("scheduler: poll due: %w", err)
	}
	var out []ScheduledAction
	for rows.Next() {
		var a ScheduledAction
		var createdBy *uuid.UUID
		if err := rows.Scan(
			&a.TenantID, &a.ID, &a.ActionType,
			&a.CronExpr, &a.IntervalSeconds,
			&a.NextRunAt, &a.LastRunAt, &a.Payload, &a.Enabled,
			&createdBy, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scheduler: scan due row: %w", err)
		}
		a.CreatedBy = createdBy
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	now := s.now()
	// One corrupt row (e.g. a cron_expr written by direct SQL that
	// bypassed Upsert's validator) must not block every other due
	// action. If NextRun rejects a row we disable it in the same
	// transaction — a hard stop that surfaces in the scheduled_actions
	// table without wedging the loop — and skip the claim. The row is
	// dropped from the returned batch because a handler has no next
	// time to run against. Successfully-advanced rows proceed normally.
	dispatchable := out[:0]
	for i := range out {
		next, nerr := NextRun(out[i], now)
		if nerr != nil {
			log.Printf("scheduler: disabling action %s (tenant=%s action_type=%s): %v",
				out[i].ID, out[i].TenantID, out[i].ActionType, nerr)
			if _, err := tx.Exec(ctx,
				`UPDATE scheduled_actions
				    SET enabled = FALSE, updated_at = now()
				  WHERE tenant_id = $1 AND id = $2`,
				out[i].TenantID, out[i].ID); err != nil {
				return nil, fmt.Errorf("scheduler: disable corrupt row: %w", err)
			}
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE scheduled_actions
			    SET last_run_at = $3,
			        next_run_at = $4,
			        updated_at  = now()
			  WHERE tenant_id = $1 AND id = $2`,
			out[i].TenantID, out[i].ID, now, next); err != nil {
			return nil, fmt.Errorf("scheduler: tentative advance: %w", err)
		}
		dispatchable = append(dispatchable, out[i])
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("scheduler: poll commit: %w", err)
	}
	return dispatchable, nil
}

// AdvanceNextRun sets last_run_at to now and next_run_at to the next
// fire time computed from cron_expr (preferred) or interval_seconds.
// Called after each handler run — when the handler errored, the
// scheduler still advances the row so a persistently-failing action
// does not stall the entire poll loop; the handler's own retry
// strategy (if any) belongs in the payload.
func (s *Store) AdvanceNextRun(ctx context.Context, action ScheduledAction) error {
	now := s.now()
	next, err := NextRun(action, now)
	if err != nil {
		return err
	}
	return dbutil.WithTenantTx(ctx, s.pool, action.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE scheduled_actions
			    SET last_run_at = $3,
			        next_run_at = $4,
			        updated_at  = now()
			  WHERE tenant_id = $1 AND id = $2`,
			action.TenantID, action.ID, now, next)
		return err
	})
}

// NextRun is the pure calculation layer: given an action and a "now"
// reference time, return the next fire time. Split out from
// AdvanceNextRun so tests can exercise the cadence math without a
// database.
func NextRun(action ScheduledAction, from time.Time) (time.Time, error) {
	if action.CronExpr != "" {
		sched, err := cronParser.Parse(action.CronExpr)
		if err != nil {
			return time.Time{}, fmt.Errorf("scheduler: invalid cron_expr %q: %w", action.CronExpr, err)
		}
		return sched.Next(from), nil
	}
	if action.IntervalSeconds > 0 {
		return from.Add(time.Duration(action.IntervalSeconds) * time.Second), nil
	}
	return time.Time{}, errors.New("scheduler: action missing cadence")
}

// RunLoop polls for due actions on the supplied interval and
// dispatches each one to its registered handler. A missing handler
// surfaces a warning and advances the row anyway so the scheduler
// does not wedge on a misconfigured tenant.
//
// The loop logs and continues on individual errors; the only way to
// exit is ctx cancellation. Callers typically `go scheduler.RunLoop(ctx)`
// as a sibling goroutine to the outbox drain.
func RunLoop(ctx context.Context, store *Store, registry *Registry, interval time.Duration) {
	if store == nil || registry == nil {
		log.Printf("scheduler: run loop skipped: store or registry nil")
		return
	}
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Tick once immediately so a freshly-seeded action fires without
	// waiting a full interval.
	runTick(ctx, store, registry)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runTick(ctx, store, registry)
		}
	}
}

// runTick is the body of a single RunLoop iteration, factored out so
// tests can drive one pass deterministically without sitting on a
// time.Ticker. next_run_at is advanced inside PollDue, so the
// dispatch loop below only needs to invoke handlers.
func runTick(ctx context.Context, store *Store, registry *Registry) {
	actions, err := store.PollDue(ctx, 0)
	if err != nil {
		log.Printf("scheduler: poll due: %v", err)
		return
	}
	for _, a := range actions {
		handler, ok := registry.Lookup(a.ActionType)
		if !ok {
			log.Printf("scheduler: no handler registered for action_type=%q (tenant=%s id=%s)",
				a.ActionType, a.TenantID, a.ID)
			continue
		}
		if err := handler.Handle(ctx, a.TenantID, a); err != nil {
			log.Printf("scheduler: handler %s failed (tenant=%s id=%s): %v",
				a.ActionType, a.TenantID, a.ID, err)
		}
	}
}
