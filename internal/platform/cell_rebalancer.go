package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Cell rebalancing — tenant migration between cells.
//
// A Rebalancer moves a tenant from one cell to another by repointing
// tenants.cell_id. It is used in two situations:
//
//  1. Operator-driven: an operator rebalances a hot cell or relocates a
//     tenant for data-residency reasons (intended HTTP surface:
//     POST /api/v1/admin/tenants/{id}/migrate-cell — wired by the API
//     service; the handler delegates to MigrateTenant).
//  2. Autoscaler-driven: when a cell is being scaled down, its tenants
//     must be drained onto other cells before the cell is deprovisioned
//     (see AutoscaleEngine.drainCell).
//
// The move is transactional: the tenants row update and the
// `tenant.cell_migrated` audit entry commit together, so a tenant is
// never recorded as migrated without the cell_id actually changing (and
// vice versa). The `tenants` table is a control-plane table with no RLS
// of its own, but the audit_log IS tenant-scoped, so the whole unit runs
// inside a WithTenantTx that sets app.tenant_id = the migrating tenant.

// AuditActionCellMigrated is the audit_log action recorded for a tenant
// cell migration. Mirrors the dotted "tenant.<verb>" convention used by
// tenant.tier_upgrade.
const AuditActionCellMigrated = "tenant.cell_migrated"

// DefaultCellID is the implicit cell a tenant belongs to when
// tenants.cell_id is NULL. It mirrors the seed row in
// migrations/000041_cell_capacity.sql and the COALESCE in the
// autoscaler's cell snapshot query.
const DefaultCellID = "default"

// ErrNoOpMigration is returned when the source and destination cell are
// the same after normalisation — there is nothing to migrate.
var ErrNoOpMigration = errors.New("rebalance: source and destination cell are identical")

// ErrTenantNotOnSourceCell is returned when no tenant row matched the
// (tenant_id, fromCell) pair: either the tenant does not exist or it has
// already moved off the named source cell (e.g. a concurrent migration).
var ErrTenantNotOnSourceCell = errors.New("rebalance: tenant not found on source cell")

// CacheInvalidator is the optional hook the Rebalancer calls after a
// successful migration so an in-process tenant cache can drop its stale
// entry. Cross-process caches (the API replicas) rely on the tenant
// cache's short TTL rather than this hook, which is local to the
// process holding the Rebalancer.
type CacheInvalidator interface {
	InvalidateTenant(tenantID uuid.UUID)
}

// tenantCellMove is the validated, normalised migration request handed
// to the persistence layer.
type tenantCellMove struct {
	TenantID   uuid.UUID
	FromCellID string
	ToCellID   string
}

// tenantCellRepo abstracts the transactional persistence the Rebalancer
// needs. Production uses pgxTenantCellRepo; tests inject a fake so the
// orchestration logic is exercised without a live database.
type tenantCellRepo interface {
	// moveTenantCell repoints tenants.cell_id from FromCellID to
	// ToCellID iff the tenant currently sits on FromCellID, writing the
	// audit entry in the same transaction. It returns true when exactly
	// one row moved, false (nil error) when no row matched.
	moveTenantCell(ctx context.Context, m tenantCellMove) (bool, error)
}

// Rebalancer migrates tenants between cells.
type Rebalancer struct {
	repo        tenantCellRepo
	invalidator CacheInvalidator
	logger      *slog.Logger
}

// NewRebalancer binds a Rebalancer to a pool and (optional) audit
// logger. A nil auditLogger disables audit writes (the migration still
// happens); a nil logger falls back to slog.Default.
func NewRebalancer(pool *pgxpool.Pool, auditLogger audit.Logger, logger *slog.Logger) *Rebalancer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Rebalancer{
		repo:   &pgxTenantCellRepo{pool: pool, audit: auditLogger},
		logger: logger,
	}
}

// WithCacheInvalidator attaches a cache-invalidation hook and returns
// the receiver for chaining.
func (r *Rebalancer) WithCacheInvalidator(inv CacheInvalidator) *Rebalancer {
	r.invalidator = inv
	return r
}

