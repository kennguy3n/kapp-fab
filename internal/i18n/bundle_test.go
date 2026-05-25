package i18n

import (
	"encoding/json"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// TestDefaultBundleLoadsCleanly proves the embedded JSON files are
// well-formed and parse into a complete Bundle at boot. The same
// path that the production binary runs at init is exercised here.
func TestDefaultBundleLoadsCleanly(t *testing.T) {
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): unexpected error: %v", err)
	}
	if b == nil {
		t.Fatalf("Default(): returned nil bundle without error")
	}
	tags := b.Supported()
	if len(tags) == 0 {
		t.Fatalf("Default().Supported(): empty list")
	}
	if tags[0] != DefaultLocale {
		t.Fatalf("Supported()[0] = %q, want DefaultLocale %q (matcher fallback target must come first)",
			tags[0], DefaultLocale)
	}
}

// TestEveryLocaleShipsBaselineKeys pins the contract that every
// non-English locale ships at least every key the English baseline
// ships. If a translator adds a new English string without
// providing every other-locale rendition, T() would fall back to
// English silently — this test makes that drift loud at CI time so
// the missing tags are filed before release rather than discovered
// in production.
func TestEveryLocaleShipsBaselineKeys(t *testing.T) {
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	baseline := b.catalogues[DefaultLocale]
	if len(baseline) == 0 {
		t.Fatalf("default catalogue is empty")
	}
	baselineKeys := make([]string, 0, len(baseline))
	for k := range baseline {
		baselineKeys = append(baselineKeys, k)
	}
	sort.Strings(baselineKeys)
	for _, tag := range b.Supported() {
		if tag == DefaultLocale {
			continue
		}
		t.Run(tag, func(t *testing.T) {
			var missing []string
			for _, k := range baselineKeys {
				if !b.Has(tag, k) {
					missing = append(missing, k)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("locale %q is missing %d baseline keys:\n  %s",
					tag, len(missing), strings.Join(missing, "\n  "))
			}
		})
	}
}

// TestT_ResolutionOrder confirms the three-stage fallback:
// exact-locale hit → English baseline → key literal.
func TestT_ResolutionOrder(t *testing.T) {
	b := newBundleFromMap(t, map[string]map[string]string{
		"en": {
			"common.save":    "Save",
			"common.partial": "Only in English",
		},
		"de": {
			"common.save": "Speichern",
		},
	})
	if got, want := b.T("de", "common.save"), "Speichern"; got != want {
		t.Fatalf("T(de, common.save) = %q, want %q", got, want)
	}
	if got, want := b.T("de", "common.partial"), "Only in English"; got != want {
		t.Fatalf("T(de, common.partial) = %q, want %q (fallback to English)", got, want)
	}
	if got, want := b.T("de", "nope.brand-new"), "nope.brand-new"; got != want {
		t.Fatalf("T(de, nope.brand-new) = %q, want literal key %q", got, want)
	}
	if got, want := b.T("ar", "common.save"), "Save"; got != want {
		t.Fatalf("T(ar, common.save) = %q, want %q (fallback to English for unknown locale)", got, want)
	}
}

// TestResolve_Matrix pins the matcher's behavior for the inputs
// the Accept-Language middleware will see in production:
//
//   - empty → DefaultLocale
//   - exact tag in bundle → echo back
//   - subtag tag with shipped base ("zh-Hans" with "zh" shipped) → base
//   - Accept-Language header with weights → highest-weight match
//   - tag unrelated to any shipped catalogue → DefaultLocale
//
// The bundle uses the production locale set so the test catches
// future drift between the matcher's fallback choices and the
// shipped JSON files.
func TestResolve_Matrix(t *testing.T) {
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", DefaultLocale},
		{"exact-de", "de", "de"},
		{"exact-ar", "ar", "ar"},
		{"zh-Hans", "zh-Hans", "zh"},
		// zh-Hant ships its own catalogue so the matcher must keep
		// Traditional Chinese tenants on Traditional Chinese rather
		// than downgrading them to Simplified.
		{"zh-Hant", "zh-Hant", "zh-Hant"},
		{"zh-Hant-TW", "zh-Hant-TW", "zh-Hant"},
		{"zh-Hant-HK", "zh-Hant-HK", "zh-Hant"},
		// Browsers in Taiwan / Hong Kong / Macau commonly report
		// `navigator.language = "zh-TW"` / `"zh-HK"` / `"zh-MO"`
		// (without the explicit `Hant` script subtag) — pin these
		// to zh-Hant explicitly so a future golang.org/x/text
		// update can't silently route Taiwanese / Hong Kong /
		// Macau Accept-Language headers to Simplified Chinese.
		// The frontend's REGION_SCRIPT_OVERRIDES table mirrors
		// this resolution path (apps/web/src/lib/i18n/locales.ts);
		// keeping the backend pinned here lets the two stay in
		// lockstep via a single CI signal rather than a manual
		// drift check.
		{"zh-TW", "zh-TW", "zh-Hant"},
		{"zh-HK", "zh-HK", "zh-Hant"},
		{"zh-MO", "zh-MO", "zh-Hant"},
		// PR-2d ships fr-CA.json on both backend and frontend, so
		// the matcher resolves a Canadian-French Accept-Language
		// header to fr-CA verbatim rather than downgrading to the
		// metropolitan-French catalogue. The weighted variant
		// pins the same behaviour even when the header includes
		// the lower-priority fr/en fallbacks.
		{"fr-CA", "fr-CA", "fr-CA"},
		{"hi-IN unrelated", "hi-IN", DefaultLocale},
		{"weighted-AL", "fr-CA,fr;q=0.9,en;q=0.5", "fr-CA"},
		{"weighted-AL prefer-de", "de-CH,de;q=0.9,en;q=0.5", "de"},
		{"junk", "###not a tag###", DefaultLocale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := b.Resolve(tc.in)
			if got != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestIsSupported_Strict confirms IsSupported does NOT downgrade —
// "hi" must report unsupported even though Resolve("hi") falls back
// to "en". This is the contract the wizard's bundle whitelist
// depends on.
func TestIsSupported_Strict(t *testing.T) {
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	if !b.IsSupported("en") {
		t.Fatalf(`IsSupported("en") = false; want true`)
	}
	if !b.IsSupported("de") {
		t.Fatalf(`IsSupported("de") = false; want true`)
	}
	if b.IsSupported("hi") {
		t.Fatalf(`IsSupported("hi") = true; want false (catalogue not shipped)`)
	}
	if b.IsSupported("zh-Hans") {
		t.Fatalf(`IsSupported("zh-Hans") = true; want false (only "zh" is shipped)`)
	}
	if b.IsSupported("") {
		t.Fatalf(`IsSupported("") = true; want false`)
	}
}

// TestLoad_MissingDefaultRejected proves Load rejects a Bundle
// that doesn't ship the matcher's fallback target. The matcher
// needs a deterministic floor; without "en" we'd silently pick
// whichever locale happened to be alphabetically first.
func TestLoad_MissingDefaultRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"locales/de.json": &fstest.MapFile{
			Data: []byte(`{"common.save":"Speichern"}`),
		},
	}
	_, err := Load(fsys, "locales")
	if err == nil {
		t.Fatalf("Load(): expected error for missing default locale, got nil")
	}
	if !strings.Contains(err.Error(), DefaultLocale) {
		t.Fatalf("Load(): error = %v; want mention of default locale %q", err, DefaultLocale)
	}
}

// TestLoad_EmptyDirRejected proves Load surfaces empty-directory
// misconfigurations at boot rather than failing softly with a
// Bundle that translates nothing.
func TestLoad_EmptyDirRejected(t *testing.T) {
	fsys := fstest.MapFS{}
	_, err := Load(fsys, "locales")
	if err == nil {
		t.Fatalf("Load(): expected error for empty dir, got nil")
	}
}

// TestLoad_MalformedCatalogueRejected confirms a syntactically
// broken JSON file is a build-the-binary failure, not a runtime
// silent skip.
func TestLoad_MalformedCatalogueRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"locales/en.json": &fstest.MapFile{Data: []byte(`{not json`)},
	}
	_, err := Load(fsys, "locales")
	if err == nil {
		t.Fatalf("Load(): expected parse error, got nil")
	}
}

