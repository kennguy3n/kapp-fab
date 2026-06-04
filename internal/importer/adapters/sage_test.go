package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// sageItems builds n stub Sage resource objects starting at startIdx.
func sageItems(n, startIdx int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		id := startIdx + i
		out = append(out, map[string]any{
			"id":           fmt.Sprintf("s-%d", id),
			"displayed_as": fmt.Sprintf("Contact %d", id),
		})
	}
	return out
}

func TestSageDiscoverReadsTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sage-tok" {
			t.Errorf("missing bearer")
		}
		// Discover requests items_per_page=1.
		if r.URL.Query().Get("items_per_page") != "1" {
			t.Errorf("discover should request items_per_page=1, got %q", r.URL.Query().Get("items_per_page"))
		}
		_ = json.NewEncoder(w).Encode(sageList{Total: 42, Page: 1, ItemsPerPage: 1, Items: sageItems(1, 0)})
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(SageConfig{
		BaseURL:     srv.URL,
		AccessToken: "sage-tok",
		Entities:    []SageEntity{{Name: "contacts", TargetKType: "crm.contact"}},
	})
	a := NewSageAdapter().WithHTTPClient(srv.Client())
	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if disco.TotalRows != 42 || disco.Entities[0].RowCount != 42 {
		t.Fatalf("expected 42 from $total, got %+v", disco.Entities)
	}
}

func TestSageExportPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		perPage, _ := strconv.Atoi(r.URL.Query().Get("items_per_page"))
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case 1:
			// Full page + a $next pointer -> adapter fetches page 2.
			_ = json.NewEncoder(w).Encode(sageList{
				Total: int64(perPage + 1), Page: 1, ItemsPerPage: perPage,
				Next: "/contacts?page=2", Items: sageItems(perPage, 0),
			})
		default:
			_ = json.NewEncoder(w).Encode(sageList{
				Total: int64(perPage + 1), Page: page, ItemsPerPage: perPage,
				Items: sageItems(1, 1000),
			})
		}
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(SageConfig{
		BaseURL:     srv.URL,
		AccessToken: "sage-tok",
		PageSize:    2,
		Entities:    []SageEntity{{Name: "contacts"}},
	})
	a := NewSageAdapter().WithHTTPClient(srv.Client())

	var got []importer.NormalizedRow
	if err := a.Export(context.Background(), cfg, func(row importer.NormalizedRow) error {
		got = append(got, row)
		return nil
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("exported %d rows, want 3", len(got))
	}
	if got[0].SourceID != "s-0" || got[0].Data["name"] != "Contact 0" {
		t.Errorf("displayed_as not mapped to name: %+v", got[0])
	}
}

func TestSageRefreshTokenGrantBodyCreds(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		// Sage carries client credentials in the body, not Basic auth.
		if _, _, ok := r.BasicAuth(); ok {
			t.Error("Sage should send client creds in the body, not Basic auth")
		}
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret" {
			t.Errorf("client creds missing from body: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(oauth2Token{AccessToken: "sage-tok", RefreshToken: "r2", ExpiresIn: 3600})
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sageList{Total: 0, Items: []map[string]any{}})
	}))
	defer apiSrv.Close()

	cfg, _ := json.Marshal(SageConfig{
		BaseURL:      apiSrv.URL,
		RefreshToken: "r1",
		ClientID:     "cid",
		ClientSecret: "csecret",
		TokenURL:     tokenSrv.URL,
		Entities:     []SageEntity{{Name: "products"}},
	})
	a := NewSageAdapter().WithHTTPClient(apiSrv.Client())
	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(disco.Notes) == 0 || !strings.Contains(disco.Notes[0], "rotated") {
		t.Errorf("expected rotation note, got %v", disco.Notes)
	}
}

func TestSageDeltaParam(t *testing.T) {
	var sawSince bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updated_or_created_since") != "" {
			sawSince = true
		}
		_ = json.NewEncoder(w).Encode(sageList{Total: 0})
	}))
	defer srv.Close()

	cfg, _ := json.Marshal(SageConfig{
		BaseURL:     srv.URL,
		AccessToken: "sage-tok",
		Entities:    []SageEntity{{Name: "sales_invoices"}},
		LastSyncAt:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	a := NewSageAdapter().WithHTTPClient(srv.Client())
	if _, err := a.Discover(context.Background(), cfg); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !sawSince {
		t.Error("expected updated_or_created_since param when LastSyncAt set")
	}
}

func TestSageConfigValidation(t *testing.T) {
	a := NewSageAdapter()
	raw, _ := json.Marshal(SageConfig{})
	if _, err := a.Discover(context.Background(), raw); err == nil {
		t.Error("expected error when no credentials supplied")
	}
}
