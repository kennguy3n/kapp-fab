package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// zkTenantResolver bridges the tenant.PGStore (which knows how to
// read the per-tenant ZK Object Fabric credentials off the tenants
// row) to the files.TenantResolver shape the attachment layer
// expects. Splitting it here keeps the files package free of the
// tenant import — it only has to depend on its own TenantResolver
// interface, not on the concrete store.
type zkTenantResolver struct {
	tenants  *tenant.PGStore
	endpoint string
	region   string
}

// newZKTenantResolver wires a resolver for the supplied store.
// endpoint and region default to the platform-wide ZK fabric
// gateway URL; per-tenant rows can override them in the future
// (e.g. dedicated cells routed to a regional gateway).
func newZKTenantResolver(tenants *tenant.PGStore, endpoint, region string) *zkTenantResolver {
	return &zkTenantResolver{tenants: tenants, endpoint: endpoint, region: region}
}

// ResolveZKCredentials implements files.TenantResolver.
func (r *zkTenantResolver) ResolveZKCredentials(ctx context.Context, tenantID uuid.UUID) (files.S3StoreConfig, bool, error) {
	creds, ok, err := r.tenants.GetZKCredentials(ctx, tenantID)
	if err != nil || !ok {
		return files.S3StoreConfig{}, ok, err
	}
	return files.S3StoreConfig{
		Endpoint:       r.endpoint,
		Region:         r.region,
		Bucket:         creds.Bucket,
		AccessKey:      creds.AccessKey,
		SecretKey:      creds.SecretKey,
		ForcePathStyle: true,
	}, true, nil
}
