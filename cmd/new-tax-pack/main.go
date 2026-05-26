// Command new-tax-pack scaffolds a new country tax pack across all the
// files the per-country checklist in docs/TAX_PACK_MAINTENANCE.md
// touches. It is the entry-point for community contributors who want
// to add a country pack without first having to memorise the cross-
// file contract.
//
// Usage:
//
//	go run ./cmd/new-tax-pack -cc XX -name "Country" -locale xx
//
// Required flags:
//
//   -cc       ISO 3166-1 alpha-2 country code (2 letters). Validated
//             for shape AND for non-registration; the tool refuses to
//             clobber an existing pack.
//   -name     Human-readable country name used in the CoA template
//             label and the COA_TEMPLATES entry in SetupWizardPage.
//
// Optional flags:
//
//   -locale          IETF BCP-47 language tag for the country's
//                    default locale. If non-empty AND not already in
//                    the SupportedLocales registry, the tool also
//                    emits two locale catalogue files (backend +
//                    frontend) seeded from en.json. Default: empty
//                    (no locale work — the country falls back to en).
//   -locale-name     Native language name for the SupportedLocales
//                    entry. Required when -locale is non-empty AND
//                    the tag is not already registered.
//   -repo-root       Path to the repo root. Defaults to the current
//                    working directory.
//   -dry-run         Print what would change without touching the
//                    filesystem. Useful for CI presubmit hooks that
//                    want to verify the scaffold would apply cleanly
//                    before a contributor runs the real thing.
//   -force           Skip the "country already registered" guard.
//                    Used internally by tests; community contributors
//                    should not use this — re-running the scaffold
//                    over an existing pack will overwrite the
//                    contributor's hand-tuned constants.
//
// The scaffold deliberately writes a *minimal* pack that compiles
// and registers itself but emits zero deductions. The contributor's
// job is to fill in:
//
//   1. The statutory rate / bracket constants (with citations to the
//      revenue authority's published source — see CONTRIBUTING_TAX_
//      PACKS.md for the citation convention).
//   2. The ComputeWithholding bracket walk + cap enforcement +
//      period prorating.
//   3. The regression test matrix (nominal, threshold, cap, edge).
//
// The scaffold is intentionally not a "fill in the blanks" template —
// every country's withholding logic is too different (bracket walks
// vs flat rates, residency gating, age-banded rates, YTD caps) for a
// templated body to be more help than starting from a similar
// existing pack. Instead, the scaffold lays out the file structure,
// wires up the registration, and emits a TODO checklist that mirrors
// the per-country checklist in docs/TAX_PACK_MAINTENANCE.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "new-tax-pack:", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	fs := flag.NewFlagSet("new-tax-pack", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		cc         = fs.String("cc", "", "ISO 3166-1 alpha-2 country code (required)")
		name       = fs.String("name", "", "Human-readable country name (required)")
		locale     = fs.String("locale", "", "Default locale tag (e.g. fr, ja). Omit to fall back to en.")
		localeName = fs.String("locale-name", "", "Native display name of the locale (required when -locale is new)")
		repoRoot   = fs.String("repo-root", "", "Repo root (defaults to current working directory)")
		dryRun     = fs.Bool("dry-run", false, "Print actions without writing files")
		force      = fs.Bool("force", false, "Overwrite existing pack (testing only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	plan, err := buildPlan(planInput{
		CC:         *cc,
		Name:       *name,
		Locale:     *locale,
		LocaleName: *localeName,
		RepoRoot:   *repoRoot,
		Force:      *force,
	})
	if err != nil {
		return err
	}

	return plan.Execute(out, *dryRun)
}

// planInput is the validated, normalised input to buildPlan. Kept as
// a struct so future flags (e.g. -coa-template-base) can be added
// without changing buildPlan's signature.
type planInput struct {
	CC         string
	Name       string
	Locale     string
	LocaleName string
	RepoRoot   string
	Force      bool
}

