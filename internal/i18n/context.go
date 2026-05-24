package i18n

import "context"

// ctxKey is an unexported type to keep i18n's context key out of
// collision range with any other package using context.WithValue.
// The empty struct generates a zero-byte unique key.
type ctxKey struct{}

// localeKey is the package-private context key for the per-request
// locale tag. The string value is intentionally never compared by
// other packages — they use FromContext / WithLocale instead.
var localeKey = ctxKey{}

// WithLocale returns a copy of ctx carrying the resolved locale
// tag. The tag is expected to be a value returned by
// Bundle.Resolve, i.e. one that exists in Bundle.Supported().
// Storing an arbitrary string here will not break callers — T
// handles unknown locales by falling back to English — but it will
// defeat the matcher's calibration, so prefer to go through Resolve.
func WithLocale(ctx context.Context, locale string) context.Context {
	if locale == "" {
		locale = DefaultLocale
	}
	return context.WithValue(ctx, localeKey, locale)
}

// FromContext returns the locale tag previously attached via
// WithLocale, or DefaultLocale ("en") when no tag was set. This is
// the canonical way for any handler to discover which translation
// catalogue to use for the in-flight request — the Accept-Language
// middleware populates the value once at the edge so deeper code
// doesn't re-parse the header on every translation.
func FromContext(ctx context.Context) string {
	if ctx == nil {
		return DefaultLocale
	}
	if v, ok := ctx.Value(localeKey).(string); ok && v != "" {
		return v
	}
	return DefaultLocale
}
