package i18n

import (
	"context"
	"testing"
)

func TestFromContext_EmptyReturnsDefault(t *testing.T) {
	if got := FromContext(context.Background()); got != DefaultLocale {
		t.Fatalf("FromContext(empty) = %q, want %q", got, DefaultLocale)
	}
	if got := FromContext(nil); got != DefaultLocale { //nolint:staticcheck // SA1012 — guarded nil return is part of the contract
		t.Fatalf("FromContext(nil) = %q, want %q", got, DefaultLocale)
	}
}

func TestWithLocale_RoundTrip(t *testing.T) {
	ctx := WithLocale(context.Background(), "ja")
	if got, want := FromContext(ctx), "ja"; got != want {
		t.Fatalf("FromContext after WithLocale(ja) = %q, want %q", got, want)
	}
	ctx2 := WithLocale(ctx, "fr")
	if got, want := FromContext(ctx2), "fr"; got != want {
		t.Fatalf("FromContext after rewrap with fr = %q, want %q", got, want)
	}
	// Parent ctx should be unchanged (immutability).
	if got, want := FromContext(ctx), "ja"; got != want {
		t.Fatalf("parent ctx mutated: got %q, want %q", got, want)
	}
}

func TestWithLocale_EmptyCoercedToDefault(t *testing.T) {
	ctx := WithLocale(context.Background(), "")
	if got, want := FromContext(ctx), DefaultLocale; got != want {
		t.Fatalf("FromContext after WithLocale(empty) = %q, want %q", got, want)
	}
}
