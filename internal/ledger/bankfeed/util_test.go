package bankfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Error("empty string should map to nil")
	}
	if v := nullIfEmpty("x"); v != "x" {
		t.Errorf("non-empty = %v; want x", v)
	}
}

// TestTruncateRunesNeverSplitsRune guards the shared helper that backs both
// snippet() and sanitizeErr(): truncation must land on a rune boundary so the
// result is always valid UTF-8 (Postgres rejects invalid UTF-8 in the
// last_error TEXT column, which would silently drop the sync-failure record).
func TestTruncateRunesNeverSplitsRune(t *testing.T) {
	// "é" is 2 bytes (0xC3 0xA9); a cap landing mid-rune must back up.
	base := strings.Repeat("é", 400) // 800 bytes, 400 runes
	for _, maxLen := range []int{0, 1, 2, 3, 99, 100, 101, 499, 500, 501} {
		out := truncateRunes(base, maxLen)
		if !utf8.ValidString(out) {
			t.Fatalf("maxLen=%d produced invalid UTF-8: %q", maxLen, out)
		}
		if len(base) > maxLen && !strings.HasSuffix(out, "…") {
			t.Errorf("maxLen=%d: expected ellipsis on truncation", maxLen)
		}
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Errorf("short string altered: %q", got)
	}
}

// TestSanitizeErrProducesValidUTF8 exercises sanitizeErr with an error whose
// message exceeds the 500-byte cap and is built from multi-byte runes, so a
// naive byte slice would split a codepoint.
func TestSanitizeErrProducesValidUTF8(t *testing.T) {
	long := strings.Repeat("über-café-naïve-", 60) // > 500 bytes, multi-byte
	err := fmt.Errorf("bankfeed: provider rejected: %w", errors.New(long))
	out := sanitizeErr(err)
	if !utf8.ValidString(out) {
		t.Fatalf("sanitizeErr produced invalid UTF-8: %q", out)
	}
	if len(out) > 503 { // 500 bytes + 3-byte ellipsis rune
		t.Errorf("sanitizeErr len = %d; want <= 503", len(out))
	}
}

func TestAuditContextExcludesTokens(t *testing.T) {
	acct := uuid.New()
	c := Connection{
		Provider:      ProviderPlaid,
		BankAccountID: acct,
		Status:        StatusActive,
		ExternalID:    "item-1",
		AccessToken:   "super-secret-token",
		RefreshToken:  "refresh-secret",
	}
	raw := auditContext(c)
	s := string(raw)
	if strings.Contains(s, "super-secret-token") || strings.Contains(s, "refresh-secret") {
		t.Fatalf("audit context leaked a token: %s", s)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("audit context not valid JSON: %v", err)
	}
	if m["provider"] != ProviderPlaid || m["external_id"] != "item-1" {
		t.Errorf("audit context missing non-secret fields: %s", s)
	}
}

func TestNewSyncHandlerWiringAndTypedNil(t *testing.T) {
	// Passing concrete typed nils must normalize so the nil guards in
	// Handle / SyncOne behave (the typed-nil interface pitfall).
	h := NewSyncHandler(nil, nil, NewRegistry(NewCSVProvider()), &fakeStore{}, nil)
	if h.matcher != nil {
		t.Error("nil *SmartMatcher should normalize to nil suggester")
	}
	if h.rules != nil {
		t.Error("nil *RuleStore should normalize to nil ruleLister")
	}
	if h.conns != nil {
		t.Error("nil *ConnectionStore should normalize to nil connStore")
	}
	// A handler missing required collaborators must refuse to Handle.
	if err := h.Handle(context.Background(), uuid.New(), scheduledActionStub()); err == nil {
		t.Error("Handle should error when conns is unwired")
	}
}

func TestPlaidDisconnectWithTokenCallsItemRemove(t *testing.T) {
	called := false
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/item/remove") {
			called = true
		}
		return jsonResponse(200, `{}`), nil
	})
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, doer)
	if err := p.Disconnect(context.Background(), &Connection{AccessToken: "access-1"}); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if !called {
		t.Fatal("expected /item/remove to be called when access token present")
	}
}
