package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/events"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// eventsHandlers serves the Server-Sent-Events stream of tenant-scoped
// events. The worker owns the durable `delivered_at` semantics for the
// outbox, so this endpoint is strictly a read-only tail: it polls the
// events table for rows whose created_at is after the caller's cursor
// and streams them as SSE frames. No row is mutated here, so concurrent
// SSE consumers and the worker's drain do not conflict.
type eventsHandlers struct {
	pool *pgxpool.Pool
}

// pollInterval controls how often the SSE handler re-queries for new
// events. Two seconds matches the worker's drain cadence so a subscriber
// sees events at roughly the same rate the bus does.
const sseEventsPollInterval = 2 * time.Second

// stream holds the connection open and pushes event frames as they
// appear. Each frame's `id:` field is the event's created_at timestamp
// formatted as RFC3339Nano so that a reconnecting browser's
// `Last-Event-ID` header is a cursor `resolveSSECursor` can parse
// directly. The event UUID is still available inside the `data:`
// payload for client-side deduplication.
func (h *eventsHandlers) stream(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	cursor := resolveSSECursor(r)
	typeFilter := r.URL.Query().Get("type")

	// Send an initial comment so the client's EventSource onopen fires
	// immediately instead of waiting for the first event.
	_, _ = fmt.Fprintf(w, ": connected tenant=%s\n\n", t.ID)
	flusher.Flush()

	ticker := time.NewTicker(sseEventsPollInterval)
	defer ticker.Stop()

	for {
		batch, err := pollEvents(r.Context(), h.pool, t.ID, cursor, typeFilter, 100)
		if err != nil {
			_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
			flusher.Flush()
			return
		}
		for _, e := range batch {
			buf, err := json.Marshal(e)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", e.CreatedAt.UTC().Format(time.RFC3339Nano), e.Type, buf)
			flusher.Flush()
			if e.CreatedAt.After(cursor) {
				cursor = e.CreatedAt
			}
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// loop and poll again
		}
	}
}

// resolveSSECursor resolves the starting cursor for the SSE stream.
// Priority: explicit ?since query param, then Last-Event-ID header
// (interpreted as an RFC3339 timestamp for simplicity), then "now".
func resolveSSECursor(r *http.Request) time.Time {
	if s := r.URL.Query().Get("since"); s != "" {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts
		}
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.UnixMilli(ms).UTC()
		}
	}
	if s := r.Header.Get("Last-Event-ID"); s != "" {
		if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return ts
		}
	}
	return time.Now().UTC().Add(-time.Second)
}

// pollEvents returns all events newer than the cursor for the tenant,
// bounded by `limit`. The query runs inside a tenant-scoped transaction
// so RLS enforces isolation.
func pollEvents(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	cursor time.Time,
	typeFilter string,
	limit int,
) ([]events.Event, error) {
	var out []events.Event
	err := dbutil.WithTenantTx(ctx, pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		args := []any{tenantID, cursor}
		q := `SELECT id, tenant_id, type, payload, created_at
		        FROM events
		       WHERE tenant_id = $1 AND created_at > $2`
		if typeFilter != "" {
			args = append(args, typeFilter)
			q += fmt.Sprintf(" AND type = $%d", len(args))
		}
		args = append(args, limit)
		q += fmt.Sprintf(" ORDER BY created_at LIMIT $%d", len(args))
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("events: query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e events.Event
			if err := rows.Scan(&e.ID, &e.TenantID, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
