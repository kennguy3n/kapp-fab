package bankfeed

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestCSVProviderConnectLifecycle(t *testing.T) {
	p := NewCSVProvider()
	if p.Name() != ProviderCSV {
		t.Fatalf("name = %q", p.Name())
	}
	tok, err := p.InitiateConnect(context.Background(), uuid.New(), uuid.New(), "")
	if err != nil || tok != "" {
		t.Fatalf("InitiateConnect = (%q,%v); want empty handshake", tok, err)
	}
	tn := uuid.New()
	conn, err := p.CompleteConnect(context.Background(), tn, "")
	if err != nil || conn.TenantID != tn || conn.Provider != ProviderCSV {
		t.Fatalf("CompleteConnect = (%+v,%v)", conn, err)
	}
	// FetchTransactions is a no-op that preserves the cursor.
	txns, cursor, err := p.FetchTransactions(context.Background(), &Connection{Cursor: "keep"}, conn.CreatedAt)
	if err != nil || len(txns) != 0 || cursor != "keep" {
		t.Fatalf("FetchTransactions = (%v,%q,%v)", txns, cursor, err)
	}
	if err := p.Disconnect(context.Background(), conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestCSVIngest(t *testing.T) {
	csv := "value_date,description,amount,currency,counterparty\n" +
		"2024-01-15,\"Coffee, large\",-4.50,USD,Cafe\n" +
		"01/02/2024,Salary,\"1,200.00\",,Employer\n"
	p := NewCSVProvider()
	txns, err := p.Ingest(strings.NewReader(csv), "GBP")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("got %d rows; want 2", len(txns))
	}
	// Quoted field with embedded comma preserved.
	if txns[0].Description != "Coffee, large" {
		t.Errorf("desc = %q", txns[0].Description)
	}
	if txns[0].Currency != "USD" {
		t.Errorf("currency = %q; want USD", txns[0].Currency)
	}
	// Thousands separator stripped; default currency applied.
	if !txns[1].Amount.Equal(decimal.RequireFromString("1200.00")) {
		t.Errorf("amount = %s; want 1200.00", txns[1].Amount)
	}
	if txns[1].Currency != "GBP" {
		t.Errorf("currency = %q; want default GBP", txns[1].Currency)
	}
}

func TestCSVIngestMissingColumns(t *testing.T) {
	p := NewCSVProvider()
	if _, err := p.Ingest(strings.NewReader("date,memo\n2024-01-01,x\n"), "USD"); err == nil {
		t.Fatal("expected error when required columns absent")
	}
}

func TestCSVIngestEmptyIsNoError(t *testing.T) {
	p := NewCSVProvider()
	txns, err := p.Ingest(strings.NewReader("value_date,description,amount\n"), "USD")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(txns) != 0 {
		t.Fatalf("got %d; want 0", len(txns))
	}
}

func TestCSVIngestBadAmount(t *testing.T) {
	p := NewCSVProvider()
	_, err := p.Ingest(strings.NewReader("value_date,description,amount\n2024-01-01,x,notanumber\n"), "USD")
	if err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatalf("err = %v; want amount parse error", err)
	}
}

func TestParseCSVDateFormats(t *testing.T) {
	for _, s := range []string{"2024-01-15", "15/01/2024", "2024/01/15"} {
		if _, err := parseCSVDate(s); err != nil {
			t.Errorf("parseCSVDate(%q): %v", s, err)
		}
	}
	if _, err := parseCSVDate("nonsense"); err == nil {
		t.Error("expected error for unparseable date")
	}
}
