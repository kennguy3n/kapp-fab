package review

import (
	"context"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// summariseCheckStub is a Check that only needs Name() for
// summariseChecks (Run is never invoked from the helper). The
// package-internal test declares its own no-op Check so the
// helper can be exercised in isolation.
type summariseCheckStub struct{ name string }

func (c summariseCheckStub) Name() string { return c.name }
func (summariseCheckStub) Run(_ context.Context, _ *Bundle) []marketplace.ReviewFinding {
	return nil
}

// TestSummariseChecks_PointerSafetyOnRealloc pins BUG_0002:
// summariseChecks must not lose count updates when its backing
// slice is reallocated by an append in the synthetic-finding branch.
//
// Scenario: initial capacity == len(checks); the first finding for
// a name NOT in `checks` triggers `append(out, ...)` which
// reallocates (len == cap). Subsequent findings against earlier
// check names must still update the correct row. With pointer-based
// tracking the second batch of increments would be silently dropped
// (they would write into the detached old backing array). With
// index-based tracking they land correctly.
func TestSummariseChecks_PointerSafetyOnRealloc(t *testing.T) {
	t.Parallel()
	checks := []Check{
		summariseCheckStub{name: "alpha"},
		summariseCheckStub{name: "beta"},
	}
	findings := []marketplace.ReviewFinding{
		// First, a synthetic name (forces realloc inside summariseChecks).
		{CheckName: "synth.one", Code: "synth.one.x", Severity: marketplace.SeverityError},
		// Then findings against the original check names that should
		// still land in the correct rows.
		{CheckName: "alpha", Code: "alpha.x", Severity: marketplace.SeverityError},
		{CheckName: "alpha", Code: "alpha.y", Severity: marketplace.SeverityWarn},
		{CheckName: "beta", Code: "beta.x", Severity: marketplace.SeverityWarn},
		{CheckName: "beta", Code: "beta.y", Severity: marketplace.SeverityInfo},
		// Another synthetic (potential second realloc).
		{CheckName: "synth.two", Code: "synth.two.x", Severity: marketplace.SeverityInfo},
		// One more original-check finding to verify rows remained
		// accessible after the second realloc.
		{CheckName: "alpha", Code: "alpha.z", Severity: marketplace.SeverityWarn},
	}
	out := summariseChecks(checks, findings)

	by := map[string]CheckResultSummary{}
	for _, s := range out {
		by[s.Name] = s
	}

	a, ok := by["alpha"]
	if !ok {
		t.Fatalf("missing alpha summary; got %+v", out)
	}
	if a.ErrorCount != 1 || a.WarnCount != 2 {
		t.Errorf("alpha counts wrong: errors=%d warns=%d (want 1, 2). out=%+v", a.ErrorCount, a.WarnCount, out)
	}
	if a.Passed {
		t.Errorf("alpha should be !Passed after error/warn findings")
	}
	if a.WorstLevel != marketplace.SeverityError {
		t.Errorf("alpha WorstLevel=%q want %q", a.WorstLevel, marketplace.SeverityError)
	}

	b, ok := by["beta"]
	if !ok {
		t.Fatalf("missing beta summary; got %+v", out)
	}
	if b.WarnCount != 1 || b.InfoCount != 1 {
		t.Errorf("beta counts wrong: warns=%d infos=%d (want 1, 1). out=%+v", b.WarnCount, b.InfoCount, out)
	}
	if b.Passed {
		t.Errorf("beta should be !Passed after a warn finding")
	}

	s1, ok := by["synth.one"]
	if !ok {
		t.Fatalf("missing synth.one summary; got %+v", out)
	}
	if s1.ErrorCount != 1 {
		t.Errorf("synth.one ErrorCount=%d want 1", s1.ErrorCount)
	}

	s2, ok := by["synth.two"]
	if !ok {
		t.Fatalf("missing synth.two summary; got %+v", out)
	}
	if s2.InfoCount != 1 {
		t.Errorf("synth.two InfoCount=%d want 1", s2.InfoCount)
	}
}
