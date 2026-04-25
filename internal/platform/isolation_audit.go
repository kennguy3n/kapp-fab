package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IsolationCheck names a single assertion in the audit report.
type IsolationCheck struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
}

// IsolationReport is the structured result returned to the
// /admin/isolation-audit handler. A single failed check flips Passed
// to false so the operator can fail fast on a green/red signal.
type IsolationReport struct {
	Passed   bool             `json:"passed"`
	RanAt    time.Time        `json:"ran_at"`
	Duration string           `json:"duration"`
	Checks   []IsolationCheck `json:"checks"`
}

// IsolationAuditor runs runtime checks that verify the platform's
// tenant-isolation invariants. It uses the admin pool for cross-
// tenant scans (RLS metadata reads, GUC-less probes) and a regular
// app pool to execute SET LOCAL app.tenant_id from inside a
// transaction.
type IsolationAuditor struct {
	pool      *pgxpool.Pool
	adminPool *pgxpool.Pool
}

// NewIsolationAuditor wires the auditor. adminPool is required —
// without it we cannot run the metadata or GUC-less probes.
func NewIsolationAuditor(pool, adminPool *pgxpool.Pool) *IsolationAuditor {
	if pool == nil || adminPool == nil {
		panic("platform: isolation auditor requires non-nil pools")
	}
	return &IsolationAuditor{pool: pool, adminPool: adminPool}
}

// Run executes every check and returns the structured report.
//
// Checks:
//  1. RLS coverage — every public table containing a tenant_id column
//     has rowsecurity = true.
//  2. Cross-tenant probe — opens a row under tenant A, asserts a
//     SELECT under tenant B yields zero rows.
//  3. Default-deny — issuing a SELECT without app.tenant_id set
//     returns zero rows on a tenant-scoped table.
func (a *IsolationAuditor) Run(ctx context.Context) (*IsolationReport, error) {
	start := time.Now().UTC()
	report := &IsolationReport{Passed: true, RanAt: start}
	report.Checks = append(report.Checks, a.checkRLSCoverage(ctx))
	report.Checks = append(report.Checks, a.checkDefaultDeny(ctx))
	report.Checks = append(report.Checks, a.checkCrossTenantProbe(ctx))
	for _, c := range report.Checks {
		if !c.Passed {
			report.Passed = false
			break
		}
	}
	report.Duration = time.Since(start).Round(time.Millisecond).String()
	return report, nil
}

