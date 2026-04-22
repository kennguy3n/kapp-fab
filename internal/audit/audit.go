// Package audit provides the append-only audit logger. Every tenant-scoped
// mutation writes one Entry capturing before/after state and actor context.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ActorKind identifies the source of an audited action.
type ActorKind string

const (
	ActorUser   ActorKind = "user"
	ActorAgent  ActorKind = "agent"
	ActorSystem ActorKind = "system"
)

// Entry mirrors a row in the `audit_log` table.
type Entry struct {
	ID          int64           `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	ActorID     *uuid.UUID      `json:"actor_id,omitempty"`
	ActorKind   ActorKind       `json:"actor_kind"`
	Action      string          `json:"action"`
	TargetKType string          `json:"target_ktype,omitempty"`
	TargetID    *uuid.UUID      `json:"target_id,omitempty"`
	Before      json.RawMessage `json:"before,omitempty"`
	After       json.RawMessage `json:"after,omitempty"`
	Context     json.RawMessage `json:"context,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Logger writes audit entries. Implementations must participate in the same
// transaction as the mutation being audited so the entry is durable iff the
// mutation succeeds.
type Logger interface {
	// LogTx appends an audit entry inside the caller's transaction. The
	// transaction must already have SET LOCAL app.tenant_id configured.
	LogTx(ctx context.Context, tx pgx.Tx, entry Entry) error
}
