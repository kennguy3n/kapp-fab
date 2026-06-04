package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// qboServer is a minimal QuickBooks Online query-API stub. It answers
// COUNT(*) queries with a fixed total and SELECT * queries with a
// single page of rows, echoing the entity name in the QueryResponse
// envelope the way the real API does.
func qboServer(t *testing.T, rows map[string][]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-123" {
			t.Errorf("missing/incorrect bearer: %q", got)
		}
		query := r.URL.Query().Get("query")
		entity := entityFromFrom(query)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(query, "COUNT(*)") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"QueryResponse": map[string]any{"totalCount": len(rows[entity])},
			})
			return
		}
		// STARTPOSITION 1 returns the rows; any higher start returns an
		// empty envelope so the adapter's pagination loop terminates.
		start := afterToken(query, "STARTPOSITION ")
		body := map[string]any{"QueryResponse": map[string]any{}}
		if start == "1" {
			body["QueryResponse"] = map[string]any{entity: rows[entity]}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
}

func entityFromFrom(query string) string {
	const tok = "FROM "
	i := strings.Index(query, tok)
	if i < 0 {
		return ""
	}
	rest := query[i+len(tok):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

func afterToken(s, tok string) string {
	i := strings.Index(s, tok)
	if i < 0 {
		return ""
	}
	rest := s[i+len(tok):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestQuickBooksDiscoverAndExport(t *testing.T) {
	rows := map[string][]map[string]any{
		"Customer": {
			{"Id": "1", "DisplayName": "Acme Inc", "CompanyName": "Acme"},
			{"Id": "2", "DisplayName": "Globex"},
		},
	}
	srv := qboServer(t, rows)
	defer srv.Close()

	cfg, _ := json.Marshal(QuickBooksConfig{
		BaseURL:     srv.URL,
		RealmID:     "9999",
		AccessToken: "access-123",
		Entities:    []QuickBooksEntity{{Name: "Customer", TargetKType: "crm.customer"}},
	})
	a := NewQuickBooksAdapter().WithHTTPClient(srv.Client())

	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if disco.TotalRows != 2 {
		t.Fatalf("TotalRows = %d, want 2", disco.TotalRows)
	}
	if len(disco.Entities) != 1 || disco.Entities[0].RowCount != 2 || disco.Entities[0].TargetKT != "crm.customer" {
		t.Fatalf("unexpected entities: %+v", disco.Entities)
	}

	var got []importer.NormalizedRow
	if err := a.Export(context.Background(), cfg, func(row importer.NormalizedRow) error {
		got = append(got, row)
		return nil
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("exported %d rows, want 2", len(got))
	}
	first := got[0]
	if first.SourceID != "1" {
		t.Errorf("SourceID = %q, want 1", first.SourceID)
	}
	if first.Data["name"] != "Acme Inc" {
		t.Errorf("DisplayName not mapped to name: %+v", first.Data)
	}
	if first.Data["company"] != "Acme" {
		t.Errorf("CompanyName not mapped to company: %+v", first.Data)
	}
	if _, ok := first.Data["DisplayName"]; ok {
		t.Errorf("source field DisplayName should have been renamed, got %+v", first.Data)
	}
}

func TestQuickBooksRefreshTokenGrant(t *testing.T) {
	var tokenHits int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-abc" {
			t.Errorf("unexpected token form: %v", r.Form)
		}
		// QuickBooks uses HTTP Basic auth for client credentials.
		if user, pass, ok := r.BasicAuth(); !ok || user != "client-id" || pass != "client-secret" {
			t.Errorf("expected basic auth client creds, got user=%q ok=%v", user, ok)
		}
		_ = json.NewEncoder(w).Encode(oauth2Token{
			AccessToken:  "access-123",
			RefreshToken: "refresh-rotated",
			TokenType:    "bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokenSrv.Close()

	apiSrv := qboServer(t, map[string][]map[string]any{"Item": {{"Id": "10", "Name": "Widget"}}})
	defer apiSrv.Close()

	cfg, _ := json.Marshal(QuickBooksConfig{
		BaseURL:      apiSrv.URL,
		RealmID:      "9999",
		RefreshToken: "refresh-abc",
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		TokenURL:     tokenSrv.URL,
		Entities:     []QuickBooksEntity{{Name: "Item"}},
	})
	a := NewQuickBooksAdapter().WithHTTPClient(apiSrv.Client())

	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tokenHits == 0 {
		t.Fatal("expected a token refresh call")
	}
	if len(disco.Notes) == 0 || !strings.Contains(disco.Notes[0], "refresh token rotated") {
		t.Errorf("expected rotation note, got %v", disco.Notes)
	}
}

func TestQuickBooksDeltaFilter(t *testing.T) {
	var sawWhere bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("query"), "Metadata.LastUpdatedTime >") {
			sawWhere = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"QueryResponse": map[string]any{"totalCount": 0}})
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(QuickBooksConfig{
		BaseURL:     srv.URL,
		RealmID:     "9999",
		AccessToken: "access-123",
		Entities:    []QuickBooksEntity{{Name: "Invoice"}},
		LastSyncAt:  time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	a := NewQuickBooksAdapter().WithHTTPClient(srv.Client())
	if _, err := a.Discover(context.Background(), cfg); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !sawWhere {
		t.Error("expected LastSyncAt to add a Metadata.LastUpdatedTime WHERE clause")
	}
}

func TestQuickBooksConfigValidation(t *testing.T) {
	a := NewQuickBooksAdapter()
	cases := map[string]QuickBooksConfig{
		"missing realm":       {AccessToken: "x"},
		"missing credentials": {RealmID: "1"},
		"bad entity name":     {RealmID: "1", AccessToken: "x", Entities: []QuickBooksEntity{{Name: "Invoice; DROP"}}},
	}
	for name, c := range cases {
		raw, _ := json.Marshal(c)
		if _, err := a.Discover(context.Background(), raw); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
