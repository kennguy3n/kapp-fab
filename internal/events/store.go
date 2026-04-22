package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// PGPublisher is the PostgreSQL-backed Publisher. EmitTx participates in the
// caller's transaction so the outbox INSERT is atomic with the originating
// state change (ARCHITECTURE.md §8 rule 7). DrainBatch runs in its own
// short-lived transaction per drained tenant partition, using FOR UPDATE
// SKIP LOCKED so multiple worker replicas can share the drain work.
type PGPublisher struct {
	pool *pgxpool.Pool
}

// NewPGPublisher constructs a PGPublisher bound to the provided pool.
func NewPGPublisher(pool *pgxpool.Pool) *PGPublisher {
	return &PGPublisher{pool: pool}
}

// EmitTx inserts the event into the outbox inside the caller's transaction.
// The ID is assigned automatically if absent, and payload defaults to the
// empty JSON object.
func (p *PGPublisher) EmitTx(ctx context.Context, tx pgx.Tx, event Event) error {
	if event.TenantID == uuid.Nil {
		return fmt.Errorf("events: tenant id required")
	}
	if event.Type == "" {
		return fmt.Errorf("events: type required")
	}
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	payload := event.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage("{}")
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO events (id, tenant_id, type, payload) VALUES ($1, $2, $3, $4)`,
		event.ID, event.TenantID, event.Type, payload,
	)
	if err != nil {
		return fmt.Errorf("events: emit: %w", err)
	}
	return nil
}

// DrainBatch fetches up to `limit` undelivered rows ordered by creation time
// across tenants (partition pruning still applies because the index is
// tenant-first), invokes `deliver`, and marks the rows delivered on success.
// It uses FOR UPDATE SKIP LOCKED so multiple workers can drain concurrently.
func (p *PGPublisher) DrainBatch(
	ctx context.Context,
	limit int,
	deliver func(ctx context.Context, batch []Event) error,
) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	// We first discover which tenants have undelivered events. Draining is
	// then done per-tenant so we can set the RLS tenant GUC correctly.
	rows, err := p.pool.Query(ctx,
		`SELECT DISTINCT tenant_id FROM events WHERE delivered_at IS NULL LIMIT 256`)
	if err != nil {
		return 0, fmt.Errorf("events: discover tenants: %w", err)
	}
	var tenants []uuid.UUID
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return 0, fmt.Errorf("events: scan tenant: %w", err)
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("events: rows err: %w", err)
	}

	total := 0
	for _, tenantID := range tenants {
		remaining := limit - total
		if remaining <= 0 {
			break
		}
		n, err := p.drainTenant(ctx, tenantID, remaining, deliver)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (p *PGPublisher) drainTenant(
	ctx context.Context,
	tenantID uuid.UUID,
	limit int,
	deliver func(ctx context.Context, batch []Event) error,
) (int, error) {
	var drained int
	err := platform.WithTenantTx(ctx, p.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, tenant_id, type, payload, created_at
			 FROM events
			 WHERE delivered_at IS NULL AND tenant_id = $1
			 ORDER BY created_at
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED`,
			tenantID, limit,
		)
		if err != nil {
			return fmt.Errorf("events: select batch: %w", err)
		}
		var batch []Event
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.ID, &e.TenantID, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
				rows.Close()
				return fmt.Errorf("events: scan: %w", err)
			}
			batch = append(batch, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("events: rows err: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}
		if err := deliver(ctx, batch); err != nil {
			return err
		}
		ids := make([]uuid.UUID, 0, len(batch))
		for _, e := range batch {
			ids = append(ids, e.ID)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE events SET delivered_at = now()
			 WHERE tenant_id = $1 AND id = ANY($2::uuid[])`,
			tenantID, ids,
		); err != nil {
			return fmt.Errorf("events: mark delivered: %w", err)
		}
		drained = len(batch)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return drained, nil
}
