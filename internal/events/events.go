// Package events implements the outbox-pattern event publisher. Events are
// written in the same transaction as the originating state change and drained
// by a batched publisher to the external event bus.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Event mirrors a row in the `events` table.
type Event struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	CreatedAt   time.Time       `json:"created_at"`
	DeliveredAt *time.Time      `json:"delivered_at,omitempty"`
}

// Publisher writes to the outbox and drains undelivered batches to the bus.
//
// ARCHITECTURE.md §8 rule 7 mandates that event emits are atomic with the
// originating state change. EmitTx is therefore the primary entry point for
// mutators: it accepts the caller's *pgx.Tx so the INSERT participates in
// that transaction. DrainBatch runs in its own short-lived transaction in the
// worker process.
type Publisher interface {
	// EmitTx appends an event to the outbox inside the caller's transaction.
	// The transaction must already have SET LOCAL app.tenant_id configured
	// via platform.SetTenantContext because the events table is RLS-enabled.
	EmitTx(ctx context.Context, tx pgx.Tx, event Event) error
	// DrainBatch fetches up to `limit` undelivered events, hands them to the
	// supplied delivery function, and marks them delivered on success. It
	// uses FOR UPDATE SKIP LOCKED so multiple worker instances can drain
	// concurrently without stepping on each other.
	DrainBatch(ctx context.Context, limit int, deliver func(ctx context.Context, batch []Event) error) (int, error)
}
