// Package review implements the B7 automated review pipeline. It
// runs a chain of Checks against a freshly-submitted extension
// version, persists findings keyed by (extension_version_id,
// check_name, code, location) so re-scans replace rather than
// duplicate, and transitions the review state row based on the
// resulting severities.
//
// The pipeline is intentionally pure-Go on a *Bundle: a
// Check sees the resolved bundle, returns []ReviewFinding, and is
// free of database or HTTP I/O. This keeps individual checks unit-
// testable without a Postgres fixture and lets the pipeline run
// the full chain even when one check finds errors (so the
// publisher sees the full picture, not just the first failure).
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// Check is the unit of work in the review pipeline. Implementations
// describe themselves via Name (must match the value persisted in
// marketplace_review_findings.check_name) and Run inspects the
// bundle.
//
// Run MUST return findings in deterministic order (the natural-key
// dedupe relies on this so a re-scan that produces identical
// findings is a true no-op). Run MUST NOT mutate the bundle.
type Check interface {
	Name() string
	Run(ctx context.Context, b *Bundle) []marketplace.ReviewFinding
}

// PolicyContext is the policy state the SignatureCheck and other
// publisher-aware checks need: the publisher row + its registered
// (non-revoked) keys. The pipeline fetches this once per version
// via the PolicyLoader and threads it through to each Check via
// the bundle's package-private hook (see WithPolicy below).
type PolicyContext struct {
	Publisher       *marketplace.Publisher
	NonRevokedKeys  []marketplace.PublisherKey
}

// PolicyLoader resolves the policy context for a version. The
// pipeline calls this exactly once per submission so a Check can
// read the publisher and key set without re-querying the store.
type PolicyLoader interface {
	LoadPolicy(ctx context.Context, version *marketplace.ExtensionVersion) (*PolicyContext, error)
}

// PolicyLoaderFunc adapts a function to PolicyLoader.
type PolicyLoaderFunc func(ctx context.Context, version *marketplace.ExtensionVersion) (*PolicyContext, error)

// LoadPolicy implements PolicyLoader.
func (f PolicyLoaderFunc) LoadPolicy(ctx context.Context, version *marketplace.ExtensionVersion) (*PolicyContext, error) {
	return f(ctx, version)
}

// FindingSink persists findings produced by a pipeline run. The
// canonical implementation is Store.UpsertReviewFindings; tests
// pass a fake.
type FindingSink interface {
	// UpsertReviewFindings replaces the finding set for the
	// version atomically: any prior finding whose natural key
	// (check_name, code, location) is NOT in the new set is
	// deleted; new findings overwrite by the same key. The single
	// transaction means a partial-failure rescan never leaves the
	// version with a mixed old+new finding state.
	UpsertReviewFindings(ctx context.Context, versionID uuid.UUID, findings []marketplace.ReviewFinding) error
}

// StateSink writes the resulting review state transition. The
// canonical impl is ReviewStateStore.UpdateReviewState. The
// pipeline never writes terminal states directly — a `rejected`
// transition flows through the same UpdateReviewState path the
// human reviewer uses, so the transition-graph check + audit
// trail capture apply uniformly.
type StateSink interface {
	UpdateReviewState(ctx context.Context, in marketplace.UpdateReviewStateInput) (*marketplace.ReviewState, error)
}

// Pipeline composes the inputs and outputs. The set of checks is
// passed in (not hardcoded) so tests can run a single check in
// isolation and the worker can register a versioned bundle in v2.
type Pipeline struct {
	Source   SourceFetcher
	Policy   PolicyLoader
	Findings FindingSink
	State    StateSink
	Checks   []Check
	Now      func() time.Time
}

// DefaultChecks is the v1 ordered list of checks. The order is
// stable so a finding's slice index is meaningful in logs.
//
// The list is package-level (not constructed inside Run) so a
// caller can introspect what will run and write a test that
// asserts the set hasn't drifted (an extension that ships without
// running SignatureCheck would silently bypass the security gate).
func DefaultChecks() []Check {
	return []Check{
		BundleSizeCheck{},
		ManifestSchemaCheck{},
		PermissionScopeCheck{},
		KTypeNamespaceCheck{},
		EndpointSchemeCheck{},
		IconCheck{},
		UIStaticAnalysisCheck{},
		SignatureCheck{},
	}
}

