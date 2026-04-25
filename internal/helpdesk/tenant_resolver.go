package helpdesk

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGTenantResolver looks up the tenant for an inbound recipient by
// querying tenant_support_domains. It uses the admin pool because the
// inbound email handler runs before any tenant context is set —
// migration 000031 includes a bypass policy for this case.
type PGTenantResolver struct {
	adminPool *pgxpool.Pool
}

// NewPGTenantResolver wires a resolver from the admin pool. The admin
// pool is required so the SELECT on tenant_support_domains executes
// outside any tenant's RLS context.
func NewPGTenantResolver(adminPool *pgxpool.Pool) *PGTenantResolver {
	return &PGTenantResolver{adminPool: adminPool}
}

// ResolveByRecipient extracts the host portion of a recipient address
// (`local@host`) and looks it up in tenant_support_domains. Lookups
// are case-insensitive (the unique index is on lower(domain)).
func (r *PGTenantResolver) ResolveByRecipient(ctx context.Context, recipient string) (uuid.UUID, error) {
	host := extractHost(recipient)
	if host == "" {
		return uuid.Nil, fmt.Errorf("%w: malformed recipient %q", ErrInvalidEmail, recipient)
	}
	row := r.adminPool.QueryRow(ctx,
		`SELECT tenant_id FROM tenant_support_domains WHERE lower(domain) = lower($1) LIMIT 1`,
		host,
	)
	var id uuid.UUID
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrUnknownRecipient
		}
		return uuid.Nil, fmt.Errorf("helpdesk: resolve recipient: %w", err)
	}
	return id, nil
}

// extractHost returns the lowercased host portion of a recipient
// address. Returns the empty string when the address has no `@`.
func extractHost(recipient string) string {
	at := strings.LastIndex(recipient, "@")
	if at < 0 || at == len(recipient)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(recipient[at+1:]))
}
