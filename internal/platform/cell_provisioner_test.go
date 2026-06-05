package platform

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNoopProvisioner_Provision(t *testing.T) {
	p := NewNoopProvisioner(nil)
	cell, err := p.Provision(context.Background(), "us-east-1", CellSpec{MaxTenants: 500})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if cell == nil || cell.ID == "" {
		t.Fatalf("want non-empty cell id, got %#v", cell)
	}
	if cell.Region != "us-east-1" {
		t.Errorf("region = %q, want us-east-1", cell.Region)
	}
	if cell.MaxTenants != 500 {
		t.Errorf("max_tenants = %d, want 500", cell.MaxTenants)
	}
	if !strings.HasPrefix(cell.ID, "cell-us-east-1-") {
		t.Errorf("id %q should embed sanitized region", cell.ID)
	}
}

func TestNoopProvisioner_DeprovisionAndStatus(t *testing.T) {
	p := NewNoopProvisioner(nil)
	if err := p.Deprovision(context.Background(), "cell-x"); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	st, err := p.Status(context.Background(), "cell-x")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != CellStateUnknown {
		t.Errorf("state = %q, want unknown", st.State)
	}
	if st.CellID != "cell-x" {
		t.Errorf("cell_id = %q, want cell-x", st.CellID)
	}
}

func TestProvisionerFromEnv(t *testing.T) {
	t.Run("default noop", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISIONER", "")
		p, err := ProvisionerFromEnv(nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if _, ok := p.(*NoopProvisioner); !ok {
			t.Fatalf("want *NoopProvisioner, got %T", p)
		}
	})
	t.Run("script", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISIONER", "script")
		p, err := ProvisionerFromEnv(nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if _, ok := p.(*ScriptProvisioner); !ok {
			t.Fatalf("want *ScriptProvisioner, got %T", p)
		}
	})
	t.Run("webhook requires url", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISIONER", "webhook")
		t.Setenv("KAPP_AUTOSCALE_WEBHOOK_URL", "")
		if _, err := ProvisionerFromEnv(nil); err == nil {
			t.Fatal("want error when webhook url missing")
		}
	})
	t.Run("webhook", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISIONER", "webhook")
		t.Setenv("KAPP_AUTOSCALE_WEBHOOK_URL", "https://example.invalid/hook")
		p, err := ProvisionerFromEnv(nil)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if _, ok := p.(*WebhookProvisioner); !ok {
			t.Fatalf("want *WebhookProvisioner, got %T", p)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISIONER", "bogus")
		if _, err := ProvisionerFromEnv(nil); err == nil {
			t.Fatal("want error for unknown provisioner")
		}
	})
}

func TestAutoscaleProvisioningEnabled(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISION", "")
		if AutoscaleProvisioningEnabled() {
			t.Fatal("want disabled by default")
		}
	})
	t.Run("on", func(t *testing.T) {
		t.Setenv("KAPP_AUTOSCALE_PROVISION", "true")
		if !AutoscaleProvisioningEnabled() {
			t.Fatal("want enabled")
		}
	})
}

func TestScriptProvisioner_Provision(t *testing.T) {
	p := NewScriptProvisioner("/fake/provision-cell.sh", time.Minute, nil)
	var gotArgs []string
	p.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "/fake/provision-cell.sh" {
			t.Errorf("script = %q", name)
		}
		gotArgs = args
		return []byte("starting...\n{\"id\":\"cell-eu-1\",\"region\":\"eu-west-1\",\"endpoint\":\"https://eu.cell\",\"max_tenants\":800}\n"), nil
	}
	cell, err := p.Provision(context.Background(), "eu-west-1", CellSpec{MaxTenants: 800, Provider: "aws", Zone: "eu-west-1a"})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if cell.ID != "cell-eu-1" || cell.Endpoint != "https://eu.cell" || cell.MaxTenants != 800 {
		t.Fatalf("unexpected cell: %#v", cell)
	}
	want := []string{"provision", "eu-west-1", "800", "aws", "eu-west-1a"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", gotArgs, want)
	}
}

func TestScriptProvisioner_ProvisionBackfillsRegion(t *testing.T) {
	p := NewScriptProvisioner("s.sh", time.Minute, nil)
	p.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		// Script omits region and max_tenants; provisioner backfills.
		return []byte(`{"id":"cell-1"}`), nil
	}
	cell, err := p.Provision(context.Background(), "ap-south-1", CellSpec{MaxTenants: 250})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if cell.Region != "ap-south-1" {
		t.Errorf("region = %q, want backfilled ap-south-1", cell.Region)
	}
	if cell.MaxTenants != 250 {
		t.Errorf("max_tenants = %d, want backfilled 250", cell.MaxTenants)
	}
}

