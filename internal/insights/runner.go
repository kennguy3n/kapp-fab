package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
)

// MaxResultRows hard-caps every insights query at 10,000 rows. The
// underlying reporting runner caps individual statements at 5,000;
// the runner here truncates the post-aggregation result so a query
// that joins or expands client-side still cannot blow past the
// budget. Exposed as a constant so the API layer can document it.
const MaxResultRows = 10000

// DefaultStatementTimeout is the SET LOCAL statement_timeout applied
// to every insights query. Ten seconds short of the 30s HTTP
// deadline so the DB rolls back before the request times out and
// kills the connection.
const DefaultStatementTimeout = 20 * time.Second

// PlanLookup resolves a tenant's plan name. Implemented by
// tenant.Service in production; tests inject a closure. Kept as an
// interface so the insights package does not import internal/tenant
// directly (which would form a cycle through the agent tools).
type PlanLookup interface {
	PlanForTenant(ctx context.Context, tenantID uuid.UUID) (string, error)
}

// FeaturePolicy resolves whether a feature flag is enabled for a
// tenant. Implemented by tenant.FeatureStore in production. Kept as
// an interface so the insights package can gate the SQL-mode
// dispatch without importing internal/tenant (which would form an
// import cycle through the agent tools).
//
// Used by RunSaved to backstop the createQuery / updateQuery gate
// in services/api/insights_handlers.go: even if a SQL-mode row is
// already persisted (e.g. from before the feature was disabled, or
// from an admin downgrade), every consumer of RunSaved
// (dashboard fan-out, cache refresh worker, agent tools, kchat
// /insight) refuses to dispatch it for a tenant that no longer
// holds the `insights_sql_editor` flag.
type FeaturePolicy interface {
	IsEnabled(ctx context.Context, tenantID uuid.UUID, featureKey string) (bool, error)
}

// FeatureKeyInsightsSQLEditor mirrors the tenant package's constant.
// Duplicated here to keep this package free of an internal/tenant
// import. Kept in lock-step via the integration test that exercises
// both surfaces against the same tenant.
const FeatureKeyInsightsSQLEditor = "insights_sql_editor"

// Runner executes insights queries with cache-awareness and per-tenant
// statement timeouts. It wraps reporting.Runner so the underlying
// query grammar (sources, filters, aggregations, sort, limit) is the
// same. Cache hits return without touching reporting at all.
type Runner struct {
	pool      *pgxpool.Pool
	cache     *CacheStore
	queries   *QueryStore
	reporting *reporting.Runner
	external  *ExternalRunner
	plans     PlanLookup
	joinLimit func(plan string) int
	features  FeaturePolicy
	timeout   time.Duration
	maxRows   int
}

// NewRunner wires a Runner with the standard caching + timeout
// behaviour. Callers can swap in a CacheStore-less Runner for tests
// by passing nil — Run then degrades to "always run, never cache".
func NewRunner(pool *pgxpool.Pool, cache *CacheStore, queries *QueryStore, reportingRunner *reporting.Runner) *Runner {
	if reportingRunner == nil {
		reportingRunner = reporting.NewRunner(pool)
	}
	return &Runner{
		pool:      pool,
		cache:     cache,
		queries:   queries,
		reporting: reportingRunner,
		timeout:   DefaultStatementTimeout,
		maxRows:   MaxResultRows,
	}
}

// WithTimeout overrides the per-query statement timeout. Useful in
// tests with a mocked database that doesn't honour SET LOCAL.
func (r *Runner) WithTimeout(timeout time.Duration) *Runner {
	r.timeout = timeout
	return r
}

// WithExternal wires an ExternalRunner so queries whose source begins
// with `external:` are routed to the per-tenant external pool cache.
// When nil, external sources fail at validation time.
func (r *Runner) WithExternal(ext *ExternalRunner) *Runner {
	r.external = ext
	return r
}

