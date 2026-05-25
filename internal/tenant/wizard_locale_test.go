package tenant

import "testing"

// TestDefaultLocaleForCountry pins the country → locale mapping the
// setup wizard uses to pre-fill `tenants.locale` when the caller
// didn't supply one explicitly. The cases here are NOT auto-derived
// from the source — they are the published "primary business
// locale" for each jurisdiction the i18n plan ships in PR-2/PR-3.
// Changing the mapping changes the column the migration backs up
// with, so a regression here would surface as a wrong-language UI
// for every brand-new tenant in that country.
func TestDefaultLocaleForCountry(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// German-speaking bloc — Germany, Austria, Switzerland all
		// resolve to the de.json catalogue. CH stays German because
		// Swiss-German is the largest business language; admins in
		// the Romandie or Ticino reset to fr or it from the admin
		// surface.
		{"DE", "de"},
		{"AT", "de"},
		{"CH", "de"},
		{"ch", "de"},
		{" CH ", "de"},

		// Other European catalogues that ship locale files —
		// without these mappings a French, Italian, or Spanish
		// tenant would see English at first-run despite the bundle
		// having a perfectly good native catalogue.
		{"FR", "fr"},
		{"IT", "it"},
		{"ES", "es"},

		// Japan → ja.json. The catalogue ships, so a Japanese
		// tenant should land on the native bundle rather than
		// English.
		{"JP", "ja"},

		// Arabic GCC: all six gulf countries map to ar.
		{"SA", "ar"},
		{"AE", "ar"},
		{"QA", "ar"},
		{"KW", "ar"},
		{"BH", "ar"},
		{"OM", "ar"},

		// Localised APAC defaults.
		{"TH", "th"},
		{"ID", "id"},
		{"VN", "vi"},
		{"IN", "hi"},

		// Chinese script variants.
		{"CN", "zh-Hans"},
		{"TW", "zh-Hant"},
		{"HK", "zh-Hant"},

		// PR-2d Americas. Brazil ships its own pt-BR catalogue
		// (Brazilian Portuguese differs from European Portuguese
		// in accounting / payroll terminology). Spanish-speaking
		// LATAM jurisdictions share the existing es.json
		// catalogue.  Canada defaults to English (operators in
		// Québec / Acadia reset to fr-CA from the admin surface).
		{"BR", "pt-BR"},
		{"MX", "es"},
		{"AR", "es"},
		{"CO", "es"},
		{"CL", "es"},
		{"PE", "es"},
		{"CR", "es"},
		{"PA", "es"},
		{"UY", "es"},
		{"EC", "es"},
		{"DO", "es"},
		{"GT", "es"},
		{"PY", "es"},

		// Anglophone fallbacks — English everywhere we don't
		// have a localised bundle, so the locale resolver never
		// has to handle empty/NULL.
		{"US", "en"},
		{"AU", "en"},
		{"NZ", "en"},
		{"SG", "en"}, // SG and MY pick English as the lingua franca.
		{"MY", "en"},
		{"PH", "en"},
		{"GB", "en"},
		{"CA", "en"}, // English-speaking majority; Québec admins reset to fr-CA.
		{"TT", "en"}, // Trinidad & Tobago — English official language.
		{"", "en"},   // empty / unset country.
		{"ZZ", "en"}, // unknown ISO code.
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := DefaultLocaleForCountry(tc.in)
			if got != tc.want {
				t.Fatalf("DefaultLocaleForCountry(%q): got %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}
