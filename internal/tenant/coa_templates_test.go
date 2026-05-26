package tenant

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/hr/taxpacks"
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

		// PR-2d: Americas. Each Western Hemisphere pack maps to
		// either a standards-named country-specific chart
		// (CA ASPE, BR CPC, MX NIF, AR RT-FACPCE, CL IFRS) or
		// the shared LATAM IFRS chart for the remaining
		// jurisdictions (CO/PE/CR/PA/UY/EC/DO/GT/PY/TT) where
		// the local statutory chart is close enough to plain
		// IFRS that splitting per country would be busywork.
		{"CA", "ca_aspe_basic"},
		{"BR", "br_cpc_basic"},
		{"MX", "mx_nif_basic"},
		{"AR", "ar_rtfacpce_basic"},
		{"CL", "cl_ifrs_basic"},
		{"CO", "latam_ifrs_basic"},
		{"PE", "latam_ifrs_basic"},
		{"CR", "latam_ifrs_basic"},
		{"PA", "latam_ifrs_basic"},
		{"UY", "latam_ifrs_basic"},
		{"EC", "latam_ifrs_basic"},
		{"DO", "latam_ifrs_basic"},
		{"GT", "latam_ifrs_basic"},
		{"PY", "latam_ifrs_basic"},
		{"TT", "latam_ifrs_basic"},

		// Phase N1 — Europe Core + AU. Each gets a
		// country-specific chart with payroll-liability accounts
		// matching its tax pack's deduction codes (PAYE / NIC for
		// GB, Lohnsteuer / Soli / RV-KV-PV-ALV for DE, PAS /
		// CSG / CRDS / SS for FR, IRPF / SS for ES, IRPEF /
		// Addizionali / INPS for IT, Loonheffing / ZVW for NL,
		// Précompte / ONSS for BE, PAYE / USC / PRSI for IE,
		// Lohnsteuer / SV-Beiträge for AT, IRS / Seg. Social for
		// PT, PAYG / Superannuation / FBT / Payroll Tax for AU).
		{"GB", "gb_basic"},
		{"DE", "de_basic"},
		{"FR", "fr_basic"},
		{"ES", "es_basic"},
		{"IT", "it_basic"},
		{"NL", "nl_basic"},
		{"BE", "be_basic"},
		{"IE", "ie_basic"},
		{"AT", "at_basic"},
		{"PT", "pt_basic"},
		{"AU", "au_basic"},

		// Phase N2 — Europe Extended. Nine additional charts,
		// each carrying the country's statutory payroll-liability
		// accounts matching its tax pack's deduction codes
		// (PIT / ZUS / NFZ for PL, Kommunalskatt / Statlig /
		// Pensionsavgift for SE, Inntektsskatt / Trinnskatt /
		// Trygdeavgift for NO, A-skat / AM-bidrag / ATP for DK,
		// Valtio / Kunnallisvero / TyEL / SAVA for FI, Daň /
		// SP / ZP for CZ, SZJA / TB / Szocho for HU, Impozit /
		// CAS / CASS / CAM for RO, PIT / EFKA for GR).
		{"PL", "pl_basic"},
		{"SE", "se_basic"},
		{"NO", "no_basic"},
		{"DK", "dk_basic"},
		{"FI", "fi_basic"},
		{"CZ", "cz_basic"},
		{"HU", "hu_basic"},
		{"RO", "ro_basic"},
		{"GR", "gr_basic"},

		// Unmapped countries still fall back to generic IFRS so
		// the wizard always resolves to a registered template.
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
// shipping a matching CoA chart. Sourced live from
// taxpacks.RegisteredCountries() so adding a country to the tax pack
// roster — even if the local literal in TestDefaultCoATemplateForCountry
// stays untouched — surfaces here as a missing chart before the
// wizard silently falls back to the generic IFRS template.
//
// taxpacks lives below tenant in the dependency graph
// (internal/hr/taxpacks does not import internal/tenant), so this
// import is acyclic.
func TestEveryTaxPackCountryHasCoATemplate(t *testing.T) {
	registered := taxpacks.RegisteredCountries()
	if len(registered) == 0 {
		t.Fatalf("taxpacks.RegisteredCountries() returned empty list — " +
			"either the registry is missing or the helper drifted")
	}
	// Country-specific allow-list for templates whose names do not
	// follow the default "<cc>_basic" convention. Each entry pins
	// the accounting standard the chart encodes — keeping the
	// jurisdiction-to-template mapping legible in a single place.
	// Countries not in this map MUST resolve to the default
	// "<cc>_basic" chart so a typo in DefaultCoATemplateForCountry
	// fails this test loudly.
	//
	// LATAM batch: 10 jurisdictions (CO/PE/CR/PA/UY/EC/DO/GT/PY/TT)
	// share the generic LATAM IFRS chart — their local statutory
	// charts are close enough to IFRS that splitting per-country
	// would be busywork. The 5 LATAM countries with materially
	// different local charts (BR CPC, MX NIF, AR RT-FACPCE, CL
	// IFRS, plus CA ASPE for the north) get their own.
	customTemplate := map[string]string{
		"CA": "ca_aspe_basic",
		"BR": "br_cpc_basic",
		"MX": "mx_nif_basic",
		"AR": "ar_rtfacpce_basic",
		"CL": "cl_ifrs_basic",
		"CO": "latam_ifrs_basic",
		"PE": "latam_ifrs_basic",
		"CR": "latam_ifrs_basic",
		"PA": "latam_ifrs_basic",
		"UY": "latam_ifrs_basic",
		"EC": "latam_ifrs_basic",
		"DO": "latam_ifrs_basic",
		"GT": "latam_ifrs_basic",
		"PY": "latam_ifrs_basic",
		"TT": "latam_ifrs_basic",
	}
	for _, cc := range registered {
		t.Run(cc, func(t *testing.T) {
			templateName := DefaultCoATemplateForCountry(cc)
			if _, ok := chartOfAccountsTemplates[templateName]; !ok {
				t.Fatalf("country %s maps to template %q which is not "+
					"in chartOfAccountsTemplates", cc, templateName)
			}
			// US uses us_gaap_basic (separate from IFRS) by
			// convention; every other registered country (including
			// AU since Phase N1) must resolve to a country-specific
			// chart (either the default "<cc>_basic" or an
			// allow-listed accounting-standard variant) so the
			// payroll engine's deduction lines have matching
			// liability accounts.
			if cc == "US" {
				return
			}
			if expected, ok := customTemplate[cc]; ok {
				if templateName != expected {
					t.Fatalf("country %s should map to allow-listed %q, got %q",
						cc, expected, templateName)
				}
				return
			}
			expected := strings.ToLower(cc) + "_basic"
			if templateName != expected {
				t.Fatalf("country %s should map to %q, got %q",
					cc, expected, templateName)
			}
		})
	}
}