// Result is what Run returns. Tests inspect it directly; the
// worker calls Persist to drive both sinks atomically.
type Result struct {
	VersionID     uuid.UUID
	Findings      []marketplace.ReviewFinding
	WorstSeverity marketplace.Severity     // "" if no findings
	Status        marketplace.ReviewStatus // resolved review status
	Notes         string                   // human-readable summary

	// RanAt is the wall-clock time when the pipeline started
	// running the check chain. Stored in the automated_checks
	// JSONB column on Persist so the publisher UI can show
	// "last automated scan completed at" without joining the
	// findings table.
	RanAt time.Time

	// CheckResults is the per-check summary aggregated from
	// Findings + the set of checks that actually ran. Persisted
	// to marketplace_extension_review_state.automated_checks as
	// the canonical "this scan produced this verdict" record.
	// Findings stay in their own table for per-finding queries;
	// CheckResults is the rolled-up summary used by the
	// publisher dashboard.
	CheckResults []CheckResultSummary
}

// CheckResultSummary is the per-check rollup persisted into the
// automated_checks JSONB column. One row per check that ran in
// the pipeline, including checks that produced zero findings
// (so the publisher can see "signature: ran, 0 findings"
// distinct from "signature: didn't run, e.g. version pre-dated
// the check").
type CheckResultSummary struct {
	Name        string               `json:"name"`
	Passed      bool                 `json:"passed"`
	WorstLevel  marketplace.Severity `json:"worst,omitempty"`
	ErrorCount  int                  `json:"errors"`
	WarnCount   int                  `json:"warns"`
	InfoCount   int                  `json:"infos"`
}

// policyKey is the type used as the context value for PolicyContext.
// Using a package-local unexported type prevents collisions with
// any other ctx.Value usage outside this package.
type policyKey struct{}

// WithPolicy returns a context with the PolicyContext attached so a
// Check that needs the publisher / key set (currently only
// SignatureCheck) can pull it from ctx. We use ctx rather than a
// field on Check because: most checks don't need policy and we'd
// rather not pollute their interface; the policy is intrinsically
// per-run state.
func WithPolicy(ctx context.Context, p *PolicyContext) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, policyKey{}, p)
}

// PolicyFromContext extracts the PolicyContext attached by
// WithPolicy. Returns (nil, false) if no policy is attached — a
// Check that needs policy MUST handle the nil case (typically by
// emitting an info-severity finding so the publisher sees that
// the platform couldn't reach its own publisher table).
func PolicyFromContext(ctx context.Context) (*PolicyContext, bool) {
	v, ok := ctx.Value(policyKey{}).(*PolicyContext)
	return v, ok
}

// Run executes the full check chain against the version and
// returns the aggregated Result. The bundle is loaded once and
// reused across checks; checks are run sequentially (not in
// parallel) because (a) the per-check cost is small (in-memory
// inspection, no I/O), (b) sequential execution gives deterministic
// finding ordering, (c) a check that depends on a side-effect of
// an earlier check (none today, but plausibly in v2 — e.g. a
// "compiled UI bundle present" check that depends on the static
// analysis output) would otherwise race.
func (p *Pipeline) Run(ctx context.Context, version *marketplace.ExtensionVersion) (*Result, error) {
	if version == nil {
		return nil, errors.New("review: pipeline run with nil version")
	}
	if p.Source == nil {
		return nil, errors.New("review: pipeline has nil Source")
	}
	if len(p.Checks) == 0 {
		return nil, errors.New("review: pipeline has no checks")
	}
	rb, err := LoadReviewBundle(ctx, p.Source, version)
	if err != nil {
		// Bundle-shape failure → surface as a single unreviewable
		// finding. The state transitions to "rejected" with notes
		// pointing at the load error.
		now := p.nowFn()
		loadFinding := marketplace.ReviewFinding{
			ExtensionVersionID: version.ID,
			CheckName:          "bundle.load",
			Code:               "bundle.unloadable",
			Severity:           marketplace.SeverityError,
			Location:           "",
			Message:            fmt.Sprintf("could not load bundle for review: %v", err),
			CreatedAt:          now,
		}
		res := &Result{
			VersionID:     version.ID,
			Findings:      []marketplace.ReviewFinding{loadFinding},
			WorstSeverity: marketplace.SeverityError,
			Status:        marketplace.ReviewStatusRejected,
			Notes:         "bundle could not be loaded for review",
			RanAt:         now,
			CheckResults: []CheckResultSummary{{
				Name:       "bundle.load",
				Passed:     false,
				WorstLevel: marketplace.SeverityError,
				ErrorCount: 1,
			}},
		}
		return res, nil
	}
	var policy *PolicyContext
	if p.Policy != nil {
		policy, err = p.Policy.LoadPolicy(ctx, version)
		if err != nil {
			return nil, fmt.Errorf("review: load policy: %w", err)
		}
	}
	if policy != nil {
		ctx = WithPolicy(ctx, policy)
	}

	now := p.nowFn()
	var findings []marketplace.ReviewFinding
	for _, c := range p.Checks {
		out := c.Run(ctx, rb)
		// Normalise every finding produced by a check so a check
		// implementer cannot forget to set ExtensionVersionID /
		// CheckName / CreatedAt. This is load-bearing: the natural-
		// key UNIQUE on the table includes (version_id, check_name,
		// code, location) so a check that returns the right Code
		// but the wrong (or zero) VersionID would land in the
		// wrong row's finding set.
		for i := range out {
			out[i].ExtensionVersionID = version.ID
			if out[i].CheckName == "" {
				out[i].CheckName = c.Name()
			}
			if out[i].CreatedAt.IsZero() {
				out[i].CreatedAt = now
			}
			if !out[i].Severity.Valid() {
				out[i].Severity = marketplace.SeverityWarn
			}
		}
		findings = append(findings, out...)
	}

	// Stable sort findings for deterministic output. The natural-
	// key sort is (check_name, code, location) which matches the
	// UNIQUE index — so two consecutive runs produce identical
	// slice orderings even if the underlying check implementations
	// emit in different orders.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].CheckName != findings[j].CheckName {
			return findings[i].CheckName < findings[j].CheckName
		}
		if findings[i].Code != findings[j].Code {
			return findings[i].Code < findings[j].Code
		}
		return findings[i].Location < findings[j].Location
	})

	res := &Result{
		VersionID:    version.ID,
		Findings:     findings,
		RanAt:        now,
		CheckResults: summariseChecks(p.Checks, findings),
	}
	res.WorstSeverity, res.Status, res.Notes = computeStateTransition(findings, policy)
	return res, nil
}