func TestScriptProvisioner_DeprovisionAndStatus(t *testing.T) {
	p := NewScriptProvisioner("s.sh", time.Minute, nil)
	var calls [][]string
	p.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "status" {
			return []byte(`{"state":"ready","message":"ok"}`), nil
		}
		return nil, nil
	}
	if err := p.Deprovision(context.Background(), "cell-9"); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	st, err := p.Status(context.Background(), "cell-9")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != CellStateReady || st.CellID != "cell-9" {
		t.Errorf("unexpected status: %#v", st)
	}
	if len(calls) != 2 || calls[0][0] != "deprovision" || calls[1][0] != "status" {
		t.Errorf("calls = %v", calls)
	}
}

func TestScriptProvisioner_ErrorPropagates(t *testing.T) {
	p := NewScriptProvisioner("s.sh", time.Minute, nil)
	p.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, io.ErrUnexpectedEOF
	}
	if _, err := p.Provision(context.Background(), "r", CellSpec{}); err == nil {
		t.Fatal("want error")
	}
	if err := p.Deprovision(context.Background(), "c"); err == nil {
		t.Fatal("want error")
	}
}

func TestWebhookProvisioner_Provision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req webhookRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Action != "provision" || req.Region != "us-west-2" {
			t.Errorf("unexpected request: %#v", req)
		}
		if req.Spec == nil || req.Spec.MaxTenants != 1000 {
			t.Errorf("unexpected spec: %#v", req.Spec)
		}
		_ = json.NewEncoder(w).Encode(webhookResponse{Cell: &Cell{ID: "cell-w2", MaxTenants: 1000}})
	}))
	defer srv.Close()

	p := NewWebhookProvisioner(srv.URL, 5*time.Second, nil)
	cell, err := p.Provision(context.Background(), "us-west-2", CellSpec{MaxTenants: 1000})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if cell.ID != "cell-w2" || cell.Region != "us-west-2" {
		t.Fatalf("unexpected cell: %#v", cell)
	}
}

func TestWebhookProvisioner_StatusAndDeprovision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req webhookRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Action {
		case "status":
			_ = json.NewEncoder(w).Encode(webhookResponse{Status: &CellProvisionStatus{State: CellStateDraining}})
		case "deprovision":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected action %q", req.Action)
		}
	}))
	defer srv.Close()

	p := NewWebhookProvisioner(srv.URL, 5*time.Second, nil)
	if err := p.Deprovision(context.Background(), "cell-z"); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	st, err := p.Status(context.Background(), "cell-z")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != CellStateDraining || st.CellID != "cell-z" {
		t.Errorf("unexpected status: %#v", st)
	}
}

func TestWebhookProvisioner_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := NewWebhookProvisioner(srv.URL, 5*time.Second, nil)
	if _, err := p.Provision(context.Background(), "r", CellSpec{}); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestWebhookProvisioner_MissingCellID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(webhookResponse{Cell: &Cell{ID: ""}})
	}))
	defer srv.Close()
	p := NewWebhookProvisioner(srv.URL, 5*time.Second, nil)
	if _, err := p.Provision(context.Background(), "r", CellSpec{}); err == nil {
		t.Fatal("want error when response missing cell.id")
	}
}

func TestParseCellJSON_LastLineTolerance(t *testing.T) {
	out := []byte("creating vpc\ncreating rds\n{\"id\":\"cell-1\",\"region\":\"r\"}\n")
	c, err := parseCellJSON(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.ID != "cell-1" {
		t.Errorf("id = %q", c.ID)
	}
}

func TestParseCellJSON_Errors(t *testing.T) {
	if _, err := parseCellJSON([]byte("no json here")); err == nil {
		t.Error("want error for non-json output")
	}
	if _, err := parseCellJSON([]byte(`{"region":"r"}`)); err == nil {
		t.Error("want error for missing id")
	}
}

func TestSanitizeRegion(t *testing.T) {
	cases := map[string]string{
		"us-east-1": "us-east-1",
		"US East 1": "us-east-1",
		"":          "default",
		"  ":        "default",
		"eu_west_1": "eu-west-1",
		"!!!":       "default",
		"-us-east-": "us-east",
	}
	for in, want := range cases {
		if got := sanitizeRegion(in); got != want {
			t.Errorf("sanitizeRegion(%q) = %q, want %q", in, got, want)
		}
	}
}
