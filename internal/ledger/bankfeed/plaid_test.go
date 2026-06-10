package bankfeed

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestNewPlaidProviderNilWhenUnconfigured(t *testing.T) {
	if p := NewPlaidProvider(PlaidConfig{}, nil); p != nil {
		t.Fatal("expected nil provider when credentials absent")
	}
	if p := NewPlaidProvider(PlaidConfig{ClientID: "id"}, nil); p != nil {
		t.Fatal("expected nil provider when secret missing")
	}
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "sec"}, nil)
	if p == nil || p.Name() != ProviderPlaid {
		t.Fatalf("expected configured plaid provider, got %v", p)
	}
	if p.cfg.ClientName != "KApp" || len(p.cfg.CountryCodes) == 0 {
		t.Errorf("expected defaults applied: %+v", p.cfg)
	}
}

func TestPlaidInitiateConnect(t *testing.T) {
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/link/token/create") {
			t.Errorf("path = %s", req.URL.Path)
		}
		return jsonResponse(200, `{"link_token":"link-sandbox-123"}`), nil
	})
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, doer)
	tok, err := p.InitiateConnect(context.Background(), uuid.New(), uuid.New(), "https://cb")
	if err != nil {
		t.Fatalf("InitiateConnect: %v", err)
	}
	if tok != "link-sandbox-123" {
		t.Fatalf("token = %q", tok)
	}
}

func TestPlaidInitiateConnectEmptyToken(t *testing.T) {
	doer := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"link_token":""}`), nil
	})
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, doer)
	if _, err := p.InitiateConnect(context.Background(), uuid.New(), uuid.New(), ""); err == nil {
		t.Fatal("expected error on empty link_token")
	}
}

func TestPlaidCompleteConnect(t *testing.T) {
	doer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/item/public_token/exchange") {
			t.Errorf("path = %s", req.URL.Path)
		}
		return jsonResponse(200, `{"access_token":"access-123","item_id":"item-9"}`), nil
	})
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, doer)
	tn := uuid.New()
	conn, err := p.CompleteConnect(context.Background(), tn, "public-token")
	if err != nil {
		t.Fatalf("CompleteConnect: %v", err)
	}
	if conn.AccessToken != "access-123" || conn.ExternalID != "item-9" {
		t.Fatalf("conn = %+v", conn)
	}
	if conn.TenantID != tn || conn.Provider != ProviderPlaid || conn.Status != StatusActive {
		t.Fatalf("conn metadata = %+v", conn)
	}
}

func TestPlaidCompleteConnectRequiresCode(t *testing.T) {
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, nil)
	if _, err := p.CompleteConnect(context.Background(), uuid.New(), ""); err == nil {
		t.Fatal("expected error when public_token empty")
	}
}

func TestPlaidFetchTransactionsPaginatesAndNegates(t *testing.T) {
	page := 0
	doer := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		page++
		if page == 1 {
			return jsonResponse(200, `{
				"added":[{"transaction_id":"t1","date":"2024-01-15","name":"Coffee","merchant_name":"Cafe","amount":4.50,"iso_currency_code":"USD","pending":false}],
				"next_cursor":"c2","has_more":true}`), nil
		}
		return jsonResponse(200, `{
			"added":[{"transaction_id":"t2","date":"2024-01-16","name":"","merchant_name":"Refund Co","amount":-20,"iso_currency_code":"USD"}],
			"next_cursor":"c3","has_more":false}`), nil
	})
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, doer)
	txns, cursor, err := p.FetchTransactions(context.Background(), &Connection{AccessToken: "a"}, time.Time{})
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if cursor != "c3" {
		t.Fatalf("cursor = %q; want c3", cursor)
	}
	if len(txns) != 2 {
		t.Fatalf("got %d txns; want 2", len(txns))
	}
	// Plaid positive (money out) negates to our negative convention.
	if !txns[0].Amount.Equal(decimal.RequireFromString("-4.50")) {
		t.Errorf("txn0 amount = %s; want -4.50", txns[0].Amount)
	}
	// Negative Plaid (money in) becomes positive.
	if !txns[1].Amount.Equal(decimal.RequireFromString("20")) {
		t.Errorf("txn1 amount = %s; want 20", txns[1].Amount)
	}
	// Empty name falls back to merchant_name.
	if txns[1].Description != "Refund Co" {
		t.Errorf("txn1 desc = %q; want Refund Co", txns[1].Description)
	}
}

func TestPlaidFetchNilConnection(t *testing.T) {
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, nil)
	if _, _, err := p.FetchTransactions(context.Background(), nil, time.Time{}); err == nil {
		t.Fatal("expected error on nil connection")
	}
}

func TestPlaidBaseURLByEnv(t *testing.T) {
	cases := map[string]string{
		"sandbox":     "https://sandbox.plaid.com",
		"development": "https://development.plaid.com",
		"production":  "https://production.plaid.com",
		"":            "https://sandbox.plaid.com",
	}
	for env, want := range cases {
		p := &PlaidProvider{cfg: PlaidConfig{Env: env}}
		if got := p.baseURL(); got != want {
			t.Errorf("env %q => %q; want %q", env, got, want)
		}
	}
}

func TestPlaidDisconnectNoTokenIsNoop(t *testing.T) {
	p := NewPlaidProvider(PlaidConfig{ClientID: "id", Secret: "s", BaseURL: "https://x"}, nil)
	if err := p.Disconnect(context.Background(), &Connection{}); err != nil {
		t.Fatalf("Disconnect with no token should be no-op: %v", err)
	}
}
