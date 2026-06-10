package bankfeed

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
