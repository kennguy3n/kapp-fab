package i18n

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestFrontendBackendCatalogueParity proves the frontend and
// backend ship the same locale set and the same keys for every
// locale. Drift between the two halves is the canonical source
// of "this string is translated on the server-rendered email
// but renders as a literal key in the SPA" bugs and the inverse
// (translated in the SPA, English in the API response).
//
// The parity contract:
//
//  1. The set of *.json files in internal/i18n/locales/ MUST be
//     IDENTICAL to the set of *.json files in
//     apps/web/src/locales/ (no extra files on either side).
//  2. For every locale, the KEYSET in the backend catalogue MUST
//     be identical to the KEYSET in the frontend catalogue. Value
//     drift (different translations of the same key) is OK and
//     expected during translator turnaround — but missing keys
//     would either silently fall back to English on the rendering
//     side or render as the literal key, both of which are
//     production-visible bugs.
//
// Value-string drift is NOT enforced here on purpose:
//
//   - The backend's English baseline is the source of truth for
//     copy strings; the frontend mirrors the keys but may iterate
//     translation values asynchronously through the localisation
//     vendor's queue. Enforcing value parity would block every
//     frontend-side typo fix on a backend re-deploy.
//   - The "Has X key for locale Y" contract is enough to keep the
//     UX consistent — both halves can render the same translation
//     OR the same fallback for every (locale, key) pair.
//
// The test runs on every `go test ./...` invocation. If the
// frontend locales directory doesn't exist (e.g. a partial
// checkout, a stacked PR that doesn't include the frontend yet)
// the test SKIPS rather than fails, with an explanatory message.
// Once both halves are merged together to main the skip path is
// no longer reachable on production CI.
func TestFrontendBackendCatalogueParity(t *testing.T) {
	backendDir := "locales"
	frontendDir := findFrontendLocalesDir(t)
	if frontendDir == "" {
		t.Skip("apps/web/src/locales/ not present on this checkout — skipping cross-half parity check (will run once PR-5 lands)")
	}

	backend := loadCatalogueSet(t, os.DirFS(backendDir), "backend")
	frontend := loadCatalogueSet(t, os.DirFS(frontendDir), "frontend")

	// (1) Same locale-tag set on both sides.
	backendTags := sortedKeys(backend)
	frontendTags := sortedKeys(frontend)
	if diff := setDiff(backendTags, frontendTags); diff != "" {
		t.Fatalf("locale set mismatch between backend and frontend:\n%s", diff)
	}

	// (2) Same keyset per locale.
	for _, tag := range backendTags {
		t.Run(tag, func(t *testing.T) {
			bKeys := sortedKeys(backend[tag])
			fKeys := sortedKeys(frontend[tag])
			if diff := setDiff(bKeys, fKeys); diff != "" {
				t.Fatalf("locale %q keyset mismatch between backend and frontend:\n%s", tag, diff)
			}
		})
	}
}

// findFrontendLocalesDir walks up from the test's working
// directory looking for `apps/web/src/locales`. Returns "" if
// not found (the caller skips the test in that case).
func findFrontendLocalesDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	dir := cwd
	for i := 0; i < 16; i++ {
		candidate := filepath.Join(dir, "apps", "web", "src", "locales")
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// loadCatalogueSet reads every <tag>.json file under fsys into a
// map keyed by locale tag. Each value is the parsed catalogue
// (flat string→string map). label is used only for error
// messages so the caller can distinguish frontend vs backend.
func loadCatalogueSet(t *testing.T, fsys fs.FS, label string) map[string]map[string]string {
	t.Helper()
	out := make(map[string]map[string]string)
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("%s: read locales dir: %v", label, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		tag := strings.TrimSuffix(name, ".json")
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatalf("%s/%s: read: %v", label, name, err)
		}
		var cat map[string]string
		if err := json.Unmarshal(data, &cat); err != nil {
			// The frontend may use a non-flat catalogue shape in
			// the future; surface the parse error explicitly so
			// the parity check fails loudly rather than silently
			// counting zero keys.
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				t.Fatalf("%s/%s: invalid JSON at offset %d: %v", label, name, syntax.Offset, err)
			}
			t.Fatalf("%s/%s: unmarshal: %v", label, name, err)
		}
		out[tag] = cat
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// setDiff returns "" if a and b contain the same strings, or a
// multi-line description of the missing items on each side.
// Both slices are assumed to be sorted ascending.
func setDiff(a, b []string) string {
	aSet := make(map[string]struct{}, len(a))
	for _, x := range a {
		aSet[x] = struct{}{}
	}
	bSet := make(map[string]struct{}, len(b))
	for _, x := range b {
		bSet[x] = struct{}{}
	}
	var missingFromB, missingFromA []string
	for _, x := range a {
		if _, ok := bSet[x]; !ok {
			missingFromB = append(missingFromB, x)
		}
	}
	for _, x := range b {
		if _, ok := aSet[x]; !ok {
			missingFromA = append(missingFromA, x)
		}
	}
	if len(missingFromA) == 0 && len(missingFromB) == 0 {
		return ""
	}
	var b2 strings.Builder
	if len(missingFromB) > 0 {
		b2.WriteString("  missing from frontend (present in backend):\n    ")
		b2.WriteString(strings.Join(missingFromB, "\n    "))
		b2.WriteString("\n")
	}
	if len(missingFromA) > 0 {
		b2.WriteString("  missing from backend (present in frontend):\n    ")
		b2.WriteString(strings.Join(missingFromA, "\n    "))
		b2.WriteString("\n")
	}
	return b2.String()
}
