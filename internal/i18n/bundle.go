// Package i18n is the server-side translation registry.
//
// A single *Bundle owns the in-memory map of locale tag → message
// catalogue and the language.Matcher used to resolve an inbound
// Accept-Language header (or a tenant-stored locale tag) to the
// best-supported catalogue. The same Bundle implements the
// tenant.LocaleValidator contract so the wizard's bundle-whitelist
// gate (see internal/tenant/store.go:ValidateLocale) is sourced from
// the exact same set of tags that the runtime middleware can serve.
// This keeps "what we'll persist on tenants.locale" and "what we can
// actually translate at request time" in lockstep — there is no
// second source of truth.
//
// Locale catalogues are flat key/value JSON files keyed by dotted
// identifiers (e.g. "wizard.step.coa") and live under
// internal/i18n/locales/*.json. They are embedded into the binary at
// build time (//go:embed) so the runtime has zero filesystem
// dependencies; this matches the chart-of-accounts template loader
// in internal/tenant/wizard.go and the tax-pack registry in
// internal/hr/taxpacks/taxpacks.go.
//
// The Bundle is intentionally minimal at this stage:
//
//   - PR-4 (this package) ships the loader, the matcher, the
//     middleware, the context plumbing, and a small core string set
//     covering common UI buttons, validation messages, error
//     surfaces, wizard step labels, account section headers, and
//     payroll line labels. The strings exist so the wiring is
//     exercised by tests and so a tenant-locale-aware backend can
//     emit translated error messages without waiting for the
//     frontend extraction pass.
//   - PR-5 (i18n frontend) extends the same JSON files with the full
//     UI string set extracted from the React app. The backend
//     loader is unchanged at that point — the same Bundle resolves
//     bigger catalogues with no code edit on this side.
//
// Concurrency: a Bundle is read-only after construction. T / Resolve
// / IsSupported are safe for concurrent use from any number of
// goroutines without further synchronisation.
package i18n

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"golang.org/x/text/language"
)

//go:embed locales/*.json
var localeFS embed.FS

// DefaultLocale is the catalogue every Bundle MUST contain. The
// matcher's "no acceptable match" fallback target is the first tag
// passed to language.NewMatcher; we always pass DefaultLocale first
// so an unrecognised Accept-Language collapses to a predictable
// English UI rather than picking an arbitrary supported tag.
const DefaultLocale = "en"

// Bundle is the in-memory registry of locale → key → message strings
// for the whole API process. Construct it once at boot via Load and
// share the pointer with every handler / middleware that needs to
// translate; the value type is read-only after Load returns.
type Bundle struct {
	// catalogues holds every parsed locale catalogue keyed by the
	// catalogue's canonical BCP 47 tag (lower-cased base; subtags
	// preserved). Reads are lock-free because the map is never
	// mutated after Load.
	catalogues map[string]map[string]string

	// supported is the sorted list of tags returned by Supported().
	// Pre-computing this here avoids allocating + sorting on every
	// /api/v1/locales request and gives callers a stable iteration
	// order for snapshot tests.
	supported []string

	// matcher is the language.Matcher used to resolve an
	// Accept-Language header or a free-form tag to the
	// best-supported catalogue. Constructed once from the parsed
	// catalogue tags so request-time resolution is allocation-free
	// except for the returned tag string.
	matcher language.Matcher

	// tagSet holds the same set as catalogues but stores the
	// language.Tag for each entry. IsSupported uses this for an
	// O(1) exact-match check; the wizard's bundle whitelist must
	// be exact (no implicit fallback) so an operator picking "hi"
	// when only "en" is shipped sees a deterministic rejection
	// rather than a silent downgrade.
	tagSet map[string]struct{}
}

// loadOnce guards the package-level singleton built by Default().
var (
	loadOnce      sync.Once
	defaultBundle *Bundle
	errDefault    error
)

// Default returns a process-wide Bundle backed by the embedded
// locale files. It's the entry point production code should use;
// tests that need a custom locale set call Load with their own
// fs.FS instead.
//
// The first call parses every embedded JSON file and constructs the
// language matcher; subsequent calls are O(1) lookups of the cached
// result. A parse failure is sticky — if the embedded data is
// malformed at boot, every subsequent call returns the same error so
// the process refuses to come up rather than silently serving an
// incomplete catalogue.
func Default() (*Bundle, error) {
	loadOnce.Do(func() {
		defaultBundle, errDefault = Load(localeFS, "locales")
	})
	return defaultBundle, errDefault
}

// MustDefault is the panicking variant of Default, suitable for
// init-time wiring where a malformed embedded catalogue is a
// build-the-binary bug and not a runtime condition the caller can
// recover from.
func MustDefault() *Bundle {
	b, err := Default()
	if err != nil {
		panic(fmt.Sprintf("i18n: load default bundle: %v", err))
	}
	return b
}

