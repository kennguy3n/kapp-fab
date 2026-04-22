// Package agents wires the Phase B agent tools (CRM, Tasks, Approvals)
// behind a uniform Invoke surface. Every tool runs in one of two modes —
// dry_run returns the would-be effect without touching state, and commit
// performs the write. Both modes emit an audit entry with actor_kind =
// "agent" so humans can review what the tool did (or proposed to do)
// against any record. See ARCHITECTURE.md §11.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/workflow"
)

// Mode captures whether a tool call mutates state. `modes.commit` is the
// only mode that writes; `modes.dry_run` returns a preview. Tools that
// are read-only (e.g. crm.summarize_pipeline) accept either mode and
// behave identically.
type Mode string

const (
	ModeDryRun Mode = "dry_run"
	ModeCommit Mode = "commit"
)

// Invocation captures a single tool call. TenantID + ActorID are
// required on every invocation because the audit entry and any
// downstream writes must be attributable.
type Invocation struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	ActorID     uuid.UUID       `json:"actor_id"`
	ToolName    string          `json:"tool_name"`
	Inputs      json.RawMessage `json:"inputs"`
	Mode        Mode            `json:"mode"`
	Confirmed   bool            `json:"confirmed,omitempty"`
}

// Result is the uniform envelope returned by every tool. `Preview` is
// populated when Mode == dry_run; `Record` and `Run` are populated by
// the appropriate writes in commit mode. The `Summary` field is a
// short human-readable line suitable for a KChat card body.
type Result struct {
	Tool    string                `json:"tool"`
	Mode    Mode                  `json:"mode"`
	Summary string                `json:"summary"`
	Preview json.RawMessage       `json:"preview,omitempty"`
	Record  *record.KRecord       `json:"record,omitempty"`
	Run     *workflow.WorkflowRun `json:"run,omitempty"`
	Extra   map[string]any        `json:"extra,omitempty"`
}

// Handler is the contract every tool implements. Handlers receive the
// already-validated Invocation and return the Result directly; the
// executor takes care of audit-log emission and confirmation gates.
type Handler interface {
	Name() string
	RequiresConfirmation() bool
	Invoke(ctx context.Context, inv Invocation) (*Result, error)
}

// Executor multiplexes tool calls across the registered handlers. The
// record store / workflow engine / auditor are injected so tests can
// swap them out for in-memory fakes. RequiresConfirmation enforces the
// ARCHITECTURE.md §11 rule that "destructive" tools (approvals.decide,
// approvals.request) must ship with confirmed=true before committing.
type Executor struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	auditor  *audit.PGLogger
	records  *record.PGStore
	workflow *workflow.Engine
}

// Sentinel errors surfaced through the HTTP layer.
var (
	ErrUnknownTool          = errors.New("agents: unknown tool")
	ErrConfirmationRequired = errors.New("agents: confirmation required for destructive tool")
	ErrInvalidMode          = errors.New("agents: mode must be dry_run or commit")
	ErrMissingContext       = errors.New("agents: tenant_id and actor_id are required")
)

// NewExecutor builds a tool-empty executor. Call Register() to add
// tools before serving traffic.
func NewExecutor(records *record.PGStore, wf *workflow.Engine, auditor *audit.PGLogger) *Executor {
	return &Executor{
		handlers: map[string]Handler{},
		records:  records,
		workflow: wf,
		auditor:  auditor,
	}
}

// Register wires a tool handler. Registering twice under the same name
// replaces the previous handler — callers should only rely on this at
// setup time to avoid races with Invoke.
func (x *Executor) Register(h Handler) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.handlers[h.Name()] = h
}

// Tools returns the set of registered tool names; used by the bootstrap
// endpoint (`GET /api/v1/agents/tools`) to advertise the available
// surface area.
func (x *Executor) Tools() []string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	names := make([]string, 0, len(x.handlers))
	for n := range x.handlers {
		names = append(names, n)
	}
	return names
}

// Invoke validates the envelope, dispatches to the handler, and writes
// an audit entry whether the call succeeded or failed. The audit entry
// is written best-effort outside any handler transaction so even a
// tool that rolls back leaves an attributable breadcrumb.
func (x *Executor) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
	if inv.TenantID == uuid.Nil || inv.ActorID == uuid.Nil {
		return nil, ErrMissingContext
	}
	if inv.Mode == "" {
		inv.Mode = ModeDryRun
	}
	if inv.Mode != ModeDryRun && inv.Mode != ModeCommit {
		return nil, ErrInvalidMode
	}

	x.mu.RLock()
	h, ok := x.handlers[inv.ToolName]
	x.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTool, inv.ToolName)
	}

	// Confirmation gate. Applies only to commit-mode calls — dry-runs
	// are purely informational so callers can preview without consent.
	if inv.Mode == ModeCommit && h.RequiresConfirmation() && !inv.Confirmed {
		return nil, ErrConfirmationRequired
	}

	res, err := h.Invoke(ctx, inv)
	x.logAudit(ctx, inv, res, err)
	if err != nil {
		return nil, err
	}
	if res != nil {
		res.Tool = inv.ToolName
		res.Mode = inv.Mode
	}
	return res, nil
}

// logAudit writes one audit_log row per invocation. We emit even on
// error (and even on dry-run) so agent behaviour is fully traceable.
// Writes are best-effort: audit failures do not mask the underlying
// tool error, they are only logged via the returned error chain.
func (x *Executor) logAudit(ctx context.Context, inv Invocation, res *Result, callErr error) {
	if x.auditor == nil {
		return
	}
	after := map[string]any{
		"tool":      inv.ToolName,
		"mode":      inv.Mode,
		"confirmed": inv.Confirmed,
	}
	if callErr != nil {
		after["error"] = callErr.Error()
	} else if res != nil {
		after["summary"] = res.Summary
		if res.Record != nil {
			after["record_id"] = res.Record.ID
		}
		if res.Run != nil {
			after["run_id"] = res.Run.ID
			after["state"] = res.Run.State
		}
	}
	afterJSON, _ := json.Marshal(after)
	_ = x.auditor.Log(ctx, audit.Entry{
		TenantID:  inv.TenantID,
		ActorID:   &inv.ActorID,
		ActorKind: audit.ActorAgent,
		Action:    "agent.tool." + string(inv.Mode),
		After:     afterJSON,
	})
}