// summariseChecks builds the per-check rollup that lives in the
// automated_checks JSONB column. Walks the ordered check list
// (NOT the findings) so a check that produced zero findings
// still appears in the summary as `passed=true`. Counts findings
// by severity per check_name so the publisher can see "4 warns
// in ui_static" at a glance without fetching the full findings
// list.
//
// Implementation note: we track per-check rows by *index into the
// `out` slice* rather than `*CheckResultSummary` pointers because
// `append` may reallocate the backing array — any pointer captured
// before the realloc points at the old (detached) array and writes
// through it would be silently dropped. Indices remain valid across
// reallocations.
func summariseChecks(checks []Check, findings []marketplace.ReviewFinding) []CheckResultSummary {
	by := make(map[string]int, len(checks))
	out := make([]CheckResultSummary, 0, len(checks))
	for _, c := range checks {
		name := c.Name()
		by[name] = len(out)
		out = append(out, CheckResultSummary{Name: name, Passed: true})
	}
	for i := range findings {
		f := &findings[i]
		idx, ok := by[f.CheckName]
		if !ok {
			// Synthetic finding from a non-check source (e.g.
			// bundle.load when the resolver fails). Surface as a
			// new summary row at the end so the publisher sees it.
			// Recording the index BEFORE append is safe because
			// append only invalidates pointers — `out[idx]` after
			// the append correctly addresses the new backing array.
			idx = len(out)
			by[f.CheckName] = idx
			out = append(out, CheckResultSummary{Name: f.CheckName, Passed: true})
		}
		s := &out[idx]
		switch f.Severity {
		case marketplace.SeverityError:
			s.ErrorCount++
			s.Passed = false
			s.WorstLevel = marketplace.SeverityError
		case marketplace.SeverityWarn:
			s.WarnCount++
			s.Passed = false
			if s.WorstLevel != marketplace.SeverityError {
				s.WorstLevel = marketplace.SeverityWarn
			}
		case marketplace.SeverityInfo:
			s.InfoCount++
			// Info-only does NOT flip Passed — info findings are
			// advisory by spec (e.g. "publisher unsigned but
			// has no keys yet").
			if s.WorstLevel == "" {
				s.WorstLevel = marketplace.SeverityInfo
			}
		}
	}
	return out
}

// Persist writes the result to both sinks. The two writes are NOT
// in a single tx (the FindingSink owns its own tx; the StateSink
// owns its own). Order matters: findings first, state second — so
// a state of "rejected" never points at an empty findings table
// (which would be confusing in the admin UI).
func (p *Pipeline) Persist(ctx context.Context, res *Result) error {
	if res == nil {
		return errors.New("review: persist nil result")
	}
	if p.Findings != nil {
		if err := p.Findings.UpsertReviewFindings(ctx, res.VersionID, res.Findings); err != nil {
			return fmt.Errorf("review: persist findings: %w", err)
		}
	}
	if p.State != nil {
		checksJSON, err := encodeAutomatedChecks(res)
		if err != nil {
			return fmt.Errorf("review: encode automated_checks: %w", err)
		}
		in := marketplace.UpdateReviewStateInput{
			VersionID:       res.VersionID,
			Status:          res.Status,
			ManualNotes:     res.Notes,
			AutomatedChecks: checksJSON,
		}
		// Auto-system-attribution for terminal transitions the
		// pipeline lands on its own (rejected when an error finding
		// fires). UpdateReviewState's CHECK requires a reviewer on
		// any terminal transition; we attribute to "system" rather
		// than leaving it blank so audits can distinguish a human
		// reject from a pipeline reject.
		if res.Status == marketplace.ReviewStatusRejected || res.Status == marketplace.ReviewStatusApproved {
			in.Reviewer = "system"
		}
		if _, err := p.State.UpdateReviewState(ctx, in); err != nil {
			return fmt.Errorf("review: persist state: %w", err)
		}
	}
	return nil
}

