package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// DefaultLockTimeout caps how long any individual lock acquisition
// can wait, independent of statement_timeout. statement_timeout
// covers TOTAL query duration (lock wait + plan + execution); a
// query stuck behind an ACCESS EXCLUSIVE lock on a hot tenant
// table would otherwise burn the entire statement budget before
// surfacing the wait, masking the contention as a generic
// "statement timeout". lock_timeout fails fast with a distinct
// SQLSTATE (55P03 lock_not_available) so the operator-facing error
// names the actual blocker. Five seconds is well below the
// statement budget (so a contention failure produces 55P03, not
// 57014) and well above any realistic READ ONLY tx's natural lock
// acquisition (ACCESS SHARE locks are cheap and never wait on
// peers; only DDL on the same table would contend).
const DefaultLockTimeout = 5 * time.Second

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
//
// router routes the runner's own read transactions (RunRawSQL and the
// row_security probe inside it) to a replica pool when one is
// configured AND the replica is within KAPP_READ_REPLICA_LAG_TOLERANCE
// of the primary. Writes never originate from this runner — everything
// it executes is a SELECT, plus a Set Transaction Read Only fence so
// even a future bug emitting DML would error out instead of silently
// modifying replica state.
type Runner struct {
	router      *dbutil.PoolRouter
	cache       *CacheStore
	queries     *QueryStore
	reporting   *reporting.Runner
	external    *ExternalRunner
	plans       PlanLookup
	joinLimit   func(plan string) int
	features    FeaturePolicy
	timeout     time.Duration
	lockTimeout time.Duration
	maxRows     int
}

// NewRunner wires a Runner with the standard caching + timeout
// behaviour. Callers can swap in a CacheStore-less Runner for tests
// by passing nil — Run then degrades to "always run, never cache".
// The primary pool is used for the runner's bookkeeping (cache
// reads/writes, query saves) — replica routing is opt-in through
// NewRunnerWithRouter for callers that want their cold-path Run
// dispatches and RunRawSQL execution served by the replica.
func NewRunner(pool *pgxpool.Pool, cache *CacheStore, queries *QueryStore, reportingRunner *reporting.Runner) *Runner {
	return NewRunnerWithRouter(dbutil.NewPoolRouter(pool), cache, queries, reportingRunner)
}

// NewRunnerWithRouter is the replica-aware constructor — pass the
// process-wide PoolRouter so this runner shares the same lag sampler
// and falls back to the primary in lock-step with every other read
// path. If reportingRunner is nil, one is created from the same
// router so the inner SELECT executes on the replica too (the cache
// reads/writes on the runner side are unrelated to the reporting
// transaction).
func NewRunnerWithRouter(router *dbutil.PoolRouter, cache *CacheStore, queries *QueryStore, reportingRunner *reporting.Runner) *Runner {
	if router == nil {
		panic("insights: NewRunnerWithRouter requires a non-nil router")
	}
	if reportingRunner == nil {
		reportingRunner = reporting.NewRunnerWithRouter(router)
	}
	return &Runner{
		router:      router,
		cache:       cache,
		queries:     queries,
		reporting:   reportingRunner,
		timeout:     DefaultStatementTimeout,
		lockTimeout: DefaultLockTimeout,
		maxRows:     MaxResultRows,
	}
}

// WithTimeout overrides the per-query statement timeout. Useful in
// tests with a mocked database that doesn't honour SET LOCAL.
func (r *Runner) WithTimeout(timeout time.Duration) *Runner {
	r.timeout = timeout
	return r
}

