package adapters

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateBodyValidUTF8 ensures the error-body truncation never
// splits a multi-byte UTF-8 character, so the result is always valid
// UTF-8 even when the 512-byte cut falls mid-rune.
func TestTruncateBodyValidUTF8(t *testing.T) {
	// "é" is two bytes (0xC3 0xA9). A run of 511 ASCII bytes followed by
	// "é" puts the 512-byte boundary in the middle of the rune.
	body := strings.Repeat("a", 511) + "é" + strings.Repeat("b", 100)
	got := truncateBody([]byte(body))
	if !utf8.ValidString(got) {
		t.Fatalf("truncateBody produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got[len(got)-4:])
	}
}

// TestTruncateBodyShort leaves short bodies untouched (aside from
// trimming surrounding whitespace).
func TestTruncateBodyShort(t *testing.T) {
	if got := truncateBody([]byte("  hello  ")); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestReadCappedBody rejects a body that exceeds the in-memory cap
// rather than buffering it whole.
func TestReadCappedBody(t *testing.T) {
	if _, err := readCappedBody(strings.NewReader(strings.Repeat("x", maxResponseBytes+10))); err == nil {
		t.Error("expected error for oversized body")
	}
	if got, err := readCappedBody(strings.NewReader("small")); err != nil || string(got) != "small" {
		t.Errorf("readCappedBody small: got %q err %v", got, err)
	}
}
