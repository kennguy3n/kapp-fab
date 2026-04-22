// Package authz defines the RBAC/ABAC policy evaluator used by API middleware
// and agent tool dispatch. Implementations consult tenant-scoped roles in the
// `roles` table and (in later phases) record-level attributes to authorize
// each action.
package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrDenied is returned by Evaluator.Authorize when the actor is not permitted
// to perform the requested action.
var ErrDenied = errors.New("authz: denied")

// Permission pairs an action verb with a resource type. Actions use the
// dotted namespace convention documented in ARCHITECTURE.md §6 — for example,
// "crm.deal.read" or "hr.employee.write".
type Permission struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

// Evaluator authorizes actors (users, services, agents) against tenant roles.
type Evaluator interface {
	Authorize(ctx context.Context, tenantID, userID uuid.UUID, action, resource string) error
	ListPermissions(ctx context.Context, tenantID, userID uuid.UUID) ([]Permission, error)
}