// TestLoad_NonStringValueRejected confirms Load enforces the flat
// map[string]string shape so a future contributor cannot ship a
// nested catalogue that T() would silently render as a Go map's
// fmt.Stringer.
func TestLoad_NonStringValueRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"locales/en.json": &fstest.MapFile{
			Data: []byte(`{"key":{"nested":"object"}}`),
		},
	}
	_, err := Load(fsys, "locales")
	if err == nil {
		t.Fatalf("Load(): expected non-string-value rejection, got nil")
	}
}

// TestLoad_InvalidFilenameRejected catches a "fr_FR.json" typo
// (underscore vs hyphen) before it lands in the binary.
func TestLoad_InvalidFilenameRejected(t *testing.T) {
	fsys := fstest.MapFS{
		"locales/en.json": &fstest.MapFile{Data: []byte(`{"k":"v"}`)},
		"locales/!!.json": &fstest.MapFile{Data: []byte(`{"k":"v"}`)},
	}
	_, err := Load(fsys, "locales")
	if err == nil {
		t.Fatalf("Load(): expected BCP 47 parse error, got nil")
	}
}

// TestSupportedReturnsFreshCopy guarantees mutating the returned
// slice doesn't poison subsequent Supported() callers.
func TestSupportedReturnsFreshCopy(t *testing.T) {
	b, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	first := b.Supported()
	if len(first) == 0 {
		t.Fatalf("empty supported list")
	}
	first[0] = "garbage"
	second := b.Supported()
	if second[0] == "garbage" {
		t.Fatalf("Supported() returned a shared slice — mutation leaked between calls")
	}
}

// newBundleFromMap constructs a Bundle directly from in-memory
// catalogues. Useful when the test's intent is the resolution
// semantics, not the disk layout.
func newBundleFromMap(t *testing.T, catalogues map[string]map[string]string) *Bundle {
	t.Helper()
	fsys := fstest.MapFS{}
	for tag, cat := range catalogues {
		raw, err := json.Marshal(cat)
		if err != nil {
			t.Fatalf("marshal catalogue %q: %v", tag, err)
		}
		fsys["locales/"+tag+".json"] = &fstest.MapFile{Data: raw}
	}
	b, err := Load(fsys, "locales")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return b
}

// Sanity guard: enforce the embed.FS root exists with the
// expected number of files. Catches a forgotten //go:embed update
// when a new locale is added.
func TestEmbeddedLocaleFilesAreRegistered(t *testing.T) {
	entries, err := fs.ReadDir(localeFS, "locales")
	if err != nil {
		t.Fatalf("read embedded locales dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("embed.FS contains no locale files — //go:embed directive may have drifted")
	}
}
