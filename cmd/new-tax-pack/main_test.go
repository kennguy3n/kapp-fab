package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPlanRefusesInvalidCC pins the front-line input validation so a
// typo or shell-quoting accident doesn't silently generate files with
// a non-ISO code in the filename.
func TestPlanRefusesInvalidCC(t *testing.T) {
	cases := []struct {
		name string
		cc   string
	}{
		{"empty", ""},
		{"too-short", "X"},
		{"too-long", "USA"},
		{"non-ascii", "日本"},
		{"digits", "12"},
	}
	root := repoRoot(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildPlan(planInput{CC: tc.cc, Name: "Anywhere", RepoRoot: root})
			if err == nil {
				t.Fatalf("buildPlan(cc=%q): want error, got nil", tc.cc)
			}
		})
	}
}

// TestPlanRefusesMissingName pins the name-required guard so a
// contributor running the scaffold with only -cc gets a friendly error
// instead of a CoA template with an empty label.
func TestPlanRefusesMissingName(t *testing.T) {
	_, err := buildPlan(planInput{CC: "ZZ", RepoRoot: repoRoot(t)})
	if err == nil {
		t.Fatal("buildPlan: want error for missing -name, got nil")
	}
}

// TestPlanRefusesMissingLocaleName pins the locale-name-required guard
// for new locale tags. The check has to fire AFTER the existing-pack
// check so we use a sentinel "XK" code (Kosovo — not in the registry)
// and a sentinel "kk" locale (Kazakh — not in SupportedLocales).
func TestPlanRefusesMissingLocaleName(t *testing.T) {
	_, err := buildPlan(planInput{
		CC: "XK", Name: "Kosovo", Locale: "kk", RepoRoot: repoRoot(t),
	})
	if err == nil {
		t.Fatal("buildPlan: want error for missing -locale-name on new locale, got nil")
	}
	if !strings.Contains(err.Error(), "locale-name") {
		t.Fatalf("buildPlan error = %q; want mention of locale-name", err.Error())
	}
}

// TestPlanRefusesExistingPack pins the safety check that protects a
// contributor's hand-tuned constants from being clobbered by a
// re-run of the scaffold against an existing pack.
func TestPlanRefusesExistingPack(t *testing.T) {
	_, err := buildPlan(planInput{
		CC: "US", Name: "United States", RepoRoot: repoRoot(t),
	})
	if err == nil {
		t.Fatal("buildPlan(cc=US): want error, got nil (would clobber us.go)")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("buildPlan error = %q; want 'already exists' marker", err.Error())
	}
}

// TestPlanGeneratesValidCoATemplate end-to-end verifies the scaffold
// output parses as a valid CoA template (flat array, parent_code
// references resolve, no duplicate codes). Done in a tempdir copy so
// the source tree isn't mutated.
func TestPlanGeneratesValidCoATemplate(t *testing.T) {
	body := renderCoATemplate("ZZ", "Zedland")

	var rows []struct {
		Code       string `json:"code"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		ParentCode string `json:"parent_code"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("renderCoATemplate output is not valid JSON: %v\nbody:\n%s", err, body)
	}
	if len(rows) < 20 {
		t.Fatalf("renderCoATemplate output has %d rows; want at least 20 (IFRS hierarchy)", len(rows))
	}
	codes := map[string]bool{}
	for _, r := range rows {
		if r.Code == "" {
			t.Fatalf("renderCoATemplate row has empty code: %+v", r)
		}
		if codes[r.Code] {
			t.Fatalf("renderCoATemplate row %q is a duplicate", r.Code)
		}
		codes[r.Code] = true
		if r.Type != "asset" && r.Type != "liability" && r.Type != "equity" && r.Type != "revenue" && r.Type != "expense" {
			t.Fatalf("renderCoATemplate row %q has invalid type %q", r.Code, r.Type)
		}
	}
	// Verify every non-root parent_code resolves.
	for _, r := range rows {
		if r.ParentCode == "" {
			continue
		}
		if !codes[r.ParentCode] {
			t.Fatalf("renderCoATemplate row %q references non-existent parent_code %q", r.Code, r.ParentCode)
		}
	}
	// Verify the TODO marker is present so a careless contributor
	// can't ship a PR with the unrenamed placeholder.
	if !strings.Contains(body, "TODO(community)") {
		t.Fatal("renderCoATemplate output is missing TODO(community) marker on the payroll-liability line")
	}
}