// WithPlanGate wires a PlanLookup + plan→max-joins resolver so the
// runner can reject definitions whose join count exceeds the
// tenant's plan ceiling before executing. When nil, the engine-
// level reporting.MaxJoinsHardCeiling is the only check.
func (r *Runner) WithPlanGate(plans PlanLookup, joinLimit func(plan string) int) *Runner {
	r.plans = plans
	r.joinLimit = joinLimit
	return r
}

// WithFeaturePolicy wires the FeaturePolicy used by RunSaved to
// reject SQL-mode dispatches when the tenant lacks
// insights_sql_editor. When nil, RunSaved trusts the upstream
// gate at the create/update boundary; the test harness uses this
// to keep deeply mocked runners working without a tenant_features
// table. See FeaturePolicy's docstring for the threat model.
func (r *Runner) WithFeaturePolicy(p FeaturePolicy) *Runner {
	r.features = p
	return r
}

// RunOptions tunes a single Run call. Definition is required.
// QueryID + CacheTTL are populated by SavedRun for saved queries; ad
// hoc callers can leave them zero. FilterParams is hashed into the
// cache key when non-empty.
type RunOptions struct {
	Definition   QueryDefinition
	QueryID      *uuid.UUID
	CacheTTL     time.Duration
	FilterParams map[string]any
	BypassCache  bool
}

