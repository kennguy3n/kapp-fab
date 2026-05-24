package tenant

import "testing"

// fakeLocaleBundle is the minimal implementation of LocaleBundle
// for the setter unit test. The matcher returns DefaultLocale ("en")
// for any candidate not in the whitelist so we can exercise the
// validator-after-resolver path without depending on golang.org/x/
// text/language. The unit test only cares that both gates pick up
// the supplied bundle in one call; the resolver behaviour itself is
// covered in internal/i18n/bundle_test.go.
type fakeLocaleBundle struct {
	supported map[string]struct{}
}

func newFakeLocaleBundle(tags ...string) *fakeLocaleBundle {
	m := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		m[t] = struct{}{}
	}
	return &fakeLocaleBundle{supported: m}
}

func (f *fakeLocaleBundle) IsSupported(tag string) bool {
	_, ok := f.supported[tag]
	return ok
}

func (f *fakeLocaleBundle) Resolve(candidate string) string {
	if _, ok := f.supported[candidate]; ok {
		return candidate
	}
	return "en"
}

// TestWizard_WithLocaleBundle_WiresBothGates is the regression
// pinned by PR-4: a single combined setter must populate both
// localeValidator and localeResolver so a future caller can never
// install a validator without the matching resolver (which would
// reject every IN/CN/TW/HK row even though the matcher would have
// downgraded it cleanly).
func TestWizard_WithLocaleBundle_WiresBothGates(t *testing.T) {
	b := newFakeLocaleBundle("en", "de", "zh", "zh-Hant")
	w := NewWizard(nil).WithLocaleBundle(b)

	if w.localeValidator == nil {
		t.Fatalf("WithLocaleBundle: localeValidator was not wired")
	}
	if w.localeResolver == nil {
		t.Fatalf("WithLocaleBundle: localeResolver was not wired")
	}
	// Both fields must point at the same underlying value — the
	// LocaleBundle contract assumes one source of truth. The two
	// fields are different interface types so a direct comparison
	// won't compile; assert on the concrete pointer instead.
	if vp, _ := w.localeValidator.(*fakeLocaleBundle); vp != b {
		t.Fatalf("WithLocaleBundle: localeValidator should point at the supplied bundle, got %v", w.localeValidator)
	}
	if rp, _ := w.localeResolver.(*fakeLocaleBundle); rp != b {
		t.Fatalf("WithLocaleBundle: localeResolver should point at the supplied bundle, got %v", w.localeResolver)
	}
}

// TestWizard_WithLocaleBundle_NilDetachesBoth confirms passing nil
// clears both fields rather than panicking. Unit tests that want
// the format gate alone after a previous bundle wiring use this
// path.
func TestWizard_WithLocaleBundle_NilDetachesBoth(t *testing.T) {
	b := newFakeLocaleBundle("en", "de")
	w := NewWizard(nil).WithLocaleBundle(b).WithLocaleBundle(nil)

	if w.localeValidator != nil {
		t.Fatalf("WithLocaleBundle(nil): localeValidator should be nil, got %T", w.localeValidator)
	}
	if w.localeResolver != nil {
		t.Fatalf("WithLocaleBundle(nil): localeResolver should be nil, got %T", w.localeResolver)
	}
}

// TestWizard_WithLocaleBundle_FluentChainOrderInsensitive confirms
// the new combined setter composes cleanly with the older
// single-interface setters in either order. A caller that wires
// the bundle then overrides one half (e.g. an integration test
// that wants the real matcher but a stubbed validator) gets the
// override they asked for, not silent re-wiring.
func TestWizard_WithLocaleBundle_FluentChainOrderInsensitive(t *testing.T) {
	bundle := newFakeLocaleBundle("en", "de")
	stubValidator := newFakeValidator("en") // stricter than bundle

	w := NewWizard(nil).
		WithLocaleBundle(bundle).
		WithLocaleValidator(stubValidator)

	if vp, _ := w.localeValidator.(*fakeValidator); vp != stubValidator {
		t.Fatalf("WithLocaleValidator override after WithLocaleBundle should win, got %T", w.localeValidator)
	}
	if rp, _ := w.localeResolver.(*fakeLocaleBundle); rp != bundle {
		t.Fatalf("WithLocaleBundle resolver wiring should survive a later WithLocaleValidator call, got %T", w.localeResolver)
	}
}
