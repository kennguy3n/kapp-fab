// Package tenant defines the Tenant model and lifecycle interfaces. Every
// Kapp mutation is scoped to a Tenant; the Service interface captures the
// operations the control plane exposes for managing tenant records.
package tenant

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status represents the lifecycle state of a tenant.
type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusArchived  Status = "archived"
	StatusDeleting  Status = "deleting"
)

// Tenant mirrors a row in the `tenants` table.
type Tenant struct {
	ID        uuid.UUID       `json:"id"`
	Slug      string          `json:"slug"`
	Name      string          `json:"name"`
	Cell      string          `json:"cell"`
	Status    Status          `json:"status"`
	Plan      string          `json:"plan"`
	Quota     json.RawMessage `json:"quota"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// CreateInput is the request shape for provisioning a new tenant.
type CreateInput struct {
	Slug  string
	Name  string
	Cell  string
	Plan  string
	Quota json.RawMessage
}

// Service encapsulates tenant lifecycle operations.
type Service interface {
	Create(ctx context.Context, input CreateInput) (*Tenant, error)
	Get(ctx context.Context, id uuid.UUID) (*Tenant, error)
	GetBySlug(ctx context.Context, slug string) (*Tenant, error)
	List(ctx context.Context) ([]Tenant, error)
	Suspend(ctx context.Context, id uuid.UUID) error
	Activate(ctx context.Context, id uuid.UUID) error
	Archive(ctx context.Context, id uuid.UUID) error
	Delete(ctx context.Context, id uuid.UUID) error
}
