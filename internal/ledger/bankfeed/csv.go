package bankfeed

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// CSVProvider adapts the pre-existing manual CSV upload to the unified
// Provider interface so a CSV-fed bank account looks like any other feed
// in the UI and the sync pipeline (rules + matcher) runs identically.
//
// CSV is a push provider, not a pull one: there is no live endpoint to
// poll, so FetchTransactions is a no-op and the actual lines arrive via
// Ingest (called by the upload route). InitiateConnect/CompleteConnect
// create a credential-less connection so the account shows as "connected
// (CSV)" and Disconnect simply revokes it locally.
type CSVProvider struct{}

// NewCSVProvider returns the stateless CSV provider.
func NewCSVProvider() *CSVProvider { return &CSVProvider{} }

// Name implements Provider.
func (p *CSVProvider) Name() string { return ProviderCSV }

// InitiateConnect for CSV returns an empty handshake token: the frontend
// shows a file picker rather than a provider widget. Returning nil error
// (not ErrUnsupported) lets the connect route create the connection row
// so the account is marked CSV-connected.
func (p *CSVProvider) InitiateConnect(_ context.Context, _, _ uuid.UUID, _ string) (string, error) {
	return "", nil
}

// CompleteConnect creates a credential-less CSV connection. The caller
// stamps TenantID/BankAccountID from request context.
func (p *CSVProvider) CompleteConnect(_ context.Context, tenantID uuid.UUID, _ string) (*Connection, error) {
	return &Connection{
		TenantID: tenantID,
		Provider: ProviderCSV,
		Status:   StatusActive,
	}, nil
}

// FetchTransactions is a no-op for CSV — lines are pushed via Ingest, not
// pulled. Returns the cursor unchanged so the sync handler treats a CSV
// connection as "nothing new" rather than erroring.
func (p *CSVProvider) FetchTransactions(_ context.Context, conn *Connection, _ time.Time) ([]RawTransaction, string, error) {
	if conn == nil {
		return nil, "", nil
	}
	return nil, conn.Cursor, nil
}

// Disconnect is a local-only operation for CSV (no provider grant to
// revoke), so it always succeeds.
func (p *CSVProvider) Disconnect(_ context.Context, _ *Connection) error { return nil }

// Ingest parses a statement CSV into provider-neutral RawTransactions.
// The header row must contain value_date, description and amount columns
// (order-independent, case-insensitive); currency and counterparty are
// optional. defaultCurrency is applied when a row omits the currency.
//
// Unlike the legacy ledger.ParseBankStatementCSV (which decoded a JSON
// fixture), this uses encoding/csv so quoted fields, embedded commas and
// CRLF line endings are handled correctly — a real bank export rather
// than a toy format.
func (p *CSVProvider) Ingest(r io.Reader, defaultCurrency string) ([]RawTransaction, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate ragged rows; we index by header
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("bankfeed: parse csv: %w", err)
	}
	if len(records) < 2 {
		return nil, nil
	}
	header := records[0]
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	vi, ok1 := col["value_date"]
	di, ok2 := col["description"]
	ai, ok3 := col["amount"]
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("bankfeed: csv must have value_date, description and amount columns")
	}
	ci, hasCurrency := col["currency"]
	cpi, hasCounterparty := col["counterparty"]

	cell := func(row []string, idx int) string {
		if idx < 0 || idx >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[idx])
	}

	out := make([]RawTransaction, 0, len(records)-1)
	for lineNo, row := range records[1:] {
		rawDate := cell(row, vi)
		if rawDate == "" {
			continue // skip blank trailing rows
		}
		vd, err := parseCSVDate(rawDate)
		if err != nil {
			return nil, fmt.Errorf("bankfeed: row %d value_date %q: %w", lineNo+2, rawDate, err)
		}
		amt, err := decimal.NewFromString(strings.ReplaceAll(cell(row, ai), ",", ""))
		if err != nil {
			return nil, fmt.Errorf("bankfeed: row %d amount %q: %w", lineNo+2, cell(row, ai), err)
		}
		currency := defaultCurrency
		if hasCurrency {
			if v := strings.ToUpper(cell(row, ci)); v != "" {
				currency = v
			}
		}
		counterparty := ""
		if hasCounterparty {
			counterparty = cell(row, cpi)
		}
		out = append(out, RawTransaction{
			ValueDate:    vd,
			Description:  cell(row, di),
			Amount:       amt,
			Currency:     currency,
			Counterparty: counterparty,
		})
	}
	return out, nil
}

// parseCSVDate accepts the common statement date formats so an operator
// does not have to pre-massage their export.
//
// Slash-separated dates are interpreted as day-first (DD/MM/YYYY), the
// international / ISO-adjacent convention used across the EU/UK where the
// GoCardless Open Banking feed is focused. We deliberately do NOT also try
// month-first (MM/DD/YYYY): for any date whose day and month are both ≤ 12
// (e.g. 03/04/2024) the two are indistinguishable, so accepting both would
// silently misread roughly a third of US-formatted exports — shifting a
// statement line by weeks and breaking reconciliation against the ±7-day
// match window. Callers that must ingest month-first exports should
// normalise to ISO 8601 (YYYY-MM-DD) before upload, which is always
// unambiguous and accepted here.
func parseCSVDate(s string) (time.Time, error) {
	layouts := []string{"2006-01-02", "2006/01/02", "02/01/2006", time.RFC3339}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format (use ISO 8601 YYYY-MM-DD, or day-first DD/MM/YYYY)")
}