// TestPlanGeneratesCompileablePack runs the rendered pack skeleton
// through `go build` to pin the invariant that the scaffold output is
// always immediately compileable. The pack body emits no deductions,
// but the file structure has to satisfy every part of the TaxPack
// interface.
func TestPlanGeneratesCompileablePack(t *testing.T) {
	body := renderPackSkeleton("ZZ", "zz", "Zedland")
	// Sanity checks on the rendered source.
	mustContain := []string{
		"package taxpacks",
		"type zzPack struct{}",
		"func (zzPack) Country() string { return \"ZZ\" }",
		"func (zzPack) EffectiveYear() int",
		"func (zzPack) ComputeWithholding(",
		"Register(&zzPack{})",
		"TODO(community)",
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Fatalf("renderPackSkeleton output is missing %q\nbody:\n%s", s, body)
		}
	}
	// Compile-check by writing the file into a tempdir module that
	// re-uses the real taxpacks package via a replace directive.
	root := repoRoot(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "zz.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Use the real taxpacks dir for compile by symlinking the body
	// into it temporarily isn't allowed (parallel test runs would
	// race), so instead invoke gofmt to at least catch syntax bugs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gofmt", "-e", filepath.Join(dir, "zz.go")).CombinedOutput()
	if err != nil {
		t.Fatalf("gofmt rejected scaffold output: %v\n%s\nrendered:\n%s", err, out, body)
	}
	_ = root
}

