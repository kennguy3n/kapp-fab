package tenant

import (
	"encoding/json"
	"sort"
	"testing"
)

// TestChartOfAccountsTemplatesAreParseable round-trips every embedded
// CoA template through the wizard's loadTemplate path. A malformed
// JSON file, an unknown account `type`, or an embed directive that
// silently captures an empty payload would surface here before a
// tenant ever hits the wizard at runtime.
func TestChartOfAccountsTemplatesAreParseable(t *testing.T) {
	if len(chartOfAccountsTemplates) == 0 {
		t.Fatalf("expected at least one registered CoA template")
	}
	allowedTypes := map[string]bool{
		"asset":     true,
		"liability": true,
		"equity":    true,
		"revenue":   true,
		"expense":   true,
	}
	for name, payload := range chartOfAccountsTemplates {
		t.Run(name, func(t *testing.T) {
			if len(payload) == 0 {
				t.Fatalf("template %q embed produced an empty payload "+
					"— the //go:embed directive probably missed the "+
					"file or the JSON file is empty", name)
			}
			accounts, err := loadTemplate(name)
			if err != nil {
				t.Fatalf("loadTemplate(%q) failed: %v", name, err)
			}
			if len(accounts) == 0 {
				t.Fatalf("template %q parsed to zero accounts", name)
			}
			// Every code must be unique, every parent must exist,
			// and every type must be one of the five canonical
			// values the accounts table CHECK constraint accepts
			// (see migrations/000001_initial_schema.sql).
			seen := map[string]bool{}
			for _, a := range accounts {
				if a.Code == "" {
					t.Fatalf("template %q has an account with empty code", name)
				}
				if seen[a.Code] {
					t.Fatalf("template %q has duplicate code %q", name, a.Code)
				}
				seen[a.Code] = true
				if a.Name == "" {
					t.Fatalf("template %q account %q has empty name", name, a.Code)
				}
				if !allowedTypes[a.Type] {
					t.Fatalf("template %q account %q has unsupported type %q "+
						"(allowed: asset / liability / equity / revenue / expense)",
						name, a.Code, a.Type)
				}
			}
			for _, a := range accounts {
				if a.ParentCode == "" {
					continue
				}
				if !seen[a.ParentCode] {
					t.Fatalf("template %q account %q references missing parent %q",
						name, a.Code, a.ParentCode)
				}
			}
			// Sanity-check the JSON shape — every entry must be an
			// object with at least code/name/type. We unmarshal a
			// second time into a generic shape so the test fails
			// loudly if someone slips in (e.g.) a top-level array
			// of strings or an object instead of an array.
			var raw []map[string]json.RawMessage
			if err := json.Unmarshal(payload, &raw); err != nil {
				t.Fatalf("template %q is not a JSON array of account objects: %v", name, err)
			}
			if len(raw) != len(accounts) {
				t.Fatalf("template %q parses to %d typed accounts but %d JSON objects — schema drift",
					name, len(accounts), len(raw))
			}
		})
	}
}

// TestDefaultCoATemplateForCountry pins the country → template
// mapping the wizard uses to pre-fill SetupWizardConfig.CoATemplate
// when the caller didn't supply one. Adding a country to the tax
// pack roster without registering a matching CoA here would surface
// as a generic IFRS chart for that country's tenants, which the
// wizard handler would silently accept — better to fail the test
// loudly so the omission is caught at PR time.
func TestDefaultCoATemplateForCountry(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// US uses its own GAAP chart (separate from IFRS).
		{"US", "us_gaap_basic"},
		{"us", "us_gaap_basic"},
		{" US ", "us_gaap_basic"},

		// APAC packs: each gets a country-specific chart.
		{"SG", "sg_basic"},
		{"MY", "my_basic"},
		{"TH", "th_basic"},
		{"ID", "id_basic"},
		{"VN", "vn_basic"},
		{"PH", "ph_basic"},
		{"NZ", "nz_basic"},
		{"IN", "in_basic"},

		// Switzerland + the six GCC packs.
		{"CH", "ch_basic"},
		{"AE", "ae_basic"},
		{"SA", "sa_basic"},
		{"QA", "qa_basic"},
		{"KW", "kw_basic"},
		{"BH", "bh_basic"},
		{"OM", "om_basic"},

		// Unmapped countries fall back to generic IFRS so the
		// wizard always resolves to a registered template.
		{"AU", "ifrs_basic"},
		{"GB", "ifrs_basic"},
		{"DE", "ifrs_basic"},
		{"FR", "ifrs_basic"},
		{"", "ifrs_basic"},
		{"ZZ", "ifrs_basic"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := DefaultCoATemplateForCountry(tc.in)
			if got != tc.want {
				t.Fatalf("DefaultCoATemplateForCountry(%q): got %q, want %q",
					tc.in, got, tc.want)
			}
			// Sanity check: every value the helper returns must be
			// a registered template, so the wizard never resolves to
			// a 404.
			if _, ok := chartOfAccountsTemplates[got]; !ok {
				t.Fatalf("DefaultCoATemplateForCountry(%q) returned %q "+
					"which is not in chartOfAccountsTemplates — the "+
					"helper drifted from the registry", tc.in, got)
			}
		})
	}
}

// TestEveryTaxPackCountryHasCoATemplate is the cross-package contract
// that prevents a future PR from registering a tax pack without
// shipping a matching CoA chart. The list of country codes mirrors
// the registered packs in internal/hr/taxpacks; if a new country is
// added there, this test catches the missing CoA before the wizard
// silently falls back to the generic IFRS chart.
func TestEveryTaxPackCountryHasCoATemplate(t *testing.T) {
	// Mirror of taxpacks.RegisteredCountries() — kept as a local
	// literal so this test does not import the taxpacks package
	// (which would create a hr/taxpacks → tenant test dependency
	// graph that's not desirable). Drift between the two lists is
	// caught by the i18n maintenance checklist in
	// docs/TAX_PACK_MAINTENANCE.md.
	registered := []string{
		"AE", "AU", "BH", "CH", "ID", "IN", "KW", "MY",
		"NZ", "OM", "PH", "QA", "SA", "SG", "TH", "US", "VN",
	}
	sort.Strings(registered)
	for _, cc := range registered {
		t.Run(cc, func(t *testing.T) {
			templateName := DefaultCoATemplateForCountry(cc)
			if _, ok := chartOfAccountsTemplates[templateName]; !ok {
				t.Fatalf("country %s maps to template %q which is not "+
					"in chartOfAccountsTemplates", cc, templateName)
			}
			// AU has no dedicated CoA today — it intentionally
			// falls back to the IFRS chart. Every other registered
			// country must resolve to a country-specific chart so
			// the payroll engine's deduction lines have matching
			// liability accounts.
			if cc == "AU" || cc == "US" {
				return
			}
			expected := toLower(cc) + "_basic"
			if templateName != expected {
				t.Fatalf("country %s should map to %q, got %q",
					cc, expected, templateName)
			}
		})
	}
}

// toLower is a tiny local helper so the test file doesn't drag in
// strings just for one call.
func toLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
