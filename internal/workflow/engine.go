package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
)

// Engine orchestrates workflow state transitions for Kapp. Every mutating
// operation (RegisterWorkflow, StartRun, Transition) runs inside a single
// tenant-scoped transaction via dbutil.WithTenantTx so the engine's writes
// land atomically with the corresponding event-outbox row and audit entry.
// This is the same pattern the KRecord store uses — ARCHITECTURE.md §8
// rule 7 (outbox atomicity).
type Engine struct {
	pool    *pgxpool.Pool
	events  *events.PGPublisher
	auditor *audit.PGLogger
	now     func() time.Time
}

// NewEngine wires the engine dependencies. Callers that don't have a real
// events publisher/auditor yet (e.g. unit tests) can pass nil and the
// engine will simply skip emissions; every production caller should pass
// both so the outbox and audit log stay authoritative.
func NewEngine(pool *pgxpool.Pool, pub *events.PGPublisher, aud *audit.PGLogger) *Engine {
	return &Engine{pool: pool, events: pub, auditor: aud, now: time.Now}
}

// Sentinel errors the API layer surfaces as 4xx.
var (
	ErrWorkflowNotFound     = errors.New("workflow: definition not found")
	ErrRunNotFound          = errors.New("workflow: run not found")
	ErrInvalidTransition    = errors.New("workflow: invalid transition")
	ErrDuplicateRun         = errors.New("workflow: a run already exists for this record")
	ErrInvalidDefinition    = errors.New("workflow: invalid definition")
	ErrTransitionFromWrong  = errors.New("workflow: action not legal from current state")
	ErrActorNotFound        = errors.New("workflow: actor id required")
)

// RegisterWorkflow upserts a workflow definition for the tenant at the
// requested version. A tenant may publish multiple versions of the same
// workflow name — live runs pinned to v1 keep resolving v1 even after v2
// is published (cf. KType.Version in internal/ktype). The registration
// itself does not start runs; callers use StartRun once a KRecord exists.
func (e *Engine) RegisterWorkflow(ctx context.Context, def WorkflowDef) error {
	if def.TenantID == uuid.Nil {
		return errors.New("workflow: tenant id required")
	}
	if def.Name == "" {
		return errors.New("workflow: name required")
	}
	if def.Version <= 0 {
		def.Version = 1
	}
	if err := validateDefinition(def.Definition); err != nil {
		return err
	}
	payload, err := json.Marshal(def.Definition)
	if err != nil {
		return fmt.Errorf("workflow: marshal definition: %w", err)
	}
	return dbutil.WithTenantTx(ctx, e.pool, def.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO workflows (tenant_id, name, version, definition)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (tenant_id, name, version) DO UPDATE SET definition = EXCLUDED.definition`,
			def.TenantID, def.Name, def.Version, payload,
		)
		if err != nil {
			return fmt.Errorf("workflow: upsert: %w", err)
		}
		return nil
	})
}

// GetDefinition returns the highest-version definition for a workflow name
// in the tenant. Callers who need a pinned version should query
// workflows directly.
func (e *Engine) GetDefinition(ctx context.Context, tenantID uuid.UUID, name string) (*WorkflowDef, error) {
	if tenantID == uuid.Nil || name == "" {
		return nil, errors.New("workflow: tenant id and name required")
	}
	var out WorkflowDef
	var defJSON []byte
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT name, version, definition
			 FROM workflows
			 WHERE tenant_id = $1 AND name = $2
			 ORDER BY version DESC
			 LIMIT 1`,
			tenantID, name,
		).Scan(&out.Name, &out.Version, &defJSON)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWorkflowNotFound
		}
		return nil, fmt.Errorf("workflow: load definition: %w", err)
	}
	out.TenantID = tenantID
	if err := json.Unmarshal(defJSON, &out.Definition); err != nil {
		return nil, fmt.Errorf("workflow: decode definition: %w", err)
	}
	return &out, nil
}

