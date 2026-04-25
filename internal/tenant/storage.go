package tenant

import (
	"context"

	"github.com/google/uuid"
)

// ZKCredentials is the per-tenant ZK Object Fabric configuration the
// attachment layer needs. Bucket scopes the writes to one logical
// container per tenant; AccessKey/SecretKey are the HMAC pair the
// fabric uses for SigV4 auth.
type ZKCredentials struct {
	AccessKey string
	SecretKey string
	Bucket    string
}

// ZKStorageResolver returns the per-tenant ZK fabric credentials or
// ok=false when the tenant has not been provisioned. The PerTenant
// S3Store routes through this to pick the right bucket per request.
//
// Defined as a thin interface here so files/zk_fabric.go can adapt
// it to its own TenantResolver shape without an import cycle.
type ZKStorageResolver interface {
	GetZKCredentials(ctx context.Context, tenantID uuid.UUID) (ZKCredentials, bool, error)
}

// GetZKCredentials reads the per-tenant ZK fabric columns off the
// tenants table. Returns ok=false when any of the three columns is
// blank (i.e. fall back to the platform-default MinIO bucket).
func (s *PGStore) GetZKCredentials(ctx context.Context, tenantID uuid.UUID) (ZKCredentials, bool, error) {
	t, err := s.Get(ctx, tenantID)
	if err != nil {
		return ZKCredentials{}, false, err
	}
	if !t.HasZKFabric() {
		return ZKCredentials{}, false, nil
	}
	return ZKCredentials{
		AccessKey: t.ZKAccessKey,
		SecretKey: t.ZKSecretKey,
		Bucket:    t.ZKBucket,
	}, true, nil
}