// MigrateTenant moves tenantID from fromCellID to toCellID. An empty
// fromCellID is normalised to the implicit "default" cell, matching the
// autoscaler's treatment of a NULL cell_id. The call is idempotent in
// the sense that re-running it after the tenant has already moved
// returns ErrTenantNotOnSourceCell rather than corrupting state.
func (r *Rebalancer) MigrateTenant(ctx context.Context, tenantID uuid.UUID, fromCellID, toCellID string) error {
	if r == nil || r.repo == nil {
		return errors.New("rebalance: rebalancer not configured")
	}
	move, err := normalizeMigration(tenantID, fromCellID, toCellID)
	if err != nil {
		return err
	}
	moved, err := r.repo.moveTenantCell(ctx, move)
	if err != nil {
		return fmt.Errorf("rebalance: migrate tenant %s: %w", tenantID, err)
	}
	if !moved {
		return fmt.Errorf("%w: tenant %s, cell %q", ErrTenantNotOnSourceCell, tenantID, move.FromCellID)
	}
	if r.invalidator != nil {
		r.invalidator.InvalidateTenant(tenantID)
	}
	r.logger.Info("rebalance: tenant migrated",
		"tenant_id", tenantID, "from_cell", move.FromCellID, "to_cell", move.ToCellID)
	return nil
}

// normalizeMigration validates and canonicalises a migration request.
// It is pure so the validation rules are unit-tested without a database.
func normalizeMigration(tenantID uuid.UUID, fromCellID, toCellID string) (tenantCellMove, error) {
	if tenantID == uuid.Nil {
		return tenantCellMove{}, errors.New("rebalance: tenant id required")
	}
	// The source is normalised so an empty/NULL placement resolves to
	// the implicit "default" cell. The destination is NOT normalised:
	// an empty destination is an operator mistake, not a request to
	// move the tenant onto the default cell.
	from := normalizeCellID(fromCellID)
	to := strings.TrimSpace(toCellID)
	if to == "" {
		return tenantCellMove{}, errors.New("rebalance: destination cell id required")
	}
	if from == to {
		return tenantCellMove{}, ErrNoOpMigration
	}
	return tenantCellMove{TenantID: tenantID, FromCellID: from, ToCellID: to}, nil
}

// normalizeCellID trims whitespace and maps the empty string onto the
// implicit "default" cell so callers can pass "" to mean "wherever the
// tenant is now (the default cell)".
func normalizeCellID(cellID string) string {
	c := strings.TrimSpace(cellID)
	if c == "" {
		return DefaultCellID
	}
	return c
}

// pgxTenantCellRepo is the production tenantCellRepo backed by Postgres.
type pgxTenantCellRepo struct {
	pool  *pgxpool.Pool
	audit audit.Logger
}

func (p *pgxTenantCellRepo) moveTenantCell(ctx context.Context, m tenantCellMove) (bool, error) {
	if p.pool == nil {
		return false, errors.New("rebalance: nil pool")
	}
	var moved bool
	err := dbutil.WithTenantTx(ctx, p.pool, m.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// COALESCE folds a NULL cell_id onto 'default' so a tenant that
		// has never been explicitly placed can still be migrated off the
		// implicit default cell. The FK on tenants.cell_id guarantees
		// ToCellID names a real cell — an unknown target surfaces as a
		// foreign-key violation here rather than silently succeeding.
		tag, err := tx.Exec(ctx,
			`UPDATE tenants
			    SET cell_id = $1, updated_at = now()
			  WHERE id = $2
			    AND COALESCE(cell_id, $3) = $4`,
			m.ToCellID, m.TenantID, DefaultCellID, m.FromCellID)
		if err != nil {
			return fmt.Errorf("update tenant cell: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// No row matched: tenant gone or already off the source
			// cell. Leave moved=false; the empty tx commits harmlessly.
			return nil
		}
		moved = true
		if p.audit != nil {
			payload, err := json.Marshal(map[string]string{
				"from_cell": m.FromCellID,
				"to_cell":   m.ToCellID,
			})
			if err != nil {
				return fmt.Errorf("marshal audit context: %w", err)
			}
			if err := p.audit.LogTx(ctx, tx, audit.Entry{
				TenantID:  m.TenantID,
				ActorKind: audit.ActorSystem,
				Action:    AuditActionCellMigrated,
				Context:   payload,
			}); err != nil {
				return fmt.Errorf("audit cell migration: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
}