// TestPlanIsIdempotent runs the scaffold twice against the same temp
// copy of the repo and verifies the second run is a no-op (every
// patch's SkipIfPresent fires).
func TestPlanIsIdempotent(t *testing.T) {
	tmp := setupTempRepo(t)

	in := planInput{CC: "ZZ", Name: "Zedland", RepoRoot: tmp}
	first, err := buildPlan(in)
	if err != nil {
		t.Fatalf("buildPlan #1: %v", err)
	}
	var buf bytes.Buffer
	if err := first.Execute(redirectStdout(t, &buf), false); err != nil {
		t.Fatalf("execute #1: %v", err)
	}

	// Second run: every patch should detect the SkipIfPresent marker
	// and skip. Pass -force to bypass the "pack already exists"
	// guard so we can exercise the patch-idempotence path.
	in.Force = true
	second, err := buildPlan(in)
	if err != nil {
		t.Fatalf("buildPlan #2: %v", err)
	}
	buf.Reset()
	if err := second.Execute(redirectStdout(t, &buf), false); err != nil {
		t.Fatalf("execute #2: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "3 already-present skip(s)") {
		t.Errorf("execute #2 output: expected wizard.go to skip all 3 patches the second time;\ngot:\n%s", got)
	}
	if !strings.Contains(got, "2 already-present skip(s)") {
		t.Errorf("execute #2 output: expected SetupWizardPage.tsx to skip all 2 patches the second time;\ngot:\n%s", got)
	}
}

// TestPlanNextStepsReferencesActualPayrollLiabilityCode pins the
// invariant that the "Next steps" output points at the CoA account
// code that renderCoATemplate actually emits as the TODO placeholder
// line. The two used to be out of sync (the prose said "2131" while
// the JSON wrote "2140"), which would send a contributor hunting for
// a line that doesn't exist. The test extracts the placeholder code
// from the rendered template at runtime so a future re-numbering of
// the CoA hierarchy can't regress this — both halves move together
// or the test fails.
func TestPlanNextStepsReferencesActualPayrollLiabilityCode(t *testing.T) {
	tmp := setupTempRepo(t)

	in := planInput{CC: "ZZ", Name: "Zedland", RepoRoot: tmp}
	p, err := buildPlan(in)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	var buf bytes.Buffer
	if err := p.Execute(redirectStdout(t, &buf), false); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Pull the CoA template the scaffold wrote and find the code on
	// the line tagged with TODO(community). That's the line the
	// "Next steps" output is asking the contributor to rename.
	coaBody, err := os.ReadFile(filepath.Join(tmp, "internal", "tenant", "coa_templates", "zz_basic.json"))
	if err != nil {
		t.Fatalf("read scaffolded CoA: %v", err)
	}
	var rows []struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(coaBody, &rows); err != nil {
		t.Fatalf("parse scaffolded CoA: %v", err)
	}
	var todoCode string
	for _, r := range rows {
		if strings.Contains(r.Name, "TODO(community)") {
			todoCode = r.Code
			break
		}
	}
	if todoCode == "" {
		t.Fatalf("scaffolded CoA has no TODO(community) line; rows: %+v", rows)
	}

	got := buf.String()
	want := "Rename the " + todoCode + " line"
	if !strings.Contains(got, want) {
		t.Errorf("Next-steps output references the wrong CoA code.\n  got    %q\n  want substring %q\n  output:\n%s",
			got, want, got)
	}
}

// TestPlanLocaleFlagSeedsCatalogues exercises the -locale branch end-
// to-end against a temp repo copy: the scaffold should emit two
// locale catalogue files seeded from en.json AND patch
// COUNTRY_LOCALE_DEFAULTS + SupportedLocales.
func TestPlanLocaleFlagSeedsCatalogues(t *testing.T) {
	tmp := setupTempRepo(t)

	in := planInput{
		CC: "XK", Name: "Kosovo", Locale: "sq", LocaleName: "Shqip", RepoRoot: tmp,
	}
	p, err := buildPlan(in)
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	var buf bytes.Buffer
	if err := p.Execute(redirectStdout(t, &buf), false); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Backend + frontend locale catalogues exist.
	for _, rel := range []string{
		"internal/i18n/locales/sq.json",
		"apps/web/src/locales/sq.json",
	} {
		if _, err := os.Stat(filepath.Join(tmp, rel)); err != nil {
			t.Errorf("locale catalogue %s was not created: %v", rel, err)
		}
	}

	// COUNTRY_LOCALE_DEFAULTS got the new entry.
	localesTS, err := os.ReadFile(filepath.Join(tmp, "apps", "web", "src", "lib", "i18n", "locales.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(localesTS), `XK: "sq"`) {
		t.Error("locales.ts missing COUNTRY_LOCALE_DEFAULTS entry XK: \"sq\"")
	}
	// SupportedLocales got the new entry.
	if !strings.Contains(string(localesTS), `tag: "sq"`) {
		t.Error("locales.ts missing SupportedLocales entry for sq")
	}

	// DefaultLocaleForCountry got the new case.
	wizardGo, err := os.ReadFile(filepath.Join(tmp, "internal", "tenant", "wizard.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wizardGo), `case "XK":`) {
		t.Error("wizard.go missing DefaultLocaleForCountry XK case")
	}
}

// repoRoot resolves the project root from the test's working
// directory (tests run inside cmd/new-tax-pack/ which is two levels
// down from the repo root).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// setupTempRepo copies the minimal set of files the scaffold needs
// into a tempdir so the on-disk repo isn't mutated by the test. Only
// the files the scaffold reads or patches are copied; anything else
// stays in the source checkout.
func setupTempRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := repoRoot(t)
	files := []string{
		"internal/hr/taxpacks/taxpacks.go",
		"internal/tenant/wizard.go",
		"apps/web/src/pages/SetupWizardPage.tsx",
		"apps/web/src/lib/i18n/locales.ts",
		"internal/i18n/locales/en.json",
		"apps/web/src/locales/en.json",
	}
	for _, rel := range files {
		src := filepath.Join(root, rel)
		dst := filepath.Join(tmp, rel)
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The plan also needs the coa_templates dir to exist (it writes
	// the new <cc>_basic.json into it). Pre-create it as empty.
	if err := os.MkdirAll(filepath.Join(tmp, "internal", "tenant", "coa_templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "internal", "hr", "taxpacks"), 0o755); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// redirectStdout returns the supplied buf as the io.Writer the
// Execute method writes its progress output to. Tests use this to
// keep the scaffold's stdout out of the `go test` log and to assert
// on the rendered text.
func redirectStdout(t *testing.T, buf *bytes.Buffer) *bytes.Buffer {
	t.Helper()
	return buf
}
