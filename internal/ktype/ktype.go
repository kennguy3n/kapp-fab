// Package ktype defines the KType schema registry. KTypes are the metadata
// contracts that describe business objects (deals, tasks, invoices, etc.);
// their schemas drive validation, list/form rendering, and agent tool surfaces.
package ktype

import (
	"context"
	"encoding/json"
	"time"
)

// KType is a versioned schema definition stored in the `ktypes` table.
type KType struct {
	Name      string          `json:"name"`
	Version   int             `json:"version"`
	Schema    json.RawMessage `json:"schema"`
	CreatedAt time.Time       `json:"created_at"`
}

// Registry manages KType registration and lookup.
type Registry interface {
	Register(ctx context.Context, kt KType) error
	Get(ctx context.Context, name string, version int) (*KType, error)
	List(ctx context.Context) ([]KType, error)
}