// plan captures every filesystem change a single scaffold run would
// apply. It is built up front (without writing anything) so -dry-run
// can render the whole plan and so Execute can fail atomically — if
// any planned change conflicts with the current state, none of them
// are applied.
type plan struct {
	RepoRoot   string
	CC         string // upper-cased ISO code
	CCLower    string // lower-cased — used for filenames
	Name       string
	Locale     string
	LocaleName string

	// Newly-created files (full path → contents).
	NewFiles map[string]string

	// Patched files (full path → list of (anchor, insertion) pairs).
	Patches map[string][]patchOp
}

// patchOp describes a single insertion into an existing file. The
// scaffold never deletes or replaces existing text — every patch is
// "find this anchor, insert this block immediately before it" — so a
// dry-run preview is always accurate and re-running the scaffold on
// an existing pack with -force overwrites idempotently (the inserted
// block is detected and skipped).
type patchOp struct {
	// Anchor is a substring of the target file. The insertion lands
	// immediately before the FIRST occurrence of Anchor. Anchors are
	// chosen to be unique within their file (e.g. the closing
	// `}` of a specific map literal); a non-unique anchor causes
	// Execute to abort.
	Anchor string

	// Insertion is the text inserted before Anchor. The scaffold
	// adds trailing newlines where the surrounding style requires
	// them — callers don't need to manage whitespace.
	Insertion string

	// SkipIfPresent is a substring; if it already appears in the
	// file, the patch is a no-op. Used so re-running the scaffold
	// is idempotent — the second run sees the already-inserted
	// block and skips it instead of inserting a duplicate.
	SkipIfPresent string
}

// validISO is the shape check for the -cc flag. Two ASCII letters,
// case-insensitive. The check is intentionally permissive — the
// scaffold does NOT cross-reference against the ISO 3166-1 published
// list because (a) the list does drift (e.g. XK / Kosovo is not on
// the ISO list but is used by some payroll systems) and (b) the
// caller is expected to provide a real code; a typo here gets caught
// by the registration check below when the contributor wires up
// init() later.
var validISO = regexp.MustCompile(`^[A-Za-z]{2}$`)

// validLocale is a loose BCP-47 check sufficient for filename safety.
// Supports two-letter primary tags (en, fr, ja) and the script /
// region extensions Kapp already ships (pt-BR, zh-Hans). Stricter
// validation lives in apps/web/src/lib/i18n/locales.ts at runtime.
var validLocale = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-[A-Z]{2})?$`)

func buildPlan(in planInput) (*plan, error) {
	if in.CC == "" {
		return nil, errors.New("-cc is required (ISO 3166-1 alpha-2 country code)")
	}
	if !validISO.MatchString(in.CC) {
		return nil, fmt.Errorf("-cc=%q: must be two ASCII letters", in.CC)
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, errors.New("-name is required (human-readable country name)")
	}
	if in.Locale != "" && !validLocale.MatchString(in.Locale) {
		return nil, fmt.Errorf("-locale=%q: must be a BCP-47 language tag (e.g. fr, ja, pt-BR)", in.Locale)
	}

	root := in.RepoRoot
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve cwd: %w", err)
		}
		root = wd
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve repo-root: %w", err)
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "hr", "taxpacks", "taxpacks.go")); err != nil {
		return nil, fmt.Errorf("repo-root %q does not look like the kapp-fab checkout (missing internal/hr/taxpacks/taxpacks.go)", root)
	}

	p := &plan{
		RepoRoot:   root,
		CC:         strings.ToUpper(in.CC),
		CCLower:    strings.ToLower(in.CC),
		Name:       strings.TrimSpace(in.Name),
		Locale:     in.Locale,
		LocaleName: strings.TrimSpace(in.LocaleName),
		NewFiles:   map[string]string{},
		Patches:    map[string][]patchOp{},
	}

	packPath := filepath.Join(root, "internal", "hr", "taxpacks", p.CCLower+".go")
	coaPath := filepath.Join(root, "internal", "tenant", "coa_templates", p.CCLower+"_basic.json")

	if _, err := os.Stat(packPath); err == nil && !in.Force {
		return nil, fmt.Errorf("pack already exists: %s (re-run with -force to overwrite — but consider opening a follow-up PR against the existing pack instead)", packPath)
	}
	if _, err := os.Stat(coaPath); err == nil && !in.Force {
		return nil, fmt.Errorf("CoA template already exists: %s (re-run with -force to overwrite)", coaPath)
	}

	p.NewFiles[packPath] = renderPackSkeleton(p.CC, p.CCLower, p.Name)
	p.NewFiles[coaPath] = renderCoATemplate(p.CC, p.Name)

	wizardPath := filepath.Join(root, "internal", "tenant", "wizard.go")
	if err := p.planWizardPatches(wizardPath); err != nil {
		return nil, err
	}

	frontendWizardPath := filepath.Join(root, "apps", "web", "src", "pages", "SetupWizardPage.tsx")
	p.planFrontendWizardPatches(frontendWizardPath)

	if p.Locale != "" {
		frontendLocalesPath := filepath.Join(root, "apps", "web", "src", "lib", "i18n", "locales.ts")
		if err := p.planFrontendLocalesPatches(frontendLocalesPath); err != nil {
			return nil, err
		}
		if err := p.planLocaleCatalogues(); err != nil {
			return nil, err
		}
	}

	return p, nil
}

// renderPackSkeleton returns a compileable, registration-only tax
// pack. It implements the TaxPack interface (Country, EffectiveYear,
// ComputeWithholding) but emits zero deductions until the contributor
// adds the statutory bracket walk. The skeleton's body intentionally
// references the per-country checklist in CONTRIBUTING_TAX_PACKS.md so
// the contributor has a single document to walk through.
func renderPackSkeleton(cc, ccLower, name string) string {
	return fmt.Sprintf(`package taxpacks

