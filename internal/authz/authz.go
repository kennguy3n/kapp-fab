// Package authz defines the RBAC/ABAC policy evaluator used by API middleware
// and agent tool dispatch. Implementations consult tenant-scoped roles in the
// `roles` table and (in later phases) record-level attributes to authorize
// each action.
package authz

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// ErrDenied is returned by Evaluator.Authorize when the actor is not permitted
// to perform the requested action.
var ErrDenied = errors.New("authz: denied")

// Permission pairs an action verb with a resource type. Actions use the
// dotted namespace convention documented in ARCHITECTURE.md §6 — for example,
// "crm.deal.read" or "hr.employee.write".
//
// Conditions, when non-empty, restrict the grant to records whose attributes
// match the rule (e.g. {"owner_only": true} only grants the action on records
// the actor created).
type Permission struct {
	Action     string          `json:"action"`
	Resource   string          `json:"resource"`
	Conditions json.RawMessage `json:"conditions,omitempty"`
}

// Evaluator authorizes actors (users, services, agents) against tenant roles.
//
// AuthorizeRecord is the record-aware variant: callers pass an attribute bag
// describing the record (owner uuid, status, etc.) and the evaluator filters
// permissions whose conditions do not match. A permission with empty
// conditions is unconditional (matches Authorize semantics).
type Evaluator interface {
	Authorize(ctx context.Context, tenantID, userID uuid.UUID, action, resource string) error
	AuthorizeRecord(ctx context.Context, tenantID, userID uuid.UUID, action, resource string, recordAttrs map[string]any) error
	ListPermissions(ctx context.Context, tenantID, userID uuid.UUID) ([]Permission, error)
	ListRoles(ctx context.Context, tenantID, userID uuid.UUID) ([]string, error)
	InvalidateUser(tenantID, userID uuid.UUID)
	InvalidateTenant(tenantID uuid.UUID)
}
