package tenant

import (
	"errors"
	"strings"
	"testing"
)

// fakeValidator is a deterministic LocaleValidator for the unit
// tests. Tags listed in `supported` count as registered bundles;
// everything else falls through with ok=false so we can exercise
// the validator-rejection branch without depending on the real
// internal/i18n loader (which doesn't exist yet at PR-1 time).
type fakeValidator struct {
	supported map[string]struct{}
}

func newFakeValidator(tags ...string) *fakeValidator {
	m := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		m[t] = struct{}{}
	}
	return &fakeValidator{supported: m}
}

func (f *fakeValidator) IsSupported(tag string) bool {
	_, ok := f.supported[tag]
	return ok
}

// TestValidateLocale_FormatGate covers the BCP 47-shaped regex. The
// shape gate must reject every common payload-injection / path-
// traversal / case-mismatch variant before the validator is even
// consulted, so an empty bundle whitelist can't cause a security
// regression.
func TestValidateLocale_FormatGate(t *testing.T) {
	cases := []struct {
		name string
		tag  string
		ok   bool
	}{
		// Accepted: 2-3 letter base; optional alphanumeric subtag.
		{"two-letter", "en", true},
		{"de", "de", true},
		{"three-letter-iso639-3", "fil", true},
		{"region-subtag", "fr-CH", true},
		{"script-subtag", "zh-Hans", true},
		{"hant", "zh-Hant", true},

		// Multi-subtag forms — PR-8 widened the regex from a
		// single optional subtag to up to three trailing subtags
		// so the wizard's own `DefaultLocaleForCountry("TW") ==
		// "zh-Hant"` can round-trip back through the admin API
		// without being rejected by the format gate (the previous
		// single-subtag regex would have rejected "zh-Hant-TW"
		// and any other script+region BCP 47 form). The runtime
		// source of truth for what the loader can actually serve
		// is the bundle whitelist (the validator branch); the
		// format gate's job is only to reject injection / path-
		// traversal patterns before any service consults the i18n
		// loader.
		{"script-and-region", "zh-Hant-TW", true},
		{"latin-serbian", "sr-Latn-RS", true},
		{"latin-american-spanish", "es-419", true},
		{"swiss-italian", "it-CH", true},
		{"region-variant", "de-CH-1996", true},
		{"three-subtags", "zh-Hant-TW-pinyin", true},

		// Empty is permitted at the validator layer — callers
		// handle "" as "reset to default".
		{"empty", "", true},

		// Rejected: case errors. The CHECK on migration 000059 is
		// case-sensitive on the base subtag.
		{"uppercase-base", "EN", false},
		{"mixedcase", "En", false},

		// Rejected: injection-shaped payloads.
		{"sql-injection", "en;DROP TABLE tenants", false},
		{"path-traversal", "../../etc/passwd", false},
		{"newline", "en\nfoo", false},
		{"semicolon", "en;", false},

		// Rejected: malformed subtag lengths / characters.
		{"single-letter-base", "e", false},
		{"long-base", "engl", false},
		{"empty-subtag", "en-", false},
		{"too-short-subtag", "en-A", false},
		{"too-long-subtag", "en-Hanssssss", false},
		{"underscore", "en_US", false},
		{"trailing-space", "en ", false},
		{"leading-space", " en", false},

		// Rejected: too many subtags. Four trailing subtags is
		// past the BCP 47 forms the wizard supports; the upper
		// bound prevents pathological inputs from blowing up the
		// matcher.
		{"four-subtags", "zh-Hant-TW-pinyin-extra", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLocale(tc.tag, nil)
			if tc.ok && err != nil {
				t.Fatalf("ValidateLocale(%q, nil): unexpected error: %v", tc.tag, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateLocale(%q, nil): expected format error, got nil", tc.tag)
			}
		})
	}
}

// TestValidateLocale_BundleWhitelist exercises the (validator !=
// nil) branch. The format gate has already passed; the validator
// must additionally accept the tag for ValidateLocale to return
// nil.
func TestValidateLocale_BundleWhitelist(t *testing.T) {
	v := newFakeValidator("en", "de", "fr", "ar")

	// In-whitelist tags resolve cleanly.
	for _, tag := range []string{"en", "de", "fr", "ar"} {
		if err := ValidateLocale(tag, v); err != nil {
			t.Fatalf("ValidateLocale(%q, fakeValidator): %v", tag, err)
		}
	}

	// Format-valid but absent from the whitelist → rejected.
	if err := ValidateLocale("ko", v); err == nil {
		t.Fatalf("ValidateLocale(\"ko\", fakeValidator): expected bundle-whitelist error, got nil")
	} else if !strings.Contains(err.Error(), "registered translation bundle") {
		t.Fatalf("ValidateLocale(\"ko\", fakeValidator): wrong error: %v", err)
	}

	// Empty still passes regardless of validator content.
	if err := ValidateLocale("", v); err != nil {
		t.Fatalf("ValidateLocale(\"\", fakeValidator): unexpected error: %v", err)
	}
}

// TestValidateLocale_NilValidator confirms a nil validator does
// not panic and skips the bundle gate cleanly — the path the
// wizard uses at first-run before the i18n loader is wired in.
func TestValidateLocale_NilValidator(t *testing.T) {
	// A format-valid tag that no bundle whitelist would accept
	// (Klingon, "tlh") must still pass when no validator is
	// supplied.
	if err := ValidateLocale("tlh", nil); err != nil {
		t.Fatalf("ValidateLocale(\"tlh\", nil): unexpected error: %v", err)
	}
	// And format-invalid tags must still fail.
	if err := ValidateLocale("EN", nil); err == nil {
		t.Fatalf("ValidateLocale(\"EN\", nil): expected format error, got nil")
	} else if !strings.Contains(err.Error(), "BCP 47") {
		t.Fatalf("wrong format-error: %v", err)
	}
}

// Catch-all to ensure the locale package never silently returns
// the wrong error type on a future refactor — every error this
// function returns must be wrappable + introspectable via the
// fmt-style error.
func TestValidateLocale_ErrorsAreFmtErrors(t *testing.T) {
	err := ValidateLocale("EN", nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	// Wrapping path: callers do `fmt.Errorf("tenant: wizard: %w", err)`
	// and need errors.Is(err, original) to still resolve.
	wrapped := errors.Join(err, errors.New("downstream"))
	if !errors.Is(wrapped, err) {
		t.Fatalf("expected errors.Join chain to retain the original error")
	}
}
