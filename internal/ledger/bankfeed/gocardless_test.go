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

func TestNewGoCardlessProviderNilWhenUnconfigured(t *testing.T) {
	if NewGoCardlessProvider(GoCardlessConfig{}, nil) != nil {
		t.Fatal("expected nil when secret pair absent")
	}
	if NewGoCardlessProvider(GoCardlessConfig{SecretID: "x"}, nil) != nil {
		t.Fatal("expected nil when secret_key missing")
	}
}

// gcDoer serves the token endpoint plus a routing function for the rest.
func gcDoer(_ *testing.T, tokenCalls *int, route func(req *http.Request) (*http.Response, error)) httpDoer {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/token/new/") {
			*tokenCalls++
			return jsonResponse(200, `{"access":"tok-abc","access_expires":3600}`), nil
		}
		return route(req)
	})
}

func TestGoCardlessBearerCaches(t *testing.T) {
	calls := 0
	doer := gcDoer(t, &calls, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{}`), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", BaseURL: "https://x"}, doer)
	clock := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if _, err := p.bearer(context.Background()); err != nil {
			t.Fatalf("bearer: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("token fetched %d times; want 1 (cached)", calls)
	}
}

func TestGoCardlessInitiateConnectRequiresInstitution(t *testing.T) {
	calls := 0
	doer := gcDoer(t, &calls, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{}`), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", BaseURL: "https://x"}, doer)
	if _, err := p.InitiateConnect(context.Background(), uuid.New(), uuid.New(), "https://cb"); err == nil {
		t.Fatal("expected error when institution_id unset")
	}
}

func TestGoCardlessInitiateConnectReturnsLinkWithRequisition(t *testing.T) {
	calls := 0
	doer := gcDoer(t, &calls, func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"req-1","link":"https://bank/consent"}`), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", InstitutionID: "BANK_GB", BaseURL: "https://x"}, doer)
	link, err := p.InitiateConnect(context.Background(), uuid.New(), uuid.New(), "https://cb")
	if err != nil {
		t.Fatalf("InitiateConnect: %v", err)
	}
	if !strings.Contains(link, "https://bank/consent") || !strings.Contains(link, "requisition=req-1") {
		t.Fatalf("link = %q", link)
	}
}

func TestGoCardlessCompleteConnect(t *testing.T) {
	calls := 0
	doer := gcDoer(t, &calls, func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"req-1","accounts":["acc-9"],"status":"LN"}`), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", BaseURL: "https://x"}, doer)
	conn, err := p.CompleteConnect(context.Background(), uuid.New(), "req-1")
	if err != nil {
		t.Fatalf("CompleteConnect: %v", err)
	}
	if conn.AccessToken != "acc-9" || conn.ExternalID != "req-1" {
		t.Fatalf("conn = %+v", conn)
	}
}

func TestGoCardlessCompleteConnectNoAccounts(t *testing.T) {
	calls := 0
	doer := gcDoer(t, &calls, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"id":"req-1","accounts":[],"status":"CR"}`), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", BaseURL: "https://x"}, doer)
	if _, err := p.CompleteConnect(context.Background(), uuid.New(), "req-1"); err == nil {
		t.Fatal("expected error when requisition has no accounts")
	}
}

func TestGoCardlessFetchTransactions(t *testing.T) {
	calls := 0
	doer := gcDoer(t, &calls, func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Path, "/accounts/acc-9/transactions/") {
			t.Errorf("path = %s", req.URL.Path)
		}
		return jsonResponse(200, `{"transactions":{"booked":[
			{"transactionId":"g1","bookingDate":"2024-02-03","transactionAmount":{"amount":"-12.34","currency":"GBP"},"remittanceInformationUnstructured":"TESCO","creditorName":"Tesco"},
			{"transactionId":"g0","bookingDate":"2024-02-01","transactionAmount":{"amount":"5.00","currency":"GBP"},"remittanceInformationUnstructured":"REFUND"},
			{"transactionId":"g2","bookingDate":"2024-02-09","transactionAmount":{"amount":"bad"},"remittanceInformationUnstructured":"skip"}
		]}}`), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", BaseURL: "https://x"}, doer)
	p.now = func() time.Time { return time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC) }
	txns, cursor, err := p.FetchTransactions(context.Background(), &Connection{AccessToken: "acc-9"}, time.Time{})
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("got %d txns; want 2 (malformed amount skipped)", len(txns))
	}
	if !txns[0].Amount.Equal(decimal.RequireFromString("-12.34")) {
		t.Errorf("amount = %s", txns[0].Amount)
	}
	// Cursor is the max booking date among successfully-ingested rows; the
	// malformed g2 (2024-02-09) is skipped so the cursor never advances
	// past an un-ingested line (re-fetched next tick instead of lost).
	if cursor != "2024-02-03" {
		t.Errorf("cursor = %q; want max ingested booking date 2024-02-03", cursor)
	}
}

func TestGoCardlessDisconnectDeletesRequisition(t *testing.T) {
	calls := 0
	deleted := false
	doer := gcDoer(t, &calls, func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/requisitions/req-1/") {
			deleted = true
			return jsonResponse(204, ``), nil
		}
		t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		return jsonResponse(500, ``), nil
	})
	p := NewGoCardlessProvider(GoCardlessConfig{SecretID: "i", SecretKey: "k", BaseURL: "https://x"}, doer)
	if err := p.Disconnect(context.Background(), &Connection{ExternalID: "req-1"}); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if !deleted {
		t.Fatal("expected DELETE on requisition")
	}
}