// WithLockTimeout overrides the per-query lock_timeout applied to
// raw-SQL execution (RunRawSQL).  Setting <= 0 disables the
// SET LOCAL lock_timeout statement entirely — useful in tests
// against a mocked / non-Postgres backend.
func (r *Runner) WithLockTimeout(d time.Duration) *Runner {
	r.lockTimeout = d
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
// The raw SQL surface is gated by validateRawSQL, an AST-based
// validator (see internal/insights/sqlvalidate.go). It enforces
// five rules in order: non-empty body, parses via libpg_query,
// exactly one top-level statement, top statement is SELECT (with
// no IntoClause), and a tree walk rejecting system catalogs,
// nested non-SELECT statements (CTE-DML), and system or known-
// dangerous extension functions (pg_*, dblink_*, lo_import/export,
// set_config, schema-qualified pg_*). The previous textual
// `strings.Contains(rawSQL, ";")` heuristic is gone — it was both
// too strict (rejected `SELECT 'a;b'`) and too loose (would have
// missed `SELECT 1/**/;DROP TABLE x` under comment-stripping). The
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
	// validateRawSQL is the AST-level guard: multi-statement
	// rejection, SELECT-only at the root, no nested DML inside
	// CTEs/subqueries, no system-catalog or system-function
	// references. See validateRawSQL's docstring for the full
	// contract. It also rejects empty/whitespace-only bodies, so
	// the previous `rawSQL == ""` check here is redundant.
	//
	// The previous textual `strings.Contains(rawSQL, ";")` check
	// was both too strict (rejected harmless `SELECT 'a;b'`) and
	// too loose (missed `SELECT 1/**/;DROP TABLE x` once any
	// comment-stripping normalisation was added). The AST gives a
	// single source of truth for "exactly one statement, and that
	// statement is read-only".
	//
	// The per-tenant tx below is opened via WithReadOnlyTenantTx,
	// which sets pgx.TxOptions{AccessMode: pgx.ReadOnly} at BEGIN
	// time. This is retained as defense-in-depth: if a future
	// Postgres release adds a new statement node we don't yet
	// classify, the read-only access mode surfaces it as a
	// runtime error rather than letting it execute. Setting the
	// fence at BEGIN is strictly stronger than the previous
	// mid-callback `SET TRANSACTION READ ONLY` because it cannot
	// be lost to a future refactor that issues a data-touching
	// statement before the SET.
	if err := validateRawSQL(rawSQL); err != nil {
		return nil, err
	}

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	rows := make([]map[string]any, 0)
	columns := []string{}
	// WithReadOnlyTenantTx opens the transaction with
	// pgx.TxOptions{AccessMode: pgx.ReadOnly} so the read-only
	// fence is set at BEGIN, not via an in-transaction SET
	// TRANSACTION. This is strictly stronger than the previous
	// "SET TRANSACTION READ ONLY mid-callback" pattern because
	// it cannot be lost to a future refactor that issues a
	// data-touching statement before the SET. It also lets the
	// router serve this transaction from the replica when
	// configured + within KAPP_READ_REPLICA_LAG_TOLERANCE,
	// transparently falling back to the primary otherwise so
	// the SQL editor never appears to "stop working" just
	// because the replica is lagging.
	err := dbutil.WithReadOnlyTenantTx(ctx, r.router, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if r.timeout > 0 {
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", r.timeout.Milliseconds())); err != nil {
				return fmt.Errorf("insights: set statement_timeout: %w", err)
			}
		}
		// Lock acquisition budget is independent of total query
		// time.  Without this, a hostile or accidental query
		// against a table currently held under ACCESS EXCLUSIVE
		// (e.g. by a concurrent migration / CREATE INDEX) would
		// block until statement_timeout expired and surface as a
		// generic 57014 query_canceled instead of the actual
		// blocker (55P03 lock_not_available).  ACCESS SHARE locks
		// taken by a SELECT inside a READ ONLY tx never wait on
		// peers in a healthy DB, so DefaultLockTimeout (5s) is
		// effectively only ever hit when DDL contention is the
		// underlying cause — failing fast with a distinct
		// SQLSTATE makes that immediately recognisable in logs
		// and pg_stat_activity.
		if r.lockTimeout > 0 {
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", r.lockTimeout.Milliseconds())); err != nil {
				return fmt.Errorf("insights: set lock_timeout: %w", err)
			}
		}
		// Read-only fence is already set at BEGIN by
		// WithReadOnlyTenantTx (pgx.TxOptions{AccessMode:
		// pgx.ReadOnly}) — no in-transaction SET TRANSACTION
		// READ ONLY needed. Any DML against an RLS-scoped table
		// in the user-supplied SQL fails with PostgreSQL's
		// "cannot execute X in a read-only transaction" before
		// it can persist, which surfaces as a 400 rather than a
		// silent commit. The editor's purpose is read-only ad
		// hoc analysis; the visual runner has its own callback
		// path and is unaffected.
		// Defense-in-depth assertion: confirm RLS will actually
		// be enforced for this transaction before running the
		// user-supplied query.  Two ways the guarantee could
		// silently regress without this check:
		//
		//   1. A DBA sets `ALTER ROLE kapp_app SET row_security
		//      = off` or `ALTER DATABASE kapp SET row_security
		//      = off`.  Under the current role posture (kapp_app
		//      created without BYPASSRLS in migration 000003),
		//      Postgres itself fails closed for this case: the
		//      query errors instead of silently bypassing RLS.
		//      The assertion's real load-bearing coverage is the
		//      narrow but real intersection where Postgres would
		//      NOT fail closed:
		//
		//        (a) `BYPASSRLS` is granted to kapp_app (e.g. as
		//            an incident-response debugging shortcut that
		//            never got reverted) AND row_security=off —
		//            in this exact combination, RLS policies
		//            become permissive-mode-only and the query
		//            returns cross-tenant rows silently;
		//
		//        (b) kapp_app ends up owning a tenant-scoped
		//            table that lacks FORCE ROW LEVEL SECURITY
		//            (shouldn't happen, but a migration that
		//            runs CREATE TABLE under the wrong role
		//            could) — table owners bypass RLS by
		//            default, and with row_security=off the
		//            bypass is silent;
		//
		//        (c) a future PR proposes "grant BYPASSRLS to
		//            kapp_app for the SQL editor since it's
		//            validator-guarded" — the assertion makes
		//            that regression visible at every dispatch.
		//
		//      Outside that intersection (which is the default
		//      production posture today) Postgres already fails
		//      closed and this check is operationally redundant
		//      but cheap (single GUC read in the same round
		//      trip as the tenant probe).  The defense-in-depth
		//      principle is "fail closed loudly when the
		//      invariant might erode", not "duplicate Postgres's
		//      own fail-closed guarantee under happy-path role
		//      configuration".
		//
		//   2. dbutil.WithTenantTx returns successfully but
		//      `app.tenant_id` is unset (e.g. if a future
		//      refactor moves SetTenantContext to a different
		//      call site and forgets it on this path).  RLS
		//      policies on tenant-scoped tables would then
		//      evaluate `current_setting('app.tenant_id',
		//      true)` to the empty string and refuse all rows,
		//      OR — if any policy has a permissive fallback —
		//      return cross-tenant data.  Asserting the GUC is
		//      non-empty makes the contract explicit and the
		//      regression loud.
		//
		// We pull both values in one round-trip rather than two
		// separate SHOWs so the assertion adds at most one
		// query to the per-call latency budget.  current_setting
		// is read-only and benign — the validator's denylist
		// blocks set_config but allows current_setting precisely
		// for cases like this.
		//
		// COALESCE on app.tenant_id is structural, not cosmetic:
		// current_setting(name, missing_ok=true) returns SQL NULL
		// when the GUC has NEVER been set on this session.  pgx
		// cannot scan SQL NULL into a plain Go string and would
		// surface the missing-GUC case as a generic
		// "insights: probe rls/tenant context" Scan error
		// instead of the specific "empty app.tenant_id GUC"
		// rejection below — defeating the whole point of the
		// diagnostic.  Mapping NULL -> '' inside the SELECT
		// preserves the original intent: empty string means
		// "the GUC was either never set or set to ''", and the
		// tenantGUC == "" branch handles both uniformly with a
		// clear operator-facing message.
		var rowSecurity, tenantGUC string
		if err := tx.QueryRow(ctx,
			"SELECT current_setting('row_security'), COALESCE(current_setting('app.tenant_id', true), '')",
		).Scan(&rowSecurity, &tenantGUC); err != nil {
			return fmt.Errorf("insights: probe rls/tenant context: %w", err)
		}
		if rowSecurity != "on" {
			// Tag with ErrSecurityAssertion so the HTTP layer
			// can map this to a 500 with a distinguishable
			// envelope (not folded into the generic 5xx bucket)
			// and so future alerting / structured-log middleware
			// can errors.Is on the sentinel without parsing the
			// human-readable message.
			return fmt.Errorf("%w: refusing to run raw SQL with row_security=%q (must be 'on')", ErrSecurityAssertion, rowSecurity)
		}
		// Strict equality, not just non-empty.  dbutil.WithTenantTx
		// is the only call site that should set app.tenant_id
		// today and always does so to the same tenantID that was
		// passed in — but layering a value-match check here
		// catches the hypothetical future bug where a refactor
		// either:
		//
		//   (a) reuses a long-lived transaction across two
		//       different tenant calls without re-binding
		//       app.tenant_id, so the GUC carries a stale value
		//       from the previous tenant's request, or
		//
		//   (b) introduces a separate SetTenantContext call
		//       that takes a different uuid than the one passed
		//       to RunRawSQL (e.g. a caller-supplied override
		//       that should have been validated upstream).
		//
		// Both cases are mismatch-not-empty, so `tenantGUC == ""`
		// alone wouldn't catch them.  Compare against the exact
		// canonical string form (uuid.UUID.String() is RFC 4122
		// lowercase hyphenated) — dbutil.SetTenantContext passes
		// tenantID.String() through set_config, so the round-trip
		// is byte-equal in the happy path.
		if want := tenantID.String(); tenantGUC != want {
			if tenantGUC == "" {
				return fmt.Errorf("%w: refusing to run raw SQL with empty app.tenant_id GUC", ErrSecurityAssertion)
			}
			return fmt.Errorf("%w: refusing to run raw SQL with mismatched app.tenant_id (got %q, want %q)", ErrSecurityAssertion, tenantGUC, want)
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
