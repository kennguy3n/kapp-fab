// Package events implements the outbox-pattern event publisher. Events are
// written in the same transaction as the originating state change and drained
// by a batched publisher to the external event bus.
package events

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
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
type Publisher interface {
	// Emit appends an event to the outbox. Must run inside the caller's
	// transaction so the event is atomic with the state change.
	Emit(ctx context.Context, event Event) error
	// DrainBatch fetches up to `limit` undelivered events, hands them to the
	// supplied delivery function, and marks them delivered on success.
	DrainBatch(ctx context.Context, limit int, deliver func(ctx context.Context, batch []Event) error) (int, error)
}