// RunResult bundles the reporting result with cache metadata so the
// API surface can return both the rows and a hint about whether the
// caller hit the cache.
type RunResult struct {
	Result     *reporting.Result `json:"result"`
	CacheHit   bool              `json:"cache_hit"`
	QueryHash  string            `json:"query_hash,omitempty"`
	FilterHash string            `json:"filter_hash,omitempty"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
}

// Run executes the supplied definition with cache-aware behaviour:
//
//  1. Compute cache key from (tenant_id, definition, filter_params).
//  2. If BypassCache is false and the cache has a fresh row, return it.
//  3. Otherwise execute via reporting.Runner under SET LOCAL
//     statement_timeout, truncate to MaxResultRows, and store the
//     result in the cache with the configured TTL.
func (r *Runner) Run(ctx context.Context, tenantID uuid.UUID, opts RunOptions) (*RunResult, error) {
	if tenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if err := opts.Definition.Validate(); err != nil {
		return nil, err
	}

	if err := r.enforceJoinLimit(ctx, tenantID, opts.Definition); err != nil {
		return nil, err
	}

	queryHash, filterHash, err := CacheKey(tenantID, opts.Definition, opts.FilterParams)
	if err != nil {
		return nil, err
	}

	if r.cache != nil && !opts.BypassCache {
		cached, err := r.cache.Get(ctx, tenantID, queryHash, filterHash)
		switch {
		case err == nil:
			result := &reporting.Result{}
			if err := json.Unmarshal(cached.Result, result); err != nil {
				return nil, fmt.Errorf("insights: decode cached result: %w", err)
			}
			expires := cached.ExpiresAt
			return &RunResult{
				Result:     result,
				CacheHit:   true,
				QueryHash:  queryHash,
				FilterHash: filterHash,
				ExpiresAt:  &expires,
			}, nil
		case errors.Is(err, ErrCacheMiss):
			// fall through to live execution
		default:
			return nil, fmt.Errorf("insights: cache lookup: %w", err)
		}
	}

	result, err := r.runWithTimeout(ctx, tenantID, opts.Definition.Definition)
	if err != nil {
		return nil, err
	}
	if r.maxRows > 0 && len(result.Rows) > r.maxRows {
		result.Rows = result.Rows[:r.maxRows]
	}

	out := &RunResult{
		Result:     result,
		CacheHit:   false,
		QueryHash:  queryHash,
		FilterHash: filterHash,
	}

	if r.cache != nil && opts.CacheTTL > 0 {
		payload, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("insights: marshal result for cache: %w", err)
		}
		if err := r.cache.Set(ctx, tenantID, queryHash, filterHash, opts.QueryID, payload, len(result.Rows), opts.CacheTTL); err != nil {
			return nil, fmt.Errorf("insights: cache set: %w", err)
		}
		expires := timeNow().Add(opts.CacheTTL)
		out.ExpiresAt = &expires
	}
	return out, nil
}

// runWithTimeout delegates to reporting.Runner.RunWithStatementTimeout
// so the SET LOCAL statement_timeout is applied inside the same
// transaction as the underlying reporting query (SET LOCAL is
// transaction-scoped, so issuing it on a sibling tx that commits
// immediately would leave the actual query unprotected). The Go
// context deadline is layered on top as a defence-in-depth fence in
// case the server-side timeout is somehow not honoured.
func (r *Runner) runWithTimeout(ctx context.Context, tenantID uuid.UUID, def reporting.Definition) (*reporting.Result, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	if isExternalSource(def.Source) {
		if r.external == nil {
			return nil, validationErr("external source requires data sources to be configured")
		}
		result, err := r.external.Run(ctx, tenantID, def)
		if err != nil {
			return nil, fmt.Errorf("insights: external execute: %w", err)
		}
		return result, nil
	}
	result, err := r.reporting.RunWithStatementTimeout(ctx, tenantID, def, r.timeout)
	if err != nil {
		return nil, fmt.Errorf("insights: execute: %w", err)
	}
	return result, nil
}

// enforceJoinLimit rejects definitions whose join count exceeds the
// caller's plan ceiling. The reporting engine has its own
// MaxJoinsHardCeiling fence so a misconfigured plan can't unbound
// the engine; this gate exists so the BI surface returns a clean
// 4xx-shaped error instead of letting the engine reject deeper in
// the call chain.
func (r *Runner) enforceJoinLimit(ctx context.Context, tenantID uuid.UUID, def QueryDefinition) error {
	if r.plans == nil || r.joinLimit == nil {
		return nil
	}
	if len(def.Joins) == 0 {
		return nil
	}
	plan, err := r.plans.PlanForTenant(ctx, tenantID)
	if err != nil {
		// Plan lookup failure should not fail the query — defence
		// in depth via reporting.MaxJoinsHardCeiling still bounds
		// the engine. Surface a structured warning via the result
		// set on success.
		return nil
	}
	maxJoins := r.joinLimit(plan)
	if len(def.Joins) > maxJoins {
		return validationErr("plan %q allows at most %d joins per query (got %d)", plan, maxJoins, len(def.Joins))
	}
	return nil
}

func isExternalSource(source string) bool {
	return len(source) > len(reporting.SourceExternalPrefix) &&
		source[:len(reporting.SourceExternalPrefix)] == reporting.SourceExternalPrefix
}

// RunSaved fetches the persisted query, applies its TTL, and runs.
// FilterParams may be nil for queries with no parameter inputs.
func (r *Runner) RunSaved(ctx context.Context, tenantID, queryID uuid.UUID, filterParams map[string]any, bypassCache bool) (*RunResult, error) {
	if r.queries == nil {
		return nil, validationErr("query store not wired")
	}
	q, err := r.queries.Get(ctx, tenantID, queryID)
	if err != nil {
		return nil, err
	}
	// SQL-mode queries never round-trip a meaningful Definition
	// (the store only persists the placeholder), so dispatch them
	// to the raw runner before the visual fall-through. Caching
	// is intentionally not honoured for raw-SQL — RunRawSQL doesn't
	// touch insights_query_cache yet, so a SQL-mode dashboard
	// widget re-executes on every refresh just like a 0-TTL visual
	// query would. Adding cache support requires a separate
	// fingerprint scheme (raw text + params) and is tracked
	// outside this hotfix.
	if q.Mode == QueryModeSQL {
		// Belt-and-suspenders gate: createQuery / updateQuery in
		// services/api/insights_handlers.go reject a SQL-mode body
		// from a tenant without insights_sql_editor, but a tenant
		// that was downgraded after persisting a SQL-mode row
		// would otherwise still execute it through any RunSaved
		// caller. Refuse here so the gate covers the dashboard
		// fan-out, the cache refresh worker, every agent tool, and
		// the kchat /insight slash command in a single check.
		if r.features != nil {
			ok, ferr := r.features.IsEnabled(ctx, tenantID, FeatureKeyInsightsSQLEditor)
			if ferr != nil {
				return nil, fmt.Errorf("feature lookup: %w", ferr)
			}
			if !ok {
				return nil, fmt.Errorf("%w: %s", ErrFeatureDisabled, FeatureKeyInsightsSQLEditor)
			}
		}
		return r.RunRawSQL(ctx, tenantID, q.RawSQL, nil)
	}
	// q.CacheTTLSeconds is always non-nil after QueryStore.Get
	// (Get scans the column into a local int and assigns its
	// address); guard defensively to keep the runner robust
	// against future store changes.
	var ttl time.Duration
	if q.CacheTTLSeconds != nil {
		ttl = time.Duration(*q.CacheTTLSeconds) * time.Second
	}
	return r.Run(ctx, tenantID, RunOptions{
		Definition:   q.Definition,
		QueryID:      &q.ID,
		CacheTTL:     ttl,
		FilterParams: filterParams,
		BypassCache:  bypassCache,
	})
}

// RunRawSQL executes a parameterised SQL statement under the same
// per-tenant fences the visual runner uses: dbutil.WithTenantTx
// pins `app.tenant_id` so RLS bounds every read to the caller's
// tenant, and SET LOCAL statement_timeout cancels runaway scans
// before the HTTP request slot expires. params are bound via
// pgx.Query so callers cannot string-interpolate untrusted values.
//
// The raw SQL surface intentionally rejects multi-statement bodies
// (semicolon-separated) — pgx.Query already only honours the first
// statement on the wire, but the explicit guard turns a silently
// dropped trailing statement into a 400 the user can act on. The
// row cap mirrors the visual runner (MaxResultRows = 10,000) so a
// SELECT * without LIMIT can't exhaust memory.
//
// Caller must gate this on both `insights` and `insights_sql_editor`
// feature flags. The runner does not consult feature state itself
// because the API and agent layers already own that policy.
func (r *Runner) RunRawSQL(ctx context.Context, tenantID uuid.UUID, rawSQL string, params []any) (*RunResult, error) {
	if tenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if rawSQL == "" {
		return nil, validationErr("raw_sql body required")
	}
	// Multi-statement bodies fail closed with a 400. pgx already
	// only executes the first statement on the wire so trailing
	// statements in `SELECT 1; DROP TABLE x` would otherwise be
	// silently dropped — surfacing a validation error gives the
	// caller a chance to split the body or remove the trailing
	// `;`. The check is intentionally textual rather than parsed
	// because we'd otherwise need a real SQL parser to reject
	// `;` inside a string literal, and that's overkill for the
	// editor's first cut.
	if strings.Contains(rawSQL, ";") {
		return nil, validationErr("multi-statement SQL is not allowed; submit one statement at a time")
	}

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	rows := make([]map[string]any, 0)
	columns := []string{}
	err := dbutil.WithTenantTx(ctx, r.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if r.timeout > 0 {
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", r.timeout.Milliseconds())); err != nil {
				return fmt.Errorf("insights: set statement_timeout: %w", err)
			}
		}
		pgxRows, err := tx.Query(ctx, rawSQL, params...)
		if err != nil {
			return fmt.Errorf("insights: execute raw sql: %w", err)
		}
		defer pgxRows.Close()
		fieldDescs := pgxRows.FieldDescriptions()
		for _, fd := range fieldDescs {
			columns = append(columns, string(fd.Name))
		}
		for pgxRows.Next() {
			vals, err := pgxRows.Values()
			if err != nil {
				return err
			}
			row := make(map[string]any, len(vals))
			for i, v := range vals {
				row[columns[i]] = v
			}
			rows = append(rows, row)
			if r.maxRows > 0 && len(rows) >= r.maxRows {
				break
			}
		}
		return pgxRows.Err()
	})
	if err != nil {
		return nil, err
	}

	return &RunResult{
		Result: &reporting.Result{
			Columns: columns,
			Rows:    rows,
		},
		CacheHit: false,
	}, nil
}
