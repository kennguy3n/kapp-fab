// Package record models generic KRecord storage. KRecords are tenant-scoped
// JSONB rows keyed by (tenant_id, id), validated against a KType schema.
package record

import (
	"context"
	"encoding/json"
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
type ListFilter struct {
	KType  string
	Status string
	Limit  int
	Offset int
}

// Store captures KRecord CRUD operations. Implementations must set tenant
// context on the underlying transaction before issuing queries.
type Store interface {
	Create(ctx context.Context, r KRecord) (*KRecord, error)
	Get(ctx context.Context, tenantID, id uuid.UUID) (*KRecord, error)
	List(ctx context.Context, tenantID uuid.UUID, filter ListFilter) ([]KRecord, error)
	Update(ctx context.Context, r KRecord) (*KRecord, error)
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
}