// StartRun opens a new workflow run for a KRecord. If initialState is
// empty the definition's InitialState is used. Emits `workflow.started`
// on the outbox.
func (e *Engine) StartRun(
	ctx context.Context,
	tenantID uuid.UUID,
	workflowName string,
	recordID uuid.UUID,
	initialState string,
	actorID uuid.UUID,
) (*WorkflowRun, error) {
	if tenantID == uuid.Nil || workflowName == "" || recordID == uuid.Nil {
		return nil, errors.New("workflow: tenant id, workflow name, and record id required")
	}
	def, err := e.GetDefinition(ctx, tenantID, workflowName)
	if err != nil {
		return nil, err
	}
	state := initialState
	if state == "" {
		state = def.Definition.InitialState
	}
	if !containsState(def.Definition.States, state) {
		return nil, fmt.Errorf("%w: %q is not in states %v", ErrInvalidDefinition, state, def.Definition.States)
	}

	run := &WorkflowRun{
		ID:        uuid.New(),
		TenantID:  tenantID,
		Workflow:  workflowName,
		RecordID:  recordID,
		State:     state,
		History:   []HistoryEntry{},
		CreatedAt: e.now().UTC(),
		UpdatedAt: e.now().UTC(),
	}
	err = dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Enforce "one open run per (tenant, record)" at the application layer
		// because the schema does not carry a terminal-state column yet. A
		// later migration can promote this to a UNIQUE index.
		var existing uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT id FROM workflow_runs
			 WHERE tenant_id = $1 AND record_id = $2 LIMIT 1`,
			tenantID, recordID,
		).Scan(&existing)
		if err == nil {
			return ErrDuplicateRun
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("workflow: existing run lookup: %w", err)
		}

		historyJSON, _ := json.Marshal(run.History)
		if _, err := tx.Exec(ctx,
			`INSERT INTO workflow_runs
			     (id, tenant_id, workflow, record_id, state, history, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			run.ID, run.TenantID, run.Workflow, run.RecordID, run.State,
			historyJSON, run.CreatedAt, run.UpdatedAt,
		); err != nil {
			return fmt.Errorf("workflow: insert run: %w", err)
		}

		if e.events != nil {
			payload, _ := json.Marshal(map[string]any{
				"run_id":    run.ID,
				"workflow":  run.Workflow,
				"record_id": run.RecordID,
				"state":     run.State,
				"actor_id":  actorID,
			})
			if err := e.events.EmitTx(ctx, tx, events.Event{
				TenantID: tenantID, Type: "workflow.started", Payload: payload,
			}); err != nil {
				return err
			}
		}
		if e.auditor != nil {
			after, _ := json.Marshal(map[string]any{
				"state":    run.State,
				"workflow": run.Workflow,
			})
			if err := e.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:    tenantID,
				ActorID:     &actorID,
				ActorKind:   audit.ActorUser,
				Action:      "workflow.started",
				TargetKType: run.Workflow,
				TargetID:    &run.RecordID,
				After:       after,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// Transition advances a run by applying an action. The current state and
// action must match one of the registered transitions; otherwise
// ErrInvalidTransition is returned. A successful call atomically:
//   - appends to the run's history JSONB
//   - updates the state column
//   - emits `workflow.transitioned` on the outbox
//   - writes an audit entry
func (e *Engine) Transition(
	ctx context.Context,
	tenantID uuid.UUID,
	runID uuid.UUID,
	action string,
	actorID uuid.UUID,
) (*WorkflowRun, error) {
	if tenantID == uuid.Nil || runID == uuid.Nil {
		return nil, errors.New("workflow: tenant id and run id required")
	}
	if actorID == uuid.Nil {
		return nil, ErrActorNotFound
	}
	var out *WorkflowRun
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, err := loadRunTx(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		def, err := loadDefinitionTx(ctx, tx, tenantID, run.Workflow)
		if err != nil {
			return err
		}
		target, post, err := resolveTransition(def, run.State, action)
		if err != nil {
			return err
		}
		entry := HistoryEntry{
			FromState: run.State,
			ToState:   target,
			Action:    action,
			ActorID:   actorID,
			Timestamp: e.now().UTC(),
		}
		run.History = append(run.History, entry)
		run.State = target
		run.UpdatedAt = e.now().UTC()
		historyJSON, _ := json.Marshal(run.History)
		if _, err := tx.Exec(ctx,
			`UPDATE workflow_runs SET state = $1, history = $2, updated_at = $3
			 WHERE tenant_id = $4 AND id = $5`,
			run.State, historyJSON, run.UpdatedAt, tenantID, run.ID,
		); err != nil {
			return fmt.Errorf("workflow: update run: %w", err)
		}

		if e.events != nil {
			payload, _ := json.Marshal(map[string]any{
				"run_id":     run.ID,
				"workflow":   run.Workflow,
				"record_id":  run.RecordID,
				"from_state": entry.FromState,
				"to_state":   entry.ToState,
				"action":     action,
				"actor_id":   actorID,
				"post":       post,
			})
			if err := e.events.EmitTx(ctx, tx, events.Event{
				TenantID: tenantID, Type: "workflow.transitioned", Payload: payload,
			}); err != nil {
				return err
			}
		}
		if e.auditor != nil {
			before, _ := json.Marshal(map[string]any{"state": entry.FromState})
			after, _ := json.Marshal(map[string]any{
				"state":  entry.ToState,
				"action": action,
				"post":   post,
			})
			if err := e.auditor.LogTx(ctx, tx, audit.Entry{
				TenantID:    tenantID,
				ActorID:     &actorID,
				ActorKind:   audit.ActorUser,
				Action:      "workflow.transitioned",
				TargetKType: run.Workflow,
				TargetID:    &run.RecordID,
				Before:      before,
				After:       after,
			}); err != nil {
				return err
			}
		}
		out = run
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRun returns a run by its primary key.
func (e *Engine) GetRun(ctx context.Context, tenantID, runID uuid.UUID) (*WorkflowRun, error) {
	var out *WorkflowRun
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, err := loadRunTx(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		out = run
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRunByRecord returns the open run for a record, or ErrRunNotFound.
func (e *Engine) GetRunByRecord(ctx context.Context, tenantID, recordID uuid.UUID) (*WorkflowRun, error) {
	var out *WorkflowRun
	err := dbutil.WithTenantTx(ctx, e.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			run         WorkflowRun
			historyJSON []byte
		)
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id, workflow, record_id, state, history, created_at, updated_at
			 FROM workflow_runs
			 WHERE tenant_id = $1 AND record_id = $2
			 ORDER BY updated_at DESC LIMIT 1`,
			tenantID, recordID,
		).Scan(
			&run.ID, &run.TenantID, &run.Workflow, &run.RecordID,
			&run.State, &historyJSON, &run.CreatedAt, &run.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrRunNotFound
			}
			return fmt.Errorf("workflow: load run by record: %w", err)
		}
		if err := json.Unmarshal(historyJSON, &run.History); err != nil {
			return fmt.Errorf("workflow: decode history: %w", err)
		}
		out = &run
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- helpers -------------------------------------------------------------

func validateDefinition(def Definition) error {
	if def.InitialState == "" {
		return fmt.Errorf("%w: initial_state required", ErrInvalidDefinition)
	}
	if len(def.States) == 0 {
		return fmt.Errorf("%w: states required", ErrInvalidDefinition)
	}
	seen := make(map[string]struct{}, len(def.States))
	for _, s := range def.States {
		if _, dup := seen[s]; dup {
			return fmt.Errorf("%w: duplicate state %q", ErrInvalidDefinition, s)
		}
		seen[s] = struct{}{}
	}
	if _, ok := seen[def.InitialState]; !ok {
		return fmt.Errorf("%w: initial_state %q not in states", ErrInvalidDefinition, def.InitialState)
	}
	for _, tr := range def.Transitions {
		if tr.Action == "" || tr.To == "" || len(tr.From) == 0 {
			return fmt.Errorf("%w: transition requires from[], to, action", ErrInvalidDefinition)
		}
		if _, ok := seen[tr.To]; !ok {
			return fmt.Errorf("%w: to state %q not in states", ErrInvalidDefinition, tr.To)
		}
		for _, from := range tr.From {
			if _, ok := seen[from]; !ok {
				return fmt.Errorf("%w: from state %q not in states", ErrInvalidDefinition, from)
			}
		}
	}
	return nil
}

func containsState(states []string, s string) bool {
	for _, x := range states {
		if x == s {
			return true
		}
	}
	return false
}

func loadRunTx(ctx context.Context, tx pgx.Tx, tenantID, runID uuid.UUID) (*WorkflowRun, error) {
	var (
		run         WorkflowRun
		historyJSON []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, workflow, record_id, state, history, created_at, updated_at
		 FROM workflow_runs
		 WHERE tenant_id = $1 AND id = $2`,
		tenantID, runID,
	).Scan(
		&run.ID, &run.TenantID, &run.Workflow, &run.RecordID,
		&run.State, &historyJSON, &run.CreatedAt, &run.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("workflow: load run: %w", err)
	}
	if err := json.Unmarshal(historyJSON, &run.History); err != nil {
		return nil, fmt.Errorf("workflow: decode history: %w", err)
	}
	return &run, nil
}

func loadDefinitionTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name string) (*WorkflowDef, error) {
	var (
		def     WorkflowDef
		defJSON []byte
	)
	err := tx.QueryRow(ctx,
		`SELECT name, version, definition
		 FROM workflows
		 WHERE tenant_id = $1 AND name = $2
		 ORDER BY version DESC LIMIT 1`,
		tenantID, name,
	).Scan(&def.Name, &def.Version, &defJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWorkflowNotFound
		}
		return nil, fmt.Errorf("workflow: load definition: %w", err)
	}
	def.TenantID = tenantID
	if err := json.Unmarshal(defJSON, &def.Definition); err != nil {
		return nil, fmt.Errorf("workflow: decode definition: %w", err)
	}
	return &def, nil
}

// resolveTransition finds the unique transition matching (state, action).
// Returns the target state and any post-transition hooks.
func resolveTransition(def *WorkflowDef, fromState, action string) (string, []string, error) {
	for _, tr := range def.Definition.Transitions {
		if tr.Action != action {
			continue
		}
		for _, from := range tr.From {
			if from == fromState {
				return tr.To, tr.Post, nil
			}
		}
	}
	return "", nil, fmt.Errorf("%w: action %q not legal from state %q",
		ErrTransitionFromWrong, action, fromState)
}