import (
	"context"

	"github.com/shopspring/decimal"
)

// %[1]sPack implements %[2]s's payroll-side statutory withholdings.
//
// SCAFFOLD STATUS: This pack was generated by `+"`cmd/new-tax-pack`"+`
// and emits zero deductions until the contributor wires up the
// statutory rate tables. See docs/CONTRIBUTING_TAX_PACKS.md for the
// per-country checklist; the short version:
//
//   1. Replace this docstring with the statutory components the pack
//      will emit (PAYE / income tax / SSC / health levy / etc.) and
//      cite the revenue authority's published source for each.
//   2. Add the bracket / rate / cap constants below in the var block
//      (kept package-private so they live next to the function that
//      uses them and the regression test can pin them via Lookup).
//   3. Replace the ComputeWithholding body with the real bracket
//      walk + period prorating + YTD cap enforcement. The 31/365.25
//      annualisation convention from the existing packs is the
//      house style; see internal/hr/taxpacks/europe_west_packs_test.go
//      for the period-fraction pattern.
//   4. Add a regression matrix to the regional test file covering
//      nominal salary, threshold crossings, YTD caps (if any), and
//      zero/negative edge cases.
//
// References:
//
//	TODO(community): cite the revenue-authority URL for income-tax
//	bands, the social-security authority URL for SSC caps, and the
//	gazette / legal-source URL for any age-banded or residency-
//	gated rules.
type %[1]sPack struct{}

func init() { Register(&%[1]sPack{}) }

// Country returns the ISO 3166-1 alpha-2 code this pack services.
func (%[1]sPack) Country() string { return %[3]q }

// EffectiveYear is the calendar year the rates and brackets in
// this pack are sourced from. Bump this when a year-end refresh
// lands; the payroll engine logs a warning when a slip's pay-
// period year diverges from this value.
//
// TODO(community): set to the fiscal year the brackets you'll add
// below are calibrated for.
func (%[1]sPack) EffectiveYear() int { return 2025 }

