package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

func makeContacts(n, startIdx int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		id := startIdx + i
		out = append(out, map[string]any{
			"ContactID": fmt.Sprintf("c-%d", id),
			"Name":      fmt.Sprintf("Contact %d", id),
		})
	}
	return out
}

func TestXeroPaginationAndMapping(t *testing.T) {
	var contactPages, itemHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Xero-Tenant-Id") != "org-1" {
			t.Errorf("missing Xero-Tenant-Id header")
		}
		if r.Header.Get("Authorization") != "Bearer xero-tok" {
			t.Errorf("missing bearer")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/Contacts"):
			contactPages++
			page := r.URL.Query().Get("page")
			if page == "1" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Contacts": makeContacts(xeroPageSize, 0)})
			} else {
				// Page 2: short page -> terminates pagination.
				_ = json.NewEncoder(w).Encode(map[string]any{"Contacts": makeContacts(1, 1000)})
			}
		case strings.HasSuffix(r.URL.Path, "/Items"):
			itemHits++
			if r.URL.Query().Get("page") != "" {
				t.Errorf("Items must not be paginated, got page=%q", r.URL.Query().Get("page"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
				{"ItemID": "i-1", "Code": "SKU1", "Name": "Widget"},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(XeroConfig{
		BaseURL:      srv.URL,
		XeroTenantID: "org-1",
		AccessToken:  "xero-tok",
		Entities: []XeroEntity{
			{Name: "Contacts", TargetKType: "crm.contact"},
			{Name: "Items", TargetKType: "inventory.item"},
		},
	})
	a := NewXeroAdapter().WithHTTPClient(srv.Client())

	var got []importer.NormalizedRow
	if err := a.Export(context.Background(), cfg, func(row importer.NormalizedRow) error {
		got = append(got, row)
		return nil
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// 100 + 1 contacts followed by 1 item.
	if len(got) != xeroPageSize+2 {
		t.Fatalf("exported %d rows, want %d", len(got), xeroPageSize+2)
	}
	if contactPages != 2 {
		t.Errorf("expected 2 contact pages, got %d", contactPages)
	}
	if itemHits != 1 {
		t.Errorf("expected 1 items call, got %d", itemHits)
	}
	// Item field mapping: Code -> sku, Name -> name.
	item := got[len(got)-1]
	if item.SourceID != "i-1" || item.Data["sku"] != "SKU1" || item.Data["name"] != "Widget" {
		t.Errorf("item not mapped: %+v", item)
	}
}

func TestXeroDiscoverUsesSummaryOnly(t *testing.T) {
	var summaryOnlySeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("summaryOnly") == "true" {
			summaryOnlySeen = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Invoices": []map[string]any{
			{"InvoiceID": "inv-1"},
		}})
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(XeroConfig{
		BaseURL:      srv.URL,
		XeroTenantID: "org-1",
		AccessToken:  "xero-tok",
		Entities:     []XeroEntity{{Name: "Invoices"}},
	})
	a := NewXeroAdapter().WithHTTPClient(srv.Client())
	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !summaryOnlySeen {
		t.Error("expected summaryOnly=true on the Invoices discovery pass")
	}
	if disco.TotalRows != 1 {
		t.Errorf("TotalRows = %d, want 1", disco.TotalRows)
	}
}

func TestXeroIfModifiedSinceHeader(t *testing.T) {
	var sawHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("If-Modified-Since")
		_ = json.NewEncoder(w).Encode(map[string]any{"Employees": []map[string]any{}})
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(XeroConfig{
		BaseURL:      srv.URL,
		XeroTenantID: "org-1",
		AccessToken:  "xero-tok",
		Entities:     []XeroEntity{{Name: "Employees"}},
		LastSyncAt:   time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
	})
	a := NewXeroAdapter().WithHTTPClient(srv.Client())
	if _, err := a.Discover(context.Background(), cfg); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if sawHeader == "" || !strings.Contains(sawHeader, "Mon, 06 May 2024") {
		t.Errorf("If-Modified-Since header = %q, want RFC1123 timestamp", sawHeader)
	}
}

func TestXeroRefreshTokenGrant(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); !ok || user != "cid" {
			t.Errorf("expected basic auth client creds")
		}
		_ = json.NewEncoder(w).Encode(oauth2Token{AccessToken: "xero-tok", ExpiresIn: 1800})
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xero-tok" {
			t.Errorf("bearer not propagated from refresh: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{}})
	}))
	defer apiSrv.Close()

	cfg, _ := json.Marshal(XeroConfig{
		BaseURL:      apiSrv.URL,
		XeroTenantID: "org-1",
		RefreshToken: "r1",
		ClientID:     "cid",
		ClientSecret: "csecret",
		TokenURL:     tokenSrv.URL,
		Entities:     []XeroEntity{{Name: "Items"}},
	})
	a := NewXeroAdapter().WithHTTPClient(apiSrv.Client())
	if _, err := a.Discover(context.Background(), cfg); err != nil {
		t.Fatalf("Discover: %v", err)
	}
}

func TestXeroConfigValidation(t *testing.T) {
	a := NewXeroAdapter()
	cases := map[string]XeroConfig{
		"missing tenant":      {AccessToken: "x"},
		"missing credentials": {XeroTenantID: "org-1"},
		"unsupported entity":  {XeroTenantID: "org-1", AccessToken: "x", Entities: []XeroEntity{{Name: "Nope"}}},
	}
	for name, c := range cases {
		raw, _ := json.Marshal(c)
		if _, err := a.Discover(context.Background(), raw); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