// Load constructs a Bundle from the supplied filesystem. dir is the
// path within fsys to scan; every "*.json" file in that directory is
// parsed as a flat map[string]string and registered under the file
// name stripped of the extension (e.g. "de.json" → "de").
//
// The function rejects:
//   - an empty filesystem (no .json files found),
//   - a file that doesn't parse as a JSON object of string values,
//   - a tag that doesn't parse via language.Parse,
//   - a Bundle that doesn't contain DefaultLocale ("en") — the
//     matcher's fallback target must always be present.
//
// All four conditions are configuration errors caught at boot; they
// can't be triggered by a request once Load returns successfully.
func Load(fsys fs.FS, dir string) (*Bundle, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("i18n: read locales dir %q: %w", dir, err)
	}
	catalogues := make(map[string]map[string]string, len(entries))
	tagSet := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		tag := strings.TrimSuffix(e.Name(), ".json")
		if _, err := language.Parse(tag); err != nil {
			return nil, fmt.Errorf(
				"i18n: locale filename %q is not a parseable BCP 47 tag: %w",
				e.Name(), err)
		}
		raw, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("i18n: read %q: %w", e.Name(), err)
		}
		var cat map[string]string
		if err := json.Unmarshal(raw, &cat); err != nil {
			return nil, fmt.Errorf(
				"i18n: parse %q as map[string]string: %w",
				e.Name(), err)
		}
		if len(cat) == 0 {
			return nil, fmt.Errorf(
				"i18n: catalogue %q is empty — every locale must "+
					"ship at least one message",
				e.Name())
		}
		catalogues[tag] = cat
		tagSet[tag] = struct{}{}
	}
	if len(catalogues) == 0 {
		return nil, errors.New("i18n: no *.json catalogues found")
	}
	if _, ok := catalogues[DefaultLocale]; !ok {
		return nil, fmt.Errorf(
			"i18n: default locale %q is missing — the matcher "+
				"fallback target must always be present",
			DefaultLocale)
	}
	// Put DefaultLocale first so the language.Matcher uses it as
	// the no-acceptable-match fallback. The remaining order does
	// not matter for resolution semantics but is stabilised for
	// snapshot-test determinism.
	supported := make([]string, 0, len(catalogues))
	for tag := range catalogues {
		if tag == DefaultLocale {
			continue
		}
		supported = append(supported, tag)
	}
	sort.Strings(supported)
	supported = append([]string{DefaultLocale}, supported...)
	matcherTags := make([]language.Tag, 0, len(supported))
	for _, t := range supported {
		matcherTags = append(matcherTags, language.Make(t))
	}
	return &Bundle{
		catalogues: catalogues,
		supported:  supported,
		matcher:    language.NewMatcher(matcherTags),
		tagSet:     tagSet,
	}, nil
}

// Supported returns the sorted list of locale tags this bundle can
// translate. The first element is always DefaultLocale ("en").
//
// The returned slice is a fresh copy on each call so callers can
// retain or mutate it without affecting future Supported() callers
// or the bundle's internal state.
func (b *Bundle) Supported() []string {
	out := make([]string, len(b.supported))
	copy(out, b.supported)
	return out
}

// IsSupported reports whether tag matches an exact catalogue in the
// bundle. It implements tenant.LocaleValidator so the same Bundle
// can be passed straight into tenant.ValidateLocale / SetLocale to
// gate writes against the runtime-serviceable set.
//
// IsSupported is intentionally strict — "hi" is not accepted when
// only "en" is shipped, even though Resolve("hi") would fall back to
// "en" at request time. Writes to tenants.locale must be exact so an
// operator's stored preference does not silently downgrade across
// deploys when a bundle is added or removed.
func (b *Bundle) IsSupported(tag string) bool {
	if tag == "" {
		return false
	}
	_, ok := b.tagSet[strings.TrimSpace(tag)]
	return ok
}

// Resolve returns the best supported catalogue tag for the given
// candidate. The candidate may be:
//   - an empty string (returns DefaultLocale),
//   - a single BCP 47 tag (e.g. "fr-CA"),
//   - an Accept-Language header value with weights (e.g. "fr-CA,fr;q=0.9,en;q=0.5"),
//   - a tag with subtags not present in the bundle ("zh-Hans" → "zh"),
//   - a tag with no related language in the bundle ("hi" → DefaultLocale).
//
// Resolve never returns an empty string and never returns a tag
// that is not in Supported(). It is the canonical way to pick a
// catalogue for a given request and is what the Accept-Language
// middleware uses internally.
func (b *Bundle) Resolve(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return DefaultLocale
	}
	tags, _, err := language.ParseAcceptLanguage(candidate)
	if err != nil || len(tags) == 0 {
		return DefaultLocale
	}
	_, idx, _ := b.matcher.Match(tags...)
	if idx < 0 || idx >= len(b.supported) {
		return DefaultLocale
	}
	return b.supported[idx]
}

// T returns the translated message for key in the supplied locale.
//
// Resolution order:
//  1. The exact key in the requested locale's catalogue.
//  2. The same key in DefaultLocale (English).
//  3. The literal key itself, returned unchanged.
//
// This three-stage fallback means a partially-translated catalogue
// never blocks a release — missing strings render as English, and a
// brand-new key that hasn't reached translators yet renders as its
// identifier (e.g. "wizard.brandNew") which is visibly broken in
// the UI but never blank or panicking.
//
// T is allocation-free on cache hits and is safe for concurrent use.
func (b *Bundle) T(locale, key string) string {
	if cat, ok := b.catalogues[locale]; ok {
		if msg, ok := cat[key]; ok {
			return msg
		}
	}
	if locale != DefaultLocale {
		if cat, ok := b.catalogues[DefaultLocale]; ok {
			if msg, ok := cat[key]; ok {
				return msg
			}
		}
	}
	return key
}

// Has reports whether the bundle has an explicit translation for
// key in the given locale (no fallback to DefaultLocale). It exists
// so coverage tests can assert each locale catalogue keeps pace
// with the English baseline without scanning every key the test
// already knows about.
func (b *Bundle) Has(locale, key string) bool {
	cat, ok := b.catalogues[locale]
	if !ok {
		return false
	}
	_, ok = cat[key]
	return ok
}