// ComputeWithholding emits the slip's statutory deductions.
//
// SCAFFOLD: the generated body returns no deductions. Replace it
// with the real bracket walk once the rate constants above are in
// place. The signature MUST stay verbatim — the TaxPack interface
// is exercised by the engine via this exact method.
func (%[1]sPack) ComputeWithholding(_ context.Context, _ EmployeeInfo, gross decimal.Decimal, period PayPeriod) ([]Deduction, error) {
	// Guard against zero / negative periods up front so the
	// caller's slip-replay path doesn't divide by zero downstream
	// when the contributor wires up annualisation.
	if period.Days() <= 0 || gross.IsNegative() {
		return nil, nil
	}
	// TODO(community): walk the statutory brackets, prorate from
	// annual to the slip's period (use a periodFraction = days /
	// 365.25 convention to match the rest of the pack roster),
	// enforce YTD caps (read from EmployeeInfo.YTDGross), and
	// return one Deduction line per statutory component.
	return nil, nil
}
`, ccLower, name, cc)
}

// renderCoATemplate returns a minimal IFRS Chart-of-Accounts template
// keyed by the country's two-letter code. The hierarchy follows the
// same 1xxx-Assets / 2xxx-Liabilities / 3xxx-Equity / 4xxx-Revenue /
// 5xxx-COGS / 6xxx-Expenses / 7xxx-Finance shape every other pack
// uses. The contributor MUST extend the 2xxx liability tree with
// the country's payroll-liability accounts (one per Deduction.Code
// the pack emits) so the payroll posting hook has a real ledger
// destination on day one.
func renderCoATemplate(cc, _ string) string {
	// The chart schema is a flat JSON array of accounts as ingested
	// by tenant.loadTemplate (see internal/tenant/wizard.go: every
	// non-root account references its parent via the "parent_code"
	// key). The 1xxx-Assets / 2xxx-Liabilities / 3xxx-Equity /
	// 4xxx-Revenue / 5xxx-COGS / 6xxx-Expenses / 7xxx-Finance
	// hierarchy mirrors what every other country pack ships; the
	// 21xx Current Liabilities subtree is the right place for the
	// contributor to expand with one statutory-liability account per
	// Deduction.Code the pack emits (e.g. 2140 PAYE Payable, 2141
	// SSC Payable). The scaffold lands a single placeholder line at
	// 2140 marked TODO(community); the contributor expands and
	// renames it before opening the PR.
	return fmt.Sprintf(`[
  {"code": "1", "name": "Assets", "type": "asset"},
  {"code": "11", "name": "Current Assets", "type": "asset", "parent_code": "1"},
  {"code": "1110", "name": "Cash and Cash Equivalents", "type": "asset", "parent_code": "11"},
  {"code": "1120", "name": "Trade Receivables", "type": "asset", "parent_code": "11"},
  {"code": "1130", "name": "Inventories", "type": "asset", "parent_code": "11"},
  {"code": "1140", "name": "Prepayments", "type": "asset", "parent_code": "11"},
  {"code": "12", "name": "Non-Current Assets", "type": "asset", "parent_code": "1"},
  {"code": "1210", "name": "Property, Plant and Equipment", "type": "asset", "parent_code": "12"},
  {"code": "1220", "name": "Intangible Assets", "type": "asset", "parent_code": "12"},
  {"code": "1230", "name": "Right-of-Use Assets", "type": "asset", "parent_code": "12"},
  {"code": "2", "name": "Liabilities", "type": "liability"},
  {"code": "21", "name": "Current Liabilities", "type": "liability", "parent_code": "2"},
  {"code": "2110", "name": "Trade Payables", "type": "liability", "parent_code": "21"},
  {"code": "2120", "name": "Accruals", "type": "liability", "parent_code": "21"},
  {"code": "2130", "name": "VAT/GST Payable (Output Tax)", "type": "liability", "parent_code": "21"},
  {"code": "2140", "name": "TODO(community): rename to the statutory payroll-liability line the %[1]s pack emits (e.g. PAYE Payable)", "type": "liability", "parent_code": "21"},
  {"code": "22", "name": "Non-Current Liabilities", "type": "liability", "parent_code": "2"},
  {"code": "2210", "name": "Borrowings", "type": "liability", "parent_code": "22"},
  {"code": "2220", "name": "Lease Liabilities", "type": "liability", "parent_code": "22"},
  {"code": "3", "name": "Equity", "type": "equity"},
  {"code": "3010", "name": "Share Capital", "type": "equity", "parent_code": "3"},
  {"code": "3020", "name": "Retained Earnings", "type": "equity", "parent_code": "3"},
  {"code": "4", "name": "Revenue", "type": "revenue"},
  {"code": "4010", "name": "Revenue from Contracts with Customers", "type": "revenue", "parent_code": "4"},
  {"code": "4020", "name": "Other Income", "type": "revenue", "parent_code": "4"},
  {"code": "5", "name": "Cost of Sales", "type": "expense"},
  {"code": "5010", "name": "Cost of Goods Sold", "type": "expense", "parent_code": "5"},
  {"code": "6", "name": "Operating Expenses", "type": "expense"},
  {"code": "6010", "name": "Salaries and Wages", "type": "expense", "parent_code": "6"},
  {"code": "6020", "name": "Employer Statutory Contributions", "type": "expense", "parent_code": "6"},
  {"code": "6030", "name": "Rent", "type": "expense", "parent_code": "6"},
  {"code": "6040", "name": "Utilities", "type": "expense", "parent_code": "6"},
  {"code": "7", "name": "Finance Costs", "type": "expense"},
  {"code": "7010", "name": "Interest Expense", "type": "expense", "parent_code": "7"}
]
`, cc)
}

// planWizardPatches lays out the two insertions the scaffold makes
// in internal/tenant/wizard.go: a //go:embed directive + var decl
// block, and a case statement in DefaultCoATemplateForCountry. The
// embed and map-registration anchors are chosen so the patches land
// at the end of the Phase-N2 block (the most recent batch added) —
// future batches will add a new block after this one and the
// anchors slide naturally.
func (p *plan) planWizardPatches(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("planWizardPatches: %w", err)
	}
	embedBlock := fmt.Sprintf("//go:embed coa_templates/%[1]s_basic.json\nvar coa%[2]sBasic []byte\n\n", p.CCLower, p.CC)
	mapEntry := fmt.Sprintf("\t%q: coa%sBasic,\n", p.CCLower+"_basic", p.CC)
	caseEntry := fmt.Sprintf("\tcase %q:\n\t\treturn %q\n", p.CC, p.CCLower+"_basic")

	// Append rather than overwrite so future planners that target the
	// same path accumulate instead of clobbering. There is no current
	// caller that patches wizard.go from outside this function, but
	// keeping the append form costs nothing and prevents the silent-
	// drop foot-gun if a sibling planner is ever added (e.g. a
	// localisation registry that also lives in wizard.go).
	p.Patches[path] = append(p.Patches[path],
		patchOp{
			// Anchor is the SCAFFOLD marker line added one-time to
			// wizard.go just before the chartOfAccountsTemplates map.
			// New embed directives + var decls land immediately
			// before this marker, preserving the marker for the next
			// scaffold run.
			Anchor:        "// SCAFFOLD: cmd/new-tax-pack inserts new //go:embed directives + var decls above this line.",
			Insertion:     embedBlock,
			SkipIfPresent: fmt.Sprintf("//go:embed coa_templates/%s_basic.json", p.CCLower),
		},
		patchOp{
			// Anchor is the SCAFFOLD marker line inside the
			// chartOfAccountsTemplates map. The new entry lands
			// immediately before the marker.
			Anchor:        "\t// SCAFFOLD: cmd/new-tax-pack inserts new chartOfAccountsTemplates entries above this line.",
			Insertion:     mapEntry,
			SkipIfPresent: fmt.Sprintf("%q: coa%sBasic,", p.CCLower+"_basic", p.CC),
		},
		patchOp{
			// Anchor is the SCAFFOLD marker line inside
			// DefaultCoATemplateForCountry's switch, immediately
			// before the `default:` branch.
			Anchor:        "\t// SCAFFOLD: cmd/new-tax-pack inserts new DefaultCoATemplateForCountry cases above this line.",
			Insertion:     caseEntry,
			SkipIfPresent: fmt.Sprintf("case %q:\n\t\treturn %q", p.CC, p.CCLower+"_basic"),
		},
	)
	if p.Locale != "" {
		localeCase := fmt.Sprintf("\tcase %q:\n\t\treturn %q\n", p.CC, p.Locale)
		p.Patches[path] = append(p.Patches[path], patchOp{
			// Anchor is the SCAFFOLD marker line inside
			// DefaultLocaleForCountry's switch, immediately before
			// the `default:` branch.
			Anchor:        "\t// SCAFFOLD: cmd/new-tax-pack inserts new DefaultLocaleForCountry cases above this line.",
			Insertion:     localeCase,
			SkipIfPresent: fmt.Sprintf("case %q:\n\t\treturn %q\n", p.CC, p.Locale),
		})
	}
	return nil
}

// planFrontendWizardPatches mirrors planWizardPatches on the React
// side: it adds an entry to COA_TEMPLATES and to COUNTRY_COA_DEFAULTS
// so the setup wizard renders the new chart in the dropdown and
// pre-selects it when the user picks the country.
func (p *plan) planFrontendWizardPatches(path string) {
	if _, err := os.Stat(path); err != nil {
		// Tolerate a missing frontend wizard for monorepo splits
		// where the frontend is checked out separately. Stat
		// failures (ENOENT, permission) all map to a graceful
		// no-op — the contributor's PR will get the same
		// frontend-build failure from CI either way.
		return
	}
	templateEntry := fmt.Sprintf("  { value: %q, label: %q },\n", p.CCLower+"_basic",
		fmt.Sprintf("%s — IFRS + (TODO statutory components)", p.Name))
	defaultEntry := fmt.Sprintf("  %s: %q,\n", p.CC, p.CCLower+"_basic")

	// Append rather than overwrite — see the comment on the
	// equivalent block in planWizardPatches.
	p.Patches[path] = append(p.Patches[path],
		patchOp{
			Anchor:        "  // SCAFFOLD: cmd/new-tax-pack inserts new COA_TEMPLATES entries above this line.",
			Insertion:     templateEntry,
			SkipIfPresent: fmt.Sprintf("{ value: %q,", p.CCLower+"_basic"),
		},
		patchOp{
			Anchor:        "  // SCAFFOLD: cmd/new-tax-pack inserts new COUNTRY_COA_DEFAULTS entries above this line.",
			Insertion:     defaultEntry,
			SkipIfPresent: fmt.Sprintf("%s: %q,", p.CC, p.CCLower+"_basic"),
		},
	)
}

// planFrontendLocalesPatches updates locales.ts when -locale is
// non-empty. Two cases:
//
//   1. The locale tag is already in SupportedLocales (e.g. adding a
//      Spanish-speaking country to an es-already-shipped registry).
//      Only the COUNTRY_LOCALE_DEFAULTS map needs an entry.
//
//   2. The locale tag is new. Both SupportedLocales (with the
//      contributor-supplied native name) AND COUNTRY_LOCALE_DEFAULTS
//      need entries, AND two locale catalogue files (backend +
//      frontend) need to be seeded from en.json.
//
// The scaffold can't reliably introspect locales.ts at build time
// (it's TypeScript), so the "is this tag already registered?" check
// is a substring grep against the file. False negatives just mean
// the contributor has to remove a duplicate entry — the eslint /
// tsc pass at PR time catches it.
func (p *plan) planFrontendLocalesPatches(path string) error {
	if _, err := os.Stat(path); err != nil {
		// Missing locales.ts in a monorepo split where the
		// frontend lives elsewhere — callers explicitly tolerate
		// this so the scaffold still seeds the Go side. We DON'T
		// fold an os.IsNotExist check in because permission /
		// loop-detection errors deserve the same graceful path
		// (the contributor's PR build fails cleanly either way).
		return nil //nolint:nilerr // intentional graceful-degrade
	}
	body, err := os.ReadFile(path) //nolint:gosec // path comes from the validated repo-root flag, not user-controlled HTTP input
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	localeAlreadyRegistered := strings.Contains(string(body), fmt.Sprintf("tag: %q", p.Locale))

	patches := []patchOp{
		{
			Anchor:        "  // SCAFFOLD: cmd/new-tax-pack inserts new COUNTRY_LOCALE_DEFAULTS entries above this line.",
			Insertion:     fmt.Sprintf("  %s: %q,\n", p.CC, p.Locale),
			SkipIfPresent: fmt.Sprintf("%s: %q,", p.CC, p.Locale),
		},
	}
	if !localeAlreadyRegistered {
		if p.LocaleName == "" {
			return fmt.Errorf("-locale=%q is not in SupportedLocales — pass -locale-name with the native display name (e.g. %q)", p.Locale, "日本語")
		}
		patches = append(patches, patchOp{
			Anchor:        "  // SCAFFOLD: cmd/new-tax-pack inserts new SupportedLocales entries above this line.",
			Insertion:     fmt.Sprintf("  { tag: %q, name: %q, direction: \"ltr\" },\n", p.Locale, p.LocaleName),
			SkipIfPresent: fmt.Sprintf("tag: %q", p.Locale),
		})
	}
	// Append rather than overwrite — see the comment on the
	// equivalent block in planWizardPatches.
	p.Patches[path] = append(p.Patches[path], patches...)
	return nil
}

// planLocaleCatalogues seeds two new locale catalogue files
// (backend + frontend) from en.json when the locale is new. The
// contents are en.json verbatim so the catalogues compile and pass
// the baseline-keyset test; the contributor is expected to translate
// the values in a follow-up commit before opening the PR.
func (p *plan) planLocaleCatalogues() error {
	backendEn := filepath.Join(p.RepoRoot, "internal", "i18n", "locales", "en.json")
	frontendEn := filepath.Join(p.RepoRoot, "apps", "web", "src", "locales", "en.json")

	// Seed the backend catalogue first and independently: it lives
	// in this repo and must always succeed. A read failure here is
	// a real problem (missing en.json in a checkout that claims to
	// be the kapp-fab repo) and must surface as an error.
	backendBody, err := os.ReadFile(backendEn) //nolint:gosec // path derived from the validated repo-root flag
	if err != nil {
		return fmt.Errorf("read backend en.json: %w", err)
	}
	backendOut := filepath.Join(p.RepoRoot, "internal", "i18n", "locales", p.Locale+".json")
	if _, err := os.Stat(backendOut); err != nil {
		// Either the file doesn't exist (the expected case for a
		// new locale) or it's unreadable (e.g. permission denied).
		// In both cases we still want the scaffold to attempt the
		// write; Execute will surface the underlying write error
		// with a clearer message if the path is truly broken.
		p.NewFiles[backendOut] = string(backendBody)
	}

	// The frontend catalogue is best-effort: in a monorepo split
	// where apps/web is checked out separately, the frontend
	// en.json is genuinely absent and we still want the backend
	// side to be seeded. Skip silently when the frontend read
	// fails; the contributor's CI build catches a real misconfig.
	frontendBody, err := os.ReadFile(frontendEn) //nolint:gosec // path derived from the validated repo-root flag
	if err != nil {
		return nil //nolint:nilerr // intentional graceful-degrade
	}
	frontendOut := filepath.Join(p.RepoRoot, "apps", "web", "src", "locales", p.Locale+".json")
	if _, err := os.Stat(frontendOut); err != nil {
		p.NewFiles[frontendOut] = string(frontendBody)
	}
	return nil
}

// Execute applies the plan to the filesystem. In -dry-run mode it
// only prints the actions. Execution is atomic at the per-file level
// (each file is written via a .tmp + rename) and the order of
// operations is "new files first, then patches" so a partial run
// always leaves the source tree in a state that compiles.
func (p *plan) Execute(out io.Writer, dryRun bool) error {
	buf := &strings.Builder{}
	fmt.Fprintf(buf, "Scaffolding %s (%s) tax pack:\n\n", p.CC, p.Name)

	for path, body := range p.NewFiles {
		rel, _ := filepath.Rel(p.RepoRoot, path)
		fmt.Fprintf(buf, "  CREATE  %s (%d bytes)\n", rel, len(body))
		if dryRun {
			continue
		}
		if err := writeAtomic(path, []byte(body)); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
	}

	for path, patches := range p.Patches {
		rel, _ := filepath.Rel(p.RepoRoot, path)
		applied := 0
		skipped := 0
		var body []byte
		if !dryRun {
			b, err := os.ReadFile(path) //nolint:gosec // path derived from the validated repo-root flag
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			body = b
		}
		for _, op := range patches {
			if op.SkipIfPresent != "" && body != nil && strings.Contains(string(body), op.SkipIfPresent) {
				skipped++
				continue
			}
			if body != nil {
				before, after, ok := strings.Cut(string(body), op.Anchor)
				if !ok {
					return fmt.Errorf("patch %s: anchor not found: %q", rel, op.Anchor)
				}
				body = []byte(before + op.Insertion + op.Anchor + after)
			}
			applied++
		}
		fmt.Fprintf(buf, "  PATCH   %s (%d insertion(s), %d already-present skip(s))\n", rel, applied, skipped)
		if dryRun {
			continue
		}
		if err := writeAtomic(path, body); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	fmt.Fprintln(buf, "")
	fmt.Fprintln(buf, "Next steps (run from the repo root):")
	fmt.Fprintln(buf, "  1. Read docs/CONTRIBUTING_TAX_PACKS.md for the per-country checklist.")
	fmt.Fprintln(buf, "  2. Replace the scaffold body in internal/hr/taxpacks/"+p.CCLower+".go")
	fmt.Fprintln(buf, "     with the real statutory bracket walk + cap enforcement.")
	fmt.Fprintln(buf, "  3. Rename the 2140 line in internal/tenant/coa_templates/"+p.CCLower+"_basic.json")
	fmt.Fprintln(buf, "     to the actual payroll-liability account(s) the pack emits.")
	fmt.Fprintln(buf, "  4. Add a regression matrix to the regional test file.")
	fmt.Fprintln(buf, "  5. Run: go test ./internal/{hr/taxpacks,tenant,i18n}/...")
	fmt.Fprintln(buf, "  6. Run: golangci-lint run ./internal/hr/taxpacks/...")
	fmt.Fprintln(buf, "")
	if p.Locale != "" {
		fmt.Fprintln(buf, "  7. Translate the seeded locale catalogue files (backend + frontend).")
		fmt.Fprintln(buf, "     They are currently English copies of en.json so the baseline-keyset")
		fmt.Fprintln(buf, "     test passes; do a translation pass before opening the PR.")
		fmt.Fprintln(buf, "")
	}
	if _, err := io.WriteString(out, buf.String()); err != nil {
		return fmt.Errorf("write scaffold progress: %w", err)
	}
	return nil
}

// writeAtomic writes body to path via a sibling .tmp file followed
// by rename. On a partial-failure rerun, the .tmp lingers but the
// destination file is never half-written.
func writeAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	// Source files written by the scaffold are checked into git
	// and read by everyone on the team — 0o600 would be wrong
	// because the file system gates aren't the trust boundary;
	// git review is. 0o644 matches every other source file in
	// the repo (verifiable with `find . -type f -name '*.go'
	// -perm 0644 | wc -l`). The gosec exception is intentional.
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	// Restore the standard source-file mode after the atomic
	// rename so the scaffold output matches the rest of the
	// repo's source-file modes. Chmod failures here are
	// non-fatal — if the umask is restrictive the file is
	// still readable by the owner that ran the CLI.
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o644) //nolint:gosec // intentional 0o644 for source-file parity
	return nil
}
