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

	// ZK Object Fabric per-tenant credentials. Populated by the
	// setup wizard against the ZK fabric console API at :8081 so
	// each tenant's attachments are encrypted under a tenant-
	// specific DEK. Empty values mean "fall back to the global
	// S3_BUCKET / S3_ENDPOINT env vars" (legacy MinIO path).
	ZKAccessKey string `json:"zk_access_key,omitempty"`
	ZKSecretKey string `json:"-"` // never serialise the secret
	ZKBucket    string `json:"zk_bucket,omitempty"`

	// BaseCurrency is the ISO-4217 functional currency for the
	// tenant. Defaults to USD on tenants created before migration
	// 000029. Used by PostJournalEntry to detect + auto-convert
	// foreign-currency lines.
	BaseCurrency string `json:"base_currency,omitempty"`
}

// HasZKFabric reports whether the tenant has been provisioned with
// per-tenant ZK Object Fabric credentials. Used by the attachment
// layer to decide whether to route uploads through the per-tenant
// bucket or fall back to the platform-wide MinIO store.
func (t *Tenant) HasZKFabric() bool {
	return t != nil && t.ZKAccessKey != "" && t.ZKSecretKey != "" && t.ZKBucket != ""
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
