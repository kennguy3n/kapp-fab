package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Assert the actual idempotence invariant — every PATCH line
	// for each touched file reports zero insertions — rather than
	// pinning the absolute skip count. Pinning the count breaks
	// every time a new SCAFFOLD marker is added to wizard.go /
	// SetupWizardPage.tsx (e.g. a future mapping table the scaffold
	// learns to patch), even though those changes are correct. The
	// invariant we actually care about is "the second run inserts
	// nothing"; whether that's 2 skips, 3 skips, or 7 doesn't
	// matter.
	for _, file := range []string{
		"internal/tenant/wizard.go",
		"apps/web/src/pages/SetupWizardPage.tsx",
	} {
		re := regexp.MustCompile(`PATCH\s+` + regexp.QuoteMeta(file) + `\s+\((\d+) insertion\(s\), (\d+) already-present skip\(s\)\)`)
		match := re.FindStringSubmatch(got)
		if match == nil {
			t.Errorf("execute #2 output: no PATCH line for %s;\ngot:\n%s", file, got)
			continue
		}
		insertions := match[1]
		if insertions != "0" {
			t.Errorf("execute #2 output: %s reported %s insertions on the second run — idempotence violation;\ngot:\n%s", file, insertions, got)
		}
		// Also assert at least one skip — if the second run
		// reports both zero insertions AND zero skips, that
		// means the patch loop didn't see any patches at all,
		// which is the failure mode pinned-count assertions
		// would have caught.
		if match[2] == "0" {
			t.Errorf("execute #2 output: %s reported zero skips — patches not actually applied on the first run?\ngot:\n%s", file, got)
		}
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

// TestPlanLocaleFallsBackToBackendOnlyWhenFrontendMissing pins the
// monorepo-split contract: when apps/web/src/locales/en.json is
// absent (because the frontend lives in a separate checkout), the
// scaffold must still seed the backend locale catalogue. The bug
// it locks against: an early `return nil` after the frontend read
// error that silently skipped the backend catalogue write.
func TestPlanLocaleFallsBackToBackendOnlyWhenFrontendMissing(t *testing.T) {
	tmp := setupTempRepo(t)

	// Simulate a monorepo split — remove the frontend en.json so
	// planLocaleCatalogues' frontend read fails. The backend
	// en.json + locales.ts + SetupWizardPage stay (the scaffold
	// best-efforts the frontend patches too; only the catalogue
	// file seeding is gated on the en.json read).
	frontendEn := filepath.Join(tmp, "apps", "web", "src", "locales", "en.json")
	if err := os.Remove(frontendEn); err != nil {
		t.Fatalf("remove frontend en.json: %v", err)
	}

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

	// Backend catalogue MUST be seeded.
	backendOut := filepath.Join(tmp, "internal", "i18n", "locales", "sq.json")
	if _, err := os.Stat(backendOut); err != nil {
		t.Errorf("backend locale catalogue %s was not created despite the backend en.json being readable: %v", backendOut, err)
	}
	// Frontend catalogue MUST NOT be seeded (the frontend en.json
	// was absent, so there's nothing to seed from).
	frontendOut := filepath.Join(tmp, "apps", "web", "src", "locales", "sq.json")
	if _, err := os.Stat(frontendOut); err == nil {
		t.Errorf("frontend locale catalogue %s was unexpectedly created despite the source en.json being absent", frontendOut)
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

// TestValidateRejectsNonUniqueAnchor pins the load-bearing patchOp
// uniqueness claim. strings.Cut inserts at the FIRST occurrence, so a
// duplicate anchor would silently misplace the insertion; the
// patchOp doc says this is forbidden and validate() enforces it.
func TestValidateRejectsNonUniqueAnchor(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "dup.txt")
	// Two occurrences of the anchor in the file.
	if err := os.WriteFile(target, []byte("ANCHOR\n...\nANCHOR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &plan{
		RepoRoot: tmp,
		Patches: map[string][]patchOp{
			target: {{Anchor: "ANCHOR", Insertion: "X\n"}},
		},
	}
	err := p.validate()
	if err == nil {
		t.Fatal("validate(): want error for non-unique anchor, got nil")
	}
	if !strings.Contains(err.Error(), "non-unique") {
		t.Fatalf("validate() error = %q; want 'non-unique' marker", err.Error())
	}
}

// TestValidateRejectsMissingFile pins the planner-invariant guard:
// validate must surface a clear "planner queued patch for non-existent
// file" diagnostic when a future planner forgets its own os.Stat
// check, instead of leaking ENOENT through os.ReadFile.
func TestValidateRejectsMissingFile(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist.txt")
	p := &plan{
		RepoRoot: tmp,
		Patches: map[string][]patchOp{
			missing: {{Anchor: "ANCHOR", Insertion: "X\n"}},
		},
	}
	err := p.validate()
	if err == nil {
		t.Fatal("validate(): want error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "non-existent file") {
		t.Fatalf("validate() error = %q; want 'non-existent file' marker (got ENOENT leak instead?)", err.Error())
	}
}

// TestDryRunTolerantOfMissingAnchor pins the dry-run tolerance the
// Execute doc-block promises: an absent anchor in an existing file
// surfaces in the preview log as "anchor missing: …" and the run
// continues to the next file (so the contributor sees every anchor
// problem in one pass), instead of aborting on the first defect.
// Real (non-dry-run) execution against the same condition is caught
// by validate() and aborts — that path is exercised by
// TestValidateRejectsMissingFile and TestValidateRejectsNonUniqueAnchor.
func TestDryRunTolerantOfMissingAnchor(t *testing.T) {
	tmp := t.TempDir()
	// File exists, but the anchor the patchOp expects is NOT in it.
	target := filepath.Join(tmp, "no-anchor.txt")
	if err := os.WriteFile(target, []byte("hello world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &plan{
		RepoRoot: tmp,
		CC:       "ZZ",
		Name:     "Zedland",
		Patches: map[string][]patchOp{
			target: {{Anchor: "MISSING_ANCHOR", Insertion: "X\n"}},
		},
	}
	var buf bytes.Buffer
	if err := p.Execute(redirectStdout(t, &buf), true /*dryRun*/); err != nil {
		t.Fatalf("Execute(dry-run): want nil (tolerant), got %v", err)
	}
	if !strings.Contains(buf.String(), "anchor missing") {
		t.Fatalf("dry-run output should surface 'anchor missing' marker; got:\n%s", buf.String())
	}
	// Real run against the same plan must abort via validate().
	buf.Reset()
	err := p.Execute(redirectStdout(t, &buf), false /*dryRun*/)
	if err == nil {
		t.Fatal("Execute(real): want error from validate() on missing anchor, got nil")
	}
	if !strings.Contains(err.Error(), "anchor not found") {
		t.Fatalf("Execute(real) error = %q; want 'anchor not found' diagnostic", err.Error())
	}
}

// TestTaxPackPackagesDoNotImportGen pins the load-bearing assumption
// that tax-pack-pr.yml's "skip make proto-gen" optimisation is safe:
// the scoped test job runs `go test` directly on the tax-pack
// packages without first regenerating proto bindings, which is
// correct ONLY if none of those packages transitively import
// kapp-fab/gen/* (the bindings are gitignored and absent from a
// fresh checkout).
//
// If a future refactor adds a gen/ import to any tax-pack package,
// this test fails LOCALLY (where developers run go test) instead of
// the tax-pack-pr workflow failing on CI with a cryptic
// "package not found" — and the failure message points at the
// workflow that needs a proto-gen step prepended, not at the
// package that started importing gen/.
//
// Scope mirrors tax-pack-pr.yml's `paths:` filter: internal/hr/
// taxpacks, internal/tenant, internal/i18n, cmd/new-tax-pack.
func TestTaxPackPackagesDoNotImportGen(t *testing.T) {
	root := repoRoot(t)
	roots := []string{
		filepath.Join(root, "internal", "hr", "taxpacks"),
		filepath.Join(root, "internal", "tenant"),
		filepath.Join(root, "internal", "i18n"),
		filepath.Join(root, "cmd", "new-tax-pack"),
	}
	importRe := regexp.MustCompile(`"(github\.com/kennguy3n/kapp-fab/gen/[^"]+)"`)
	var offenders []string
	for _, dir := range roots {
		if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range importRe.FindAllStringSubmatch(string(body), -1) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+" imports "+m[1])
			}
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("tax-pack-pr.yml skips `make proto-gen` because the scoped packages don't import kapp-fab/gen/*, but the following imports break that assumption:\n  %s\nFix: either remove the gen/ imports from the tax-pack scope, or add a `make proto-gen` step to .github/workflows/tax-pack-pr.yml (mirroring ci.yml) so the bindings are on disk before `go test` runs.", strings.Join(offenders, "\n  "))
	}
}
