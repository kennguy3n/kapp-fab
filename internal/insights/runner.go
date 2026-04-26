package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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

// Runner executes insights queries with cache-awareness and per-tenant
// statement timeouts. It wraps reporting.Runner so the underlying
// query grammar (sources, filters, aggregations, sort, limit) is the
// same. Cache hits return without touching reporting at all.
type Runner struct {
	pool      *pgxpool.Pool
	cache     *CacheStore
	queries   *QueryStore
	reporting *reporting.Runner
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
		return nil, errors.New("insights: tenant id required")
	}
	if err := opts.Definition.Validate(); err != nil {
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
	result, err := r.reporting.RunWithStatementTimeout(ctx, tenantID, def, r.timeout)
	if err != nil {
		return nil, fmt.Errorf("insights: execute: %w", err)
	}
	return result, nil
}

// RunSaved fetches the persisted query, applies its TTL, and runs.
// FilterParams may be nil for queries with no parameter inputs.
func (r *Runner) RunSaved(ctx context.Context, tenantID, queryID uuid.UUID, filterParams map[string]any, bypassCache bool) (*RunResult, error) {
	if r.queries == nil {
		return nil, errors.New("insights: query store not wired")
	}
	q, err := r.queries.Get(ctx, tenantID, queryID)
	if err != nil {
		return nil, err
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
