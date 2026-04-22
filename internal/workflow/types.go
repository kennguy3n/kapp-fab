package workflow

import (
	"time"

	"github.com/google/uuid"
)

// Definition is the parsed JSONB payload stored in the `workflows.definition`
// column. It describes the legal states a record can inhabit and the
// transitions that move between them. This mirrors Frappe's Workflow /
// Workflow Transition child-table pattern but keeps the state machine out
// of the KRecord itself: run state lives in workflow_runs so the engine
// can evolve independently of the KType schema (ARCHITECTURE.md §6).
type Definition struct {
	// InitialState is the state assigned when StartRun is called without
	// an explicit override. Must appear in States.
	InitialState string `json:"initial_state"`
	// States is the canonical list of reachable states. Duplicates are
	// rejected at registration.
	States []string `json:"states"`
	// Transitions lists every legal (from→to, action) triple. Multiple
	// transitions may share an action name if their From sets are disjoint
	// (e.g. `mark_lost` from qualification / proposal / negotiation).
	Transitions []Transition `json:"transitions"`
}

// Transition is a single legal state-machine edge. Guards (role, field
// predicate) are deferred to Phase C; Post hooks are names of downstream
// agent tools or system jobs dispatched after a successful transition
// (e.g. `finance.create_sales_invoice` on deal.mark_won).
type Transition struct {
	From   []string `json:"from"`
	To     string   `json:"to"`
	Action string   `json:"action"`
	Post   []string `json:"post,omitempty"`
}

// WorkflowDef mirrors a `workflows` table row. A workflow is (tenant, name,
// version); the registry keeps every version so long-running runs pinned to
// an old version continue to resolve.
type WorkflowDef struct {
	TenantID   uuid.UUID  `json:"tenant_id"`
	Name       string     `json:"name"`
	Version    int        `json:"version"`
	Definition Definition `json:"definition"`
}

// WorkflowRun mirrors a `workflow_runs` row. One run is open per
// (tenant, record_id) pair; callers must either advance the existing run
// or finalize it before opening a new one.
type WorkflowRun struct {
	ID        uuid.UUID      `json:"id"`
	TenantID  uuid.UUID      `json:"tenant_id"`
	Workflow  string         `json:"workflow"`
	RecordID  uuid.UUID      `json:"record_id"`
	State     string         `json:"state"`
	History   []HistoryEntry `json:"history"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// HistoryEntry captures one transition. The engine appends an entry on
// every successful Transition call. Payload is kept small on purpose so
// history can be scanned cheaply from the UI without fetching the audit
// log separately.
type HistoryEntry struct {
	FromState string    `json:"from_state"`
	ToState   string    `json:"to_state"`
	Action    string    `json:"action"`
	ActorID   uuid.UUID `json:"actor_id"`
	Timestamp time.Time `json:"timestamp"`
}

// Approval mirrors an `approvals` table row. The chain JSONB carries the
// step-by-step configuration plus the running list of decisions so the
// engine can determine whether the approval is complete without an extra
// lookup.
type Approval struct {
	ID          uuid.UUID     `json:"id"`
	TenantID    uuid.UUID     `json:"tenant_id"`
	RecordKType string        `json:"record_ktype"`
	RecordID    uuid.UUID     `json:"record_id"`
	Chain       ApprovalChain `json:"chain"`
	State       string        `json:"state"`
	CreatedAt   time.Time     `json:"created_at"`
}

// Approval state values used across the engine, event bus, and KChat
// approval cards. `pending`  → at least one step still open.  `approved`
// / `rejected` are terminal.
const (
	ApprovalStatePending  = "pending"
	ApprovalStateApproved = "approved"
	ApprovalStateRejected = "rejected"
)

// Approval decisions a single approver can submit. Aligned with the
// KChat card buttons so the two APIs are interchangeable.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// ApprovalChain is the serialized chain stored in approvals.chain. The
// RequestedBy fan-out and per-step quorum are captured here so a later
// read — e.g. rendering a KChat card — never needs to hit a second
// table. Steps run sequentially: all approvers on step N must respond
// before step N+1 opens.
type ApprovalChain struct {
	RequestedBy uuid.UUID        `json:"requested_by"`
	CurrentStep int              `json:"current_step"`
	Steps       []ApprovalStep   `json:"steps"`
	History     []ApprovalAction `json:"history"`
}

// ApprovalStep is a single rung in the chain. RequiredCount supports
// quorum — e.g. 2-of-3 finance directors — while the default of 0 is
// treated as "all approvers must decide" to keep declarative chains
// unambiguous.
type ApprovalStep struct {
	Approvers     []uuid.UUID   `json:"approvers"`
	RequiredCount int           `json:"required_count,omitempty"`
	Timeout       time.Duration `json:"timeout,omitempty"`
}

// ApprovalAction records one approver's decision. Appended to
// Chain.History so the full approval timeline is self-contained on the
// approvals row.
type ApprovalAction struct {
	StepIndex int       `json:"step_index"`
	ActorID   uuid.UUID `json:"actor_id"`
	Decision  string    `json:"decision"`
	Timestamp time.Time `json:"timestamp"`
}
