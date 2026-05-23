// Package record models generic KRecord storage. KRecords are tenant-scoped
// JSONB rows keyed by (tenant_id, id), validated against a KType schema.
package record

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// KRecord mirrors a row in the `krecords` table.
type KRecord struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	KType        string          `json:"ktype"`
	KTypeVersion int             `json:"ktype_version"`
	Data         json.RawMessage `json:"data"`
	Status       string          `json:"status"`
	Version      int             `json:"version"`
	CreatedBy    uuid.UUID       `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedBy    *uuid.UUID      `json:"updated_by,omitempty"`
	UpdatedAt    time.Time       `json:"updated_at"`
	DeletedAt    *time.Time      `json:"deleted_at,omitempty"`
}

// ListFilter narrows a List query by KType and optional status.
//
// Cursor (opaque base64 token from a prior page's NextCursor) selects
// keyset pagination — `WHERE (updated_at, id) < (cursor_ts, cursor_id)`
// — which is stable under concurrent inserts. When Cursor is empty
// the store starts from the newest row. Offset is the legacy
// OFFSET-based path, kept for backward compatibility and is only
// consulted when Cursor is empty.
type ListFilter struct {
	KType  string
	Status string
	Limit  int
	Offset int
	Cursor string
}

// ListPage is the envelope returned by ListPage / the HTTP list
// handler. NextCursor is empty when there are no more pages.
type ListPage struct {
	Records    []KRecord `json:"records"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// ErrInvalidCursor is returned when a cursor token cannot be
// decoded — malformed base64, wrong field count, unparseable
// timestamp, or unparseable UUID. Callers should treat this as a
// 400-class error.
var ErrInvalidCursor = errors.New("record: invalid cursor")

// ListAllMaxRows is the defensive safety cap on PGStore.ListAll /
// ListByField in-memory accumulation. Callers that walk a KType with
// more than this many rows must migrate to PGStore.ForEach (Pillar A2)
// instead of materialising the whole result set. The cap is sized to
// be comfortably above realistic SME tenants for any single KType
// (hundreds of thousands of journal lines, recurring templates,
// deals) while still firing well before a typical worker process
// runs out of heap.
//
// Declared var rather than const so integration tests can temporarily
// lower it to a value reachable from a few hundred test rows. Treat
// as effectively immutable in production code paths — never write to
// this from non-test code.
var ListAllMaxRows = 100_000

// ErrListAllExceedsCap is returned by ListAll / ListByField when the
// accumulated row count crosses ListAllMaxRows mid-walk. The error
// wraps additional context (ktype, rows, cap) via fmt.Errorf so
// callers can log it directly. Callers that need to process larger
// data sets should switch to the streaming PGStore.ForEach iterator.
var ErrListAllExceedsCap = errors.New("record: ListAll exceeded max rows")

// EncodeCursor packs a (updated_at, id) pair into an opaque
// base64 token. The wire format is `<unix_nanos>|<uuid>` so future
// fields can be appended without breaking existing tokens — the
// decoder ignores trailing segments.
func EncodeCursor(updatedAt time.Time, id uuid.UUID) string {
	raw := fmt.Sprintf("%d|%s", updatedAt.UnixNano(), id.String())
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor reverses EncodeCursor. Empty tokens map to the zero
// (time, uuid) pair with a nil error so callers can treat the
// first-page case uniformly.
func DecodeCursor(token string) (time.Time, uuid.UUID, error) {
	if token == "" {
		return time.Time{}, uuid.Nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) < 2 {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: missing field", ErrInvalidCursor)
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	return time.Unix(0, nanos).UTC(), id, nil
}

// Store captures KRecord CRUD operations. Implementations must set tenant
// context on the underlying transaction before issuing queries.
type Store interface {
	Create(ctx context.Context, r KRecord) (*KRecord, error)
	Get(ctx context.Context, tenantID, id uuid.UUID) (*KRecord, error)
	List(ctx context.Context, tenantID uuid.UUID, filter ListFilter) ([]KRecord, error)
	Update(ctx context.Context, r KRecord) (*KRecord, error)
	Delete(ctx context.Context, tenantID, id, actorID uuid.UUID) error
}