// encodeAutomatedChecks builds the JSONB payload for the
// automated_checks column. Schema is intentionally flat so
// downstream consumers (publisher UI, ops dashboards) can read
// `automated_checks.checks[*].name = 'signature'` without
// jq-grep gymnastics. The findings table remains the canonical
// per-finding store; this column is the rolled-up scan record
// (one JSONB per scan) for the publisher dashboard's
// "checks ran" pane.
func encodeAutomatedChecks(res *Result) ([]byte, error) {
	payload := struct {
		RanAt         time.Time            `json:"ran_at"`
		Status        marketplace.ReviewStatus `json:"status"`
		WorstSeverity marketplace.Severity `json:"worst,omitempty"`
		Checks        []CheckResultSummary `json:"checks"`
	}{
		RanAt:         res.RanAt,
		Status:        res.Status,
		WorstSeverity: res.WorstSeverity,
		Checks:        res.CheckResults,
	}
	return json.Marshal(payload)
}

// nowFn returns p.Now() with a time.Now fallback.
func (p *Pipeline) nowFn() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// computeStateTransition picks the resulting review status based
// on the worst-severity finding and the publisher's auto-approve-
// patch policy.
//
// Rules (mapped onto the ReviewStatus state graph in types.go):
//   - Any error severity finding → ReviewStatusRejected (terminal;
//     the publisher must fix and resubmit. The worker attributes
//     the rejection to "system" so the audit trail distinguishes
//     pipeline rejection from human rejection).
//   - Any warn severity finding (and no errors) →
//     ReviewStatusManualReview (the version reached the human-
//     review inbox after passing the structural automated checks
//     but flagging something that needs a human signature).
//   - No findings, no warnings → ReviewStatusAutomatedPassed
//     (the structural pass; B7.1 will gate the auto-approve-patch
//     fast-path off this state).
//   - Info-severity findings do not block: a publisher unsigned
//     finding (publisher has no keys yet) is info, not warn.
//
// Returns (worstSeverity, status, notes). worstSeverity is "" if
// no findings were produced.
func computeStateTransition(findings []marketplace.ReviewFinding, policy *PolicyContext) (marketplace.Severity, marketplace.ReviewStatus, string) {
	worst := marketplace.Severity("")
	var errCount, warnCount, infoCount int
	for i := range findings {
		f := &findings[i]
		switch f.Severity {
		case marketplace.SeverityError:
			errCount++
			worst = marketplace.SeverityError
		case marketplace.SeverityWarn:
			warnCount++
			if worst != marketplace.SeverityError {
				worst = marketplace.SeverityWarn
			}
		case marketplace.SeverityInfo:
			infoCount++
			if worst == "" {
				worst = marketplace.SeverityInfo
			}
		}
	}
	switch worst {
	case marketplace.SeverityError:
		return worst, marketplace.ReviewStatusRejected, fmt.Sprintf("%d error / %d warn / %d info finding(s); see findings for detail", errCount, warnCount, infoCount)
	case marketplace.SeverityWarn:
		return worst, marketplace.ReviewStatusManualReview, fmt.Sprintf("%d warn / %d info finding(s); human review required", warnCount, infoCount)
	default:
		// No findings, OR info-only.
		notes := "no findings; passed automated checks"
		if infoCount > 0 {
			notes = fmt.Sprintf("%d info-only finding(s); passed automated checks", infoCount)
		}
		if policy != nil && policy.Publisher != nil && policy.Publisher.AutoApprovePatch {
			// auto-approval at the worker level looks at whether
			// the version is a patch bump; the pipeline only
			// flags eligibility here. The worker is the only
			// caller that can compare to ext.ListedVersion.
			notes += " (auto-approve-patch eligible if patch bump)"
		}
		return worst, marketplace.ReviewStatusAutomatedPassed, notes
	}
}

// Once is a small helper for sync.Once+error coordination, used by
// the worker to guarantee leader-singleton semantics across replica
// restarts. Kept here rather than in worker.go so a unit test can
// exercise it in isolation.
type Once struct {
	mu   sync.Mutex
	done bool
	err  error
}

// Do runs fn at most once. Returns the result of the first call;
// subsequent calls return the cached error.
func (o *Once) Do(fn func() error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.err
	}
	o.done = true
	o.err = fn()
	return o.err
}
