// Package authz defines the RBAC/ABAC policy evaluator used by API middleware
// and agent tool dispatch. Implementations will consult tenant-scoped roles
// and record-level attributes to authorize each action.
package authz
