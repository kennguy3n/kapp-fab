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
		// Switzerland → German (largest business-language share;
		// operators can switch to fr/it after first-run).
		{"CH", "de"},
		{"ch", "de"},
		{" CH ", "de"},

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