func (a *IsolationAuditor) checkRLSCoverage(ctx context.Context) IsolationCheck {
	t := time.Now()
	check := IsolationCheck{Name: "rls_coverage_on_tenant_scoped_tables"}
	rows, err := a.adminPool.Query(ctx,
		`SELECT c.relname
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		   JOIN pg_attribute  a ON a.attrelid = c.oid
		  WHERE n.nspname = 'public'
		    AND c.relkind = 'r'
		    AND a.attname = 'tenant_id'
		    AND a.attnum  > 0
		    AND NOT a.attisdropped
		    AND c.relrowsecurity = false`,
	)
	if err != nil {
		check.Detail = fmt.Sprintf("query failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			check.Detail = fmt.Sprintf("scan failed: %v", err)
			check.Elapsed = time.Since(t).String()
			return check
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		check.Passed = true
		check.Detail = "every tenant_id-bearing table has RLS enabled"
	} else {
		check.Detail = fmt.Sprintf("RLS missing on: %v", missing)
	}
	check.Elapsed = time.Since(t).String()
	return check
}

func (a *IsolationAuditor) checkDefaultDeny(ctx context.Context) IsolationCheck {
	t := time.Now()
	check := IsolationCheck{Name: "default_deny_without_guc"}
	// `SET LOCAL` is transaction-scoped, so the empty-GUC scenario
	// must be staged inside an explicit transaction; on a bare
	// pooled connection it would revert before the SELECT and the
	// count would silently run against whatever residual session
	// state the connection inherited. We rollback at the end so no
	// session state leaks back into the pool.
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		check.Detail = fmt.Sprintf("begin failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id TO ''"); err != nil {
		check.Detail = fmt.Sprintf("could not unset app.tenant_id: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&count); err != nil {
		check.Detail = fmt.Sprintf("count failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	if count == 0 {
		check.Passed = true
		check.Detail = "audit_log returned 0 rows without app.tenant_id set"
	} else {
		check.Detail = fmt.Sprintf("audit_log returned %d rows without app.tenant_id set — RLS bypass", count)
	}
	check.Elapsed = time.Since(t).String()
	return check
}

func (a *IsolationAuditor) checkCrossTenantProbe(ctx context.Context) IsolationCheck {
	t := time.Now()
	check := IsolationCheck{Name: "cross_tenant_probe_returns_zero"}
	tenantA := uuid.New()
	tenantB := uuid.New()
	probeID := uuid.New().String()

	// `tenant_features.tenant_id` carries an FK to `tenants(id)`, so
	// the probe needs real rows on both sides before it can insert.
	// Seed two synthetic, archived tenants on the admin pool, run the
	// probe under their GUCs, then drop them on the way out.
	if err := a.seedProbeTenant(ctx, tenantA, probeID+"-a"); err != nil {
		check.Detail = fmt.Sprintf("seed tenantA failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	defer a.dropProbeTenant(ctx, tenantA, probeID)
	if err := a.seedProbeTenant(ctx, tenantB, probeID+"-b"); err != nil {
		check.Detail = fmt.Sprintf("seed tenantB failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	defer a.dropProbeTenant(ctx, tenantB, probeID)

	// Tenant A: insert a synthetic feature flag we own.
	if err := a.runUnderTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_features (tenant_id, feature_key, enabled)
			 VALUES ($1, $2, true)
			 ON CONFLICT (tenant_id, feature_key) DO NOTHING`,
			tenantA, "_isolation_probe_"+probeID,
		)
		return err
	}); err != nil {
		check.Detail = fmt.Sprintf("seed under tenantA failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}
	// Tenant B: confirm zero rows for the probe key.
	var seen int64
	if err := a.runUnderTenant(ctx, tenantB, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM tenant_features WHERE feature_key = $1`,
			"_isolation_probe_"+probeID,
		).Scan(&seen)
	}); err != nil {
		check.Detail = fmt.Sprintf("read under tenantB failed: %v", err)
		check.Elapsed = time.Since(t).String()
		return check
	}

	// Best-effort cleanup; a leftover row is annoying but does not
	// invalidate the probe. We re-acquire under tenantA's GUC.
	_ = a.runUnderTenant(ctx, tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM tenant_features WHERE feature_key = $1`,
			"_isolation_probe_"+probeID,
		)
		return err
	})

	if seen == 0 {
		check.Passed = true
		check.Detail = "tenantB saw 0 rows of tenantA's probe"
	} else {
		check.Detail = fmt.Sprintf("tenantB saw %d rows of tenantA's probe — RLS leak", seen)
	}
	check.Elapsed = time.Since(t).String()
	return check
}

func (a *IsolationAuditor) runUnderTenant(ctx context.Context, tenantID uuid.UUID, fn func(context.Context, pgx.Tx) error) error {
	if a.pool == nil {
		return errors.New("platform: app pool unwired")
	}
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := fn(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// seedProbeTenant inserts a synthetic, archived tenant row under the
// admin pool so the cross-tenant probe can satisfy FK constraints. The
// `_isolation_probe_*` slug + `archived` status keep these rows out of
// every product surface (login, search, billing, scheduler).
func (a *IsolationAuditor) seedProbeTenant(ctx context.Context, tenantID uuid.UUID, slug string) error {
	if a.adminPool == nil {
		return errors.New("platform: admin pool unwired")
	}
	_, err := a.adminPool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name, cell, status, plan)
		 VALUES ($1, $2, $2, 'isolation-audit', 'archived', 'free')
		 ON CONFLICT (id) DO NOTHING`,
		tenantID, "_isolation_probe_"+slug,
	)
	return err
}

// dropProbeTenant removes the rows seedProbeTenant created. We drop
// dependent feature rows first because tenant_features.tenant_id has
// no ON DELETE CASCADE.
func (a *IsolationAuditor) dropProbeTenant(ctx context.Context, tenantID uuid.UUID, probeID string) {
	if a.adminPool == nil {
		return
	}
	_, _ = a.adminPool.Exec(ctx,
		`DELETE FROM tenant_features WHERE tenant_id = $1 AND feature_key = $2`,
		tenantID, "_isolation_probe_"+probeID,
	)
	_, _ = a.adminPool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
}
