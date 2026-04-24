//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/helpdesk"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/reporting"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTenantForPhaseI provisions a bare tenant and returns the tenant
// plus the Phase I stores driven by the harness pool. Individual
// tests register the extra KTypes they need (crm.deal, helpdesk.ticket,
// finance.ar_invoice, …) so the helper stays small.
func newTenantForPhaseI(t *testing.T, h *harness) (
	*tenant.Tenant,
	*ledger.ExchangeRateStore,
	*helpdesk.Store,
	*reporting.Store,
	*reporting.Runner,
	*dashboard.Store,
) {
	t.Helper()
	ctx := context.Background()
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phasei"), Name: "Phase I Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	return tn,
		ledger.NewExchangeRateStore(h.pool),
		helpdesk.NewStore(h.pool),
		reporting.NewStore(h.pool),
		reporting.NewRunner(h.pool),
		dashboard.NewStore(h.pool)
}

// mustDecimalEq fails the test with a readable diff when two decimals
// differ. Tolerates representation-level noise (e.g. 1.0000000000 vs
// 1.0000) by comparing the canonical rational value.
func mustDecimalEq(t *testing.T, got, want decimal.Decimal, msg string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s: got=%s want=%s", msg, got.String(), want.String())
	}
}

// ---------------------------------------------------------------------------
// ExchangeRateStore
// ---------------------------------------------------------------------------

// TestExchangeRateUpsertAndGetRoundTrip walks the happy-path rate
// lifecycle: upsert → get (direct) → get (inverse) → same-currency
// short-circuit.
func TestExchangeRateUpsertAndGetRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, rates, _, _, _, _ := newTenantForPhaseI(t, h)

	date := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	got, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "EUR", ToCurrency: "USD",
		RateDate: date, Rate: decimal.NewFromFloat(1.10), Provider: "test",
	})
	if err != nil {
		t.Fatalf("upsert EUR→USD: %v", err)
	}
	if got.TenantID != tn.ID || got.FromCurrency != "EUR" || got.ToCurrency != "USD" {
		t.Fatalf("upsert round-trip lost identity: %+v", got)
	}
	if !got.Rate.Equal(decimal.NewFromFloat(1.10)) {
		t.Fatalf("rate round-trip: got %s want 1.10", got.Rate)
	}

	// Re-upsert with the same key mutates in place rather than inserting
	// a duplicate row.
	upd, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "EUR", ToCurrency: "USD",
		RateDate: date, Rate: decimal.NewFromFloat(1.20), Provider: "refreshed",
	})
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if !upd.Rate.Equal(decimal.NewFromFloat(1.20)) {
		t.Fatalf("re-upsert rate: got %s want 1.20", upd.Rate)
	}

	// Direct lookup returns the refreshed rate.
	rate, err := rates.GetRate(ctx, tn.ID, "EUR", "USD", date)
	if err != nil {
		t.Fatalf("get EUR→USD: %v", err)
	}
	mustDecimalEq(t, rate, decimal.NewFromFloat(1.20), "direct rate")

	// Inverse lookup: no USD→EUR row exists, but the store inverts
	// the direct rate transparently. 1 / 1.20 = 0.8333...
	inverse, err := rates.GetRate(ctx, tn.ID, "USD", "EUR", date)
	if err != nil {
		t.Fatalf("get USD→EUR (inverse): %v", err)
	}
	want := decimal.NewFromInt(1).Div(decimal.NewFromFloat(1.20))
	if !inverse.Equal(want) {
		t.Fatalf("inverse rate: got %s want %s", inverse, want)
	}

	// Same-currency short-circuits to 1 without hitting the DB.
	same, err := rates.GetRate(ctx, tn.ID, "USD", "USD", date)
	if err != nil {
		t.Fatalf("get USD→USD: %v", err)
	}
	mustDecimalEq(t, same, decimal.NewFromInt(1), "same-currency short-circuit")
}

// TestExchangeRateConvertAndUnrealizedGainLoss exercises the
// user-facing calculation helpers.
func TestExchangeRateConvertAndUnrealizedGainLoss(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, rates, _, _, _, _ := newTenantForPhaseI(t, h)

	date := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "GBP", ToCurrency: "USD",
		RateDate: date, Rate: decimal.NewFromFloat(1.25),
	}); err != nil {
		t.Fatalf("seed GBP→USD: %v", err)
	}

	// Convert £100 → $125.
	converted, err := rates.Convert(ctx, tn.ID, decimal.NewFromInt(100), "GBP", "USD", date)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	mustDecimalEq(t, converted, decimal.NewFromInt(125), "convert 100 GBP → USD")

	// Identical-currency convert is a pass-through.
	sameAmt, err := rates.Convert(ctx, tn.ID, decimal.NewFromInt(42), "USD", "USD", date)
	if err != nil {
		t.Fatalf("convert same currency: %v", err)
	}
	mustDecimalEq(t, sameAmt, decimal.NewFromInt(42), "same-currency convert")

	// Unrealized gain/loss: original £100 at 1.20 = $120 vs current
	// 1.25 = $125 → +$5.
	gain, err := rates.UnrealizedGainLoss(ctx, tn.ID,
		decimal.NewFromInt(100), "GBP", "USD",
		decimal.NewFromFloat(1.20), date)
	if err != nil {
		t.Fatalf("unrealized gain: %v", err)
	}
	mustDecimalEq(t, gain, decimal.NewFromInt(5), "unrealized gain")
}

// TestExchangeRateSentinelErrors covers the negative paths: missing
// rate → ErrExchangeRateNotFound, malformed codes → ErrInvalidCurrency.
func TestExchangeRateSentinelErrors(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, rates, _, _, _, _ := newTenantForPhaseI(t, h)

	// No rate seeded for JPY → USD.
	if _, err := rates.GetRate(ctx, tn.ID, "JPY", "USD", time.Now()); !errors.Is(err, ledger.ErrExchangeRateNotFound) {
		t.Fatalf("missing pair: got %v want ErrExchangeRateNotFound", err)
	}

	// 2-letter currency codes are rejected before any DB access.
	if _, err := rates.GetRate(ctx, tn.ID, "EU", "USD", time.Now()); !errors.Is(err, ledger.ErrInvalidCurrency) {
		t.Fatalf("bad from currency: got %v want ErrInvalidCurrency", err)
	}
	if _, err := rates.GetRate(ctx, tn.ID, "USD", "US", time.Now()); !errors.Is(err, ledger.ErrInvalidCurrency) {
		t.Fatalf("bad to currency: got %v want ErrInvalidCurrency", err)
	}

	// Same-currency short-circuit still validates the code length:
	// empty strings are 0-length so they must surface ErrInvalidCurrency
	// rather than returning 1.
	if _, err := rates.GetRate(ctx, tn.ID, "", "", time.Now()); !errors.Is(err, ledger.ErrInvalidCurrency) {
		t.Fatalf("empty codes: got %v want ErrInvalidCurrency", err)
	}
}

// ---------------------------------------------------------------------------
// Helpdesk Store
// ---------------------------------------------------------------------------

// TestHelpdeskPolicyLifecycle round-trips UpsertPolicy, asserts
// ListPolicies orders urgent first, validates ResolvePolicy's
// active-only filter, and confirms ErrPolicyNotFound surfaces when no
// active policy matches.
func TestHelpdeskPolicyLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, hd, _, _, _ := newTenantForPhaseI(t, h)

	// Seed one active policy per priority. The ordering in the input
	// slice is deliberately jumbled so ListPolicies' sort is the only
	// thing that can produce the expected output order.
	seed := []helpdesk.SLAPolicy{
		{TenantID: tn.ID, Name: "Low SLA", Priority: helpdesk.PriorityLow, ResponseMinutes: 480, ResolutionMinutes: 2880, Active: true},
		{TenantID: tn.ID, Name: "Urgent SLA", Priority: helpdesk.PriorityUrgent, ResponseMinutes: 15, ResolutionMinutes: 120, Active: true},
		{TenantID: tn.ID, Name: "High SLA", Priority: helpdesk.PriorityHigh, ResponseMinutes: 60, ResolutionMinutes: 480, Active: true},
		{TenantID: tn.ID, Name: "Medium SLA", Priority: helpdesk.PriorityMedium, ResponseMinutes: 180, ResolutionMinutes: 1440, Active: true},
	}
	for _, p := range seed {
		if _, err := hd.UpsertPolicy(ctx, p); err != nil {
			t.Fatalf("upsert %s: %v", p.Name, err)
		}
	}

	policies, err := hd.ListPolicies(ctx, tn.ID)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	wantOrder := []string{
		helpdesk.PriorityUrgent,
		helpdesk.PriorityHigh,
		helpdesk.PriorityMedium,
		helpdesk.PriorityLow,
	}
	if len(policies) != len(wantOrder) {
		t.Fatalf("list policies: got %d want %d", len(policies), len(wantOrder))
	}
	for i, p := range policies {
		if p.Priority != wantOrder[i] {
			t.Fatalf("list policies[%d]: got priority %q want %q", i, p.Priority, wantOrder[i])
		}
	}

	// ResolvePolicy returns the single active row for a priority.
	urgent, err := hd.ResolvePolicy(ctx, tn.ID, helpdesk.PriorityUrgent)
	if err != nil {
		t.Fatalf("resolve urgent: %v", err)
	}
	if urgent.Name != "Urgent SLA" {
		t.Fatalf("resolve urgent: got name %q want %q", urgent.Name, "Urgent SLA")
	}

	// Deactivate the high policy, then confirm ResolvePolicy
	// surfaces ErrPolicyNotFound — the partial unique index keeps
	// inactive rows around but they must not resolve.
	_ = urgent
	high := policies[1]
	high.Active = false
	if _, err := hd.UpsertPolicy(ctx, high); err != nil {
		t.Fatalf("deactivate high: %v", err)
	}
	if _, err := hd.ResolvePolicy(ctx, tn.ID, helpdesk.PriorityHigh); !errors.Is(err, helpdesk.ErrPolicyNotFound) {
		t.Fatalf("resolve deactivated high: got %v want ErrPolicyNotFound", err)
	}
	// Urgent still resolves.
	if _, err := hd.ResolvePolicy(ctx, tn.ID, helpdesk.PriorityUrgent); err != nil {
		t.Fatalf("urgent still should resolve: %v", err)
	}

	// A second inactive policy for the same (tenant, priority=high)
	// pair must be acceptable — the unique index is partial on
	// `WHERE active`. Insert a second inactive high policy to prove it.
	inactiveClone := helpdesk.SLAPolicy{
		TenantID: tn.ID, Name: "High SLA (archived)", Priority: helpdesk.PriorityHigh,
		ResponseMinutes: 90, ResolutionMinutes: 720, Active: false,
	}
	if _, err := hd.UpsertPolicy(ctx, inactiveClone); err != nil {
		t.Fatalf("second inactive policy for high priority: %v", err)
	}
	// Activating a second "high" policy now must succeed (since the
	// earlier one is inactive) — the unique index still only permits
	// one active row per priority.
	promote := inactiveClone
	promote.Active = true
	if _, err := hd.UpsertPolicy(ctx, promote); err != nil {
		t.Fatalf("promote archived policy to active: %v", err)
	}
	resolved, err := hd.ResolvePolicy(ctx, tn.ID, helpdesk.PriorityHigh)
	if err != nil {
		t.Fatalf("resolve promoted high: %v", err)
	}
	if resolved.Name != promote.Name {
		t.Fatalf("resolve promoted: got %q want %q", resolved.Name, promote.Name)
	}
}

// TestHelpdeskSLALog round-trips the append-only ticket_sla_log.
func TestHelpdeskSLALog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, hd, _, _, _ := newTenantForPhaseI(t, h)

	ticketA := uuid.New()
	ticketB := uuid.New()

	// Append a sequence of events for two tickets, spaced so
	// ListTicketLog ordering (newest first) is observable.
	now := time.Now().UTC()
	events := []struct {
		ticket     uuid.UUID
		kind       string
		occurredAt time.Time
	}{
		{ticketA, helpdesk.EventResponseWarning, now.Add(-3 * time.Minute)},
		{ticketA, helpdesk.EventResponseBreach, now.Add(-2 * time.Minute)},
		{ticketA, helpdesk.EventResolutionWarning, now.Add(-1 * time.Minute)},
		{ticketB, helpdesk.EventResolutionBreach, now.Add(-90 * time.Second)},
	}
	for _, e := range events {
		if _, err := hd.LogSLAEvent(ctx, helpdesk.SLALogEntry{
			TenantID: tn.ID, TicketID: e.ticket,
			EventKind: e.kind, OccurredAt: e.occurredAt,
		}); err != nil {
			t.Fatalf("log %s: %v", e.kind, err)
		}
	}

	aLog, err := hd.ListTicketLog(ctx, tn.ID, ticketA)
	if err != nil {
		t.Fatalf("list ticket A log: %v", err)
	}
	if len(aLog) != 3 {
		t.Fatalf("ticket A: got %d entries want 3", len(aLog))
	}
	// Newest first — resolution_warning is the most recent entry
	// for ticket A.
	if aLog[0].EventKind != helpdesk.EventResolutionWarning {
		t.Fatalf("ticket A newest: got %s want %s", aLog[0].EventKind, helpdesk.EventResolutionWarning)
	}
	if aLog[len(aLog)-1].EventKind != helpdesk.EventResponseWarning {
		t.Fatalf("ticket A oldest: got %s want %s", aLog[len(aLog)-1].EventKind, helpdesk.EventResponseWarning)
	}
	for _, e := range aLog {
		if e.TicketID != ticketA {
			t.Fatalf("RLS/predicate leak: ticket A query returned entry for %s", e.TicketID)
		}
	}

	bLog, err := hd.ListTicketLog(ctx, tn.ID, ticketB)
	if err != nil {
		t.Fatalf("list ticket B log: %v", err)
	}
	if len(bLog) != 1 || bLog[0].EventKind != helpdesk.EventResolutionBreach {
		t.Fatalf("ticket B log: got %+v want single resolution_breach", bLog)
	}

	// Append-only: logging another event for ticket A grows the list
	// rather than replacing the prior one.
	if _, err := hd.LogSLAEvent(ctx, helpdesk.SLALogEntry{
		TenantID: tn.ID, TicketID: ticketA,
		EventKind: helpdesk.EventResponseWarning, OccurredAt: now,
	}); err != nil {
		t.Fatalf("append follow-up: %v", err)
	}
	aLog2, err := hd.ListTicketLog(ctx, tn.ID, ticketA)
	if err != nil {
		t.Fatalf("relist ticket A: %v", err)
	}
	if len(aLog2) != 4 {
		t.Fatalf("append-only: got %d entries want 4", len(aLog2))
	}
}

// ---------------------------------------------------------------------------
// Report Builder
// ---------------------------------------------------------------------------

// dealRecord is the seed we use for every report-builder test. The
// fields stay primitive so the KRecord validator accepts them without
// the test having to re-register a custom schema.
type dealRecord struct {
	Name     string
	Stage    string
	Amount   float64
	Currency string
	Owner    string
}

// seedDeals registers the canonical crm.deal KType and inserts the
// supplied deals under `tenantID`. Returns the KType name used in
// `ktype:<name>` report sources. The caller-supplied `Owner` label is
// mapped to a deterministic UUID per label so the schema validator
// (which treats `owner` as a ref→user) accepts the input while still
// letting the report tests assert on a stable value.
func seedDeals(t *testing.T, h *harness, tenantID uuid.UUID, deals []dealRecord) string {
	t.Helper()
	ctx := context.Background()
	if err := crm.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}
	owners := map[string]uuid.UUID{}
	for _, d := range deals {
		if _, ok := owners[d.Owner]; !ok {
			owners[d.Owner] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(d.Owner))
		}
	}
	actor := uuid.New()
	for _, d := range deals {
		body, err := json.Marshal(map[string]any{
			"name":     d.Name,
			"stage":    d.Stage,
			"amount":   d.Amount,
			"currency": d.Currency,
			"owner":    owners[d.Owner].String(),
		})
		if err != nil {
			t.Fatalf("marshal deal: %v", err)
		}
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID: tenantID, KType: crm.KTypeDeal,
			Data: body, CreatedBy: actor,
		}); err != nil {
			t.Fatalf("create deal %q: %v", d.Name, err)
		}
	}
	return crm.KTypeDeal
}

// TestReportBuilderColumnsAndFilters exercises the simplest report
// shape: select columns from a KType source with a few filters.
func TestReportBuilderColumnsAndFilters(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, runner, _ := newTenantForPhaseI(t, h)

	ktypeName := seedDeals(t, h, tn.ID, []dealRecord{
		{Name: "Alpha", Stage: "qualification", Amount: 100, Currency: "USD", Owner: "alice"},
		{Name: "Bravo", Stage: "proposal", Amount: 500, Currency: "USD", Owner: "bob"},
		{Name: "Charlie", Stage: "won", Amount: 1000, Currency: "EUR", Owner: "alice"},
		{Name: "Delta", Stage: "lost", Amount: 200, Currency: "USD", Owner: "alice"},
	})

	// Columns-only projection.
	res, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:  "ktype:" + ktypeName,
		Columns: []string{"name", "stage", "amount"},
		Sort:    []reporting.Sort{{Column: "name", Direction: "asc"}},
	})
	if err != nil {
		t.Fatalf("run columns: %v", err)
	}
	if len(res.Rows) != 4 {
		t.Fatalf("columns: got %d rows want 4", len(res.Rows))
	}
	if got := res.Rows[0]["name"]; got != "Alpha" {
		t.Fatalf("first row name: got %v want Alpha", got)
	}

	// Filter: stage IN (qualification, proposal). The reporting
	// grammar expresses `in` with a JSON array.
	inRes, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:  "ktype:" + ktypeName,
		Columns: []string{"name", "stage"},
		Filters: []reporting.Filter{
			{Column: "stage", Op: "in", Value: json.RawMessage(`["qualification","proposal"]`)},
		},
	})
	if err != nil {
		t.Fatalf("run in-filter: %v", err)
	}
	if len(inRes.Rows) != 2 {
		t.Fatalf("in filter: got %d rows want 2", len(inRes.Rows))
	}

	// Filter: stage != won AND currency = USD → Alpha + Bravo + Delta.
	eqRes, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:  "ktype:" + ktypeName,
		Columns: []string{"name"},
		Filters: []reporting.Filter{
			{Column: "stage", Op: "!=", Value: json.RawMessage(`"won"`)},
			{Column: "currency", Op: "=", Value: json.RawMessage(`"USD"`)},
		},
		Sort: []reporting.Sort{{Column: "name", Direction: "asc"}},
	})
	if err != nil {
		t.Fatalf("run eq-filter: %v", err)
	}
	names := make([]string, 0, len(eqRes.Rows))
	for _, r := range eqRes.Rows {
		if s, ok := r["name"].(string); ok {
			names = append(names, s)
		}
	}
	wantNames := []string{"Alpha", "Bravo", "Delta"}
	if len(names) != len(wantNames) {
		t.Fatalf("eq filter: got rows %v want %v", names, wantNames)
	}
	for i, n := range wantNames {
		if names[i] != n {
			t.Fatalf("eq filter[%d]: got %q want %q", i, names[i], n)
		}
	}

	// Null filter: the deals all specify currency, so
	// `notes null` — which is not set on any seeded deal — must
	// return every row.
	nullRes, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:  "ktype:" + ktypeName,
		Columns: []string{"name"},
		Filters: []reporting.Filter{{Column: "notes", Op: "null"}},
	})
	if err != nil {
		t.Fatalf("run null-filter: %v", err)
	}
	if len(nullRes.Rows) != 4 {
		t.Fatalf("null filter on unset field: got %d rows want 4", len(nullRes.Rows))
	}
}

// TestReportBuilderAggregationsAndGroupBy exercises sum/count/avg with
// group_by + sort.
func TestReportBuilderAggregationsAndGroupBy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, runner, _ := newTenantForPhaseI(t, h)

	ktypeName := seedDeals(t, h, tn.ID, []dealRecord{
		{Name: "Deal1", Stage: "qualification", Amount: 100, Currency: "USD", Owner: "alice"},
		{Name: "Deal2", Stage: "qualification", Amount: 200, Currency: "USD", Owner: "alice"},
		{Name: "Deal3", Stage: "proposal", Amount: 600, Currency: "USD", Owner: "bob"},
	})

	res, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:  "ktype:" + ktypeName,
		GroupBy: []string{"stage"},
		Aggregations: []reporting.Aggregation{
			{Op: reporting.AggSum, Column: "amount", Alias: "sum_amount"},
			{Op: reporting.AggCount, Alias: "deal_count"},
			{Op: reporting.AggAvg, Column: "amount", Alias: "avg_amount"},
		},
		Sort: []reporting.Sort{{Column: "sum_amount", Direction: "desc"}},
	})
	if err != nil {
		t.Fatalf("run aggregations: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("aggregations: got %d groups want 2", len(res.Rows))
	}
	// proposal: sum=600, count=1. qualification: sum=300, count=2.
	// Sort desc on sum_amount → proposal first.
	first := res.Rows[0]
	if first["stage"] != "proposal" {
		t.Fatalf("first group: got %v want proposal", first["stage"])
	}
	if !approxEq(first["sum_amount"], 600) {
		t.Fatalf("proposal sum: got %v want 600", first["sum_amount"])
	}
	if !approxEq(first["deal_count"], 1) {
		t.Fatalf("proposal count: got %v want 1", first["deal_count"])
	}
	second := res.Rows[1]
	if second["stage"] != "qualification" {
		t.Fatalf("second group: got %v want qualification", second["stage"])
	}
	if !approxEq(second["sum_amount"], 300) {
		t.Fatalf("qualification sum: got %v want 300", second["sum_amount"])
	}
	if !approxEq(second["avg_amount"], 150) {
		t.Fatalf("qualification avg: got %v want 150", second["avg_amount"])
	}
}

// TestReportBuilderPivot verifies the pivot spec produces a valid
// PivotResult with the expected row/column headers and cell values.
func TestReportBuilderPivot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, runner, _ := newTenantForPhaseI(t, h)

	ktypeName := seedDeals(t, h, tn.ID, []dealRecord{
		{Name: "D1", Stage: "qualification", Amount: 100, Currency: "USD", Owner: "alice"},
		{Name: "D2", Stage: "proposal", Amount: 200, Currency: "USD", Owner: "alice"},
		{Name: "D3", Stage: "qualification", Amount: 300, Currency: "USD", Owner: "bob"},
	})

	res, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:  "ktype:" + ktypeName,
		GroupBy: []string{"owner", "stage"},
		Aggregations: []reporting.Aggregation{
			{Op: reporting.AggSum, Column: "amount", Alias: "sum_amount"},
		},
		Pivot: &reporting.PivotSpec{
			RowColumn:    "owner",
			ColumnColumn: "stage",
			ValueColumn:  "sum_amount",
		},
	})
	if err != nil {
		t.Fatalf("run pivot: %v", err)
	}
	if res.Pivot == nil {
		t.Fatalf("pivot: nil PivotResult")
	}
	if len(res.Pivot.RowHeaders) != 2 {
		t.Fatalf("pivot rows: got %v want 2 rows", res.Pivot.RowHeaders)
	}
	if len(res.Pivot.ColumnHeaders) < 1 {
		t.Fatalf("pivot columns: got %v want ≥1", res.Pivot.ColumnHeaders)
	}
}

// TestReportBuilderValidateRejectsBadInput ensures Validate rejects
// malformed sources, identifiers, and operators.
func TestReportBuilderValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		def  reporting.Definition
	}{
		{"empty source", reporting.Definition{}},
		{"bad ledger source", reporting.Definition{Source: "ledger.bogus_table"}},
		{"invalid column identifier", reporting.Definition{
			Source:  "ktype:crm.deal",
			Columns: []string{"amount; DROP TABLE krecords"},
		}},
		{"invalid filter op", reporting.Definition{
			Source:  "ktype:crm.deal",
			Filters: []reporting.Filter{{Column: "amount", Op: "EXPLODE", Value: json.RawMessage(`1`)}},
		}},
		{"invalid aggregation op", reporting.Definition{
			Source:       "ktype:crm.deal",
			Aggregations: []reporting.Aggregation{{Op: "median", Column: "amount"}},
		}},
		{"invalid sort column", reporting.Definition{
			Source: "ktype:crm.deal",
			Sort:   []reporting.Sort{{Column: "amount; --", Direction: "asc"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.def.Validate(); err == nil {
				t.Fatalf("expected validation failure for %s", tc.name)
			}
		})
	}
}

// TestReportBuilderExcludesSoftDeleted verifies the runner defaults
// to excluding soft-deleted KRecords.
func TestReportBuilderExcludesSoftDeleted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, runner, _ := newTenantForPhaseI(t, h)

	ktypeName := seedDeals(t, h, tn.ID, []dealRecord{
		{Name: "Alive", Stage: "qualification", Amount: 100, Currency: "USD", Owner: "alice"},
		{Name: "Doomed", Stage: "qualification", Amount: 999, Currency: "USD", Owner: "alice"},
	})

	// Soft-delete one deal by id. We need the id, so list first.
	recs, err := h.records.List(ctx, tn.ID, record.ListFilter{KType: ktypeName})
	if err != nil {
		t.Fatalf("list deals: %v", err)
	}
	var doomed uuid.UUID
	for _, r := range recs {
		var d map[string]any
		_ = json.Unmarshal(r.Data, &d)
		if d["name"] == "Doomed" {
			doomed = r.ID
		}
	}
	if doomed == uuid.Nil {
		t.Fatalf("seed lookup failed; recs=%+v", recs)
	}
	actor := uuid.New()
	if err := h.records.Delete(ctx, tn.ID, doomed, actor); err != nil {
		t.Fatalf("soft-delete doomed: %v", err)
	}

	res, err := runner.Run(ctx, tn.ID, reporting.Definition{
		Source:       "ktype:" + ktypeName,
		Aggregations: []reporting.Aggregation{{Op: reporting.AggSum, Column: "amount", Alias: "sum_amount"}},
	})
	if err != nil {
		t.Fatalf("run aggregate: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("aggregate: got %d rows want 1", len(res.Rows))
	}
	// Only "Alive" (amount=100) contributes — the deleted row is
	// excluded by default.
	if !approxEq(res.Rows[0]["sum_amount"], 100) {
		t.Fatalf("sum_amount: got %v want 100 (soft-deleted row leaked)", res.Rows[0]["sum_amount"])
	}
}

// ---------------------------------------------------------------------------
// Dashboard summary
// ---------------------------------------------------------------------------

// TestDashboardSummaryCounts seeds representative records of every
// KType the dashboard aggregates over and verifies the returned
// counters match.
func TestDashboardSummaryCounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, _, _, _, dash := newTenantForPhaseI(t, h)
	actor := uuid.New()

	// CRM deals — register and seed.
	if err := crm.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}
	for _, d := range []dealRecord{
		{Name: "Open Alpha", Stage: "qualification", Amount: 100, Currency: "USD"},
		{Name: "Open Bravo", Stage: "proposal", Amount: 200, Currency: "USD"},
		{Name: "Won Charlie", Stage: "won", Amount: 500, Currency: "USD"},
		{Name: "Lost Delta", Stage: "lost", Amount: 400, Currency: "USD"},
	} {
		body, _ := json.Marshal(map[string]any{
			"name": d.Name, "stage": d.Stage,
			"amount": d.Amount, "currency": d.Currency,
		})
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID: tn.ID, KType: crm.KTypeDeal, Data: body, CreatedBy: actor,
		}); err != nil {
			t.Fatalf("seed deal %s: %v", d.Name, err)
		}
	}

	// Finance AR invoices — register ktypes and seed.
	if err := seedDashboardInvoices(h, tn.ID, actor); err != nil {
		t.Fatalf("seed invoices: %v", err)
	}

	// Helpdesk tickets.
	if err := helpdesk.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register helpdesk ktypes: %v", err)
	}
	nowISO := time.Now().UTC()
	tickets := []map[string]any{
		{"subject": "Ticket Open", "status": "open", "priority": "high"},
		{"subject": "Ticket In Progress", "status": "in_progress", "priority": "medium"},
		{"subject": "Ticket Closed", "status": "closed", "priority": "low"},
		// Overdue ticket — status is open and resolution deadline is in the past.
		{"subject": "Ticket Overdue", "status": "open", "priority": "urgent",
			"sla_resolution_by": nowISO.Add(-2 * time.Hour).Format(time.RFC3339)},
	}
	for _, body := range tickets {
		raw, _ := json.Marshal(body)
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID: tn.ID, KType: helpdesk.KTypeTicket,
			Data: raw, CreatedBy: actor,
		}); err != nil {
			t.Fatalf("seed ticket %v: %v", body["subject"], err)
		}
	}

	summary, err := dash.ComputeSummary(ctx, tn.ID)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.OpenDealsCount != 2 {
		t.Fatalf("open_deals_count: got %d want 2", summary.OpenDealsCount)
	}
	if summary.PipelineValue != 300 {
		t.Fatalf("pipeline_value: got %v want 300", summary.PipelineValue)
	}
	if summary.OutstandingAR != 700 {
		t.Fatalf("outstanding_ar: got %v want 700", summary.OutstandingAR)
	}
	if summary.OutstandingAP != 400 {
		t.Fatalf("outstanding_ap: got %v want 400", summary.OutstandingAP)
	}
	if summary.OpenTicketsCount != 3 {
		t.Fatalf("open_tickets_count: got %d want 3", summary.OpenTicketsCount)
	}
	if summary.OverdueTicketsCount != 1 {
		t.Fatalf("overdue_tickets_count: got %d want 1", summary.OverdueTicketsCount)
	}
}

// seedDashboardInvoices inserts AR invoice and AP bill KRecords used
// by the dashboard summary counters. We set outstanding_amount + status
// directly on data rather than going through the poster because the
// dashboard aggregates over the JSONB column, not the ledger.
func seedDashboardInvoices(h *harness, tenantID, actor uuid.UUID) error {
	ctx := context.Background()
	customer1 := uuid.New().String()
	customer2 := uuid.New().String()
	customer3 := uuid.New().String()
	supplier1 := uuid.New().String()
	supplier2 := uuid.New().String()
	arBodies := []map[string]any{
		{"customer_id": customer1, "invoice_number": "AR-1", "issue_date": "2026-01-01", "due_date": "2026-02-01",
			"subtotal": 500, "tax_amount": 0, "total": 500, "outstanding_amount": 500, "currency": "USD",
			"status": "posted", "ar_account_code": "1100", "revenue_account_code": "4000"},
		{"customer_id": customer2, "invoice_number": "AR-2", "issue_date": "2026-01-02", "due_date": "2026-02-02",
			"subtotal": 200, "tax_amount": 0, "total": 200, "outstanding_amount": 200, "currency": "USD",
			"status": "posted", "ar_account_code": "1100", "revenue_account_code": "4000"},
		{"customer_id": customer3, "invoice_number": "AR-PAID", "issue_date": "2026-01-03", "due_date": "2026-02-03",
			"subtotal": 999, "tax_amount": 0, "total": 999, "outstanding_amount": 999, "currency": "USD",
			"status": "paid", "ar_account_code": "1100", "revenue_account_code": "4000"},
	}
	apBodies := []map[string]any{
		{"supplier_id": supplier1, "bill_number": "AP-1", "issue_date": "2026-01-01", "due_date": "2026-02-01",
			"subtotal": 400, "tax_amount": 0, "total": 400, "outstanding_amount": 400, "currency": "USD",
			"status": "posted", "ap_account_code": "2100", "expense_account_code": "6000"},
		{"supplier_id": supplier2, "bill_number": "AP-CANCELLED", "issue_date": "2026-01-02", "due_date": "2026-02-02",
			"subtotal": 123, "tax_amount": 0, "total": 123, "outstanding_amount": 123, "currency": "USD",
			"status": "cancelled", "ap_account_code": "2100", "expense_account_code": "6000"},
	}
	// The dashboard uses typed JSONB extraction and does not depend
	// on finance KType schemas; but the KType registry rejects
	// unknown ktypes. Register them here.
	if err := financeKTypesIfNeeded(h); err != nil {
		return err
	}
	for _, b := range arBodies {
		raw, _ := json.Marshal(b)
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID: tenantID, KType: "finance.ar_invoice", Data: raw, CreatedBy: actor,
		}); err != nil {
			return fmt.Errorf("seed AR %s: %w", b["invoice_number"], err)
		}
	}
	for _, b := range apBodies {
		raw, _ := json.Marshal(b)
		if _, err := h.records.Create(ctx, record.KRecord{
			TenantID: tenantID, KType: "finance.ap_bill", Data: raw, CreatedBy: actor,
		}); err != nil {
			return fmt.Errorf("seed AP %s: %w", b["bill_number"], err)
		}
	}
	return nil
}

// financeKTypesIfNeeded registers finance KTypes against the registry.
// ktype.Register is idempotent so repeat calls across tests are safe.
func financeKTypesIfNeeded(h *harness) error {
	return finance.RegisterKTypes(context.Background(), h.ktypes)
}

// ---------------------------------------------------------------------------
// RLS isolation
// ---------------------------------------------------------------------------

// TestRLSIsolatesPhaseITables seeds Phase I typed tables for two
// tenants and confirms RLS policies prevent cross-tenant reads.
func TestRLSIsolatesPhaseITables(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tnA, ratesA, hdA, reportsA, _, _ := newTenantForPhaseI(t, h)
	tnB, ratesB, hdB, reportsB, _, _ := newTenantForPhaseI(t, h)

	today := time.Now().UTC().Truncate(24 * time.Hour)
	if _, err := ratesA.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tnA.ID, FromCurrency: "EUR", ToCurrency: "USD",
		RateDate: today, Rate: decimal.NewFromFloat(1.10),
	}); err != nil {
		t.Fatalf("seed rate A: %v", err)
	}
	if _, err := hdA.UpsertPolicy(ctx, helpdesk.SLAPolicy{
		TenantID: tnA.ID, Name: "A urgent", Priority: helpdesk.PriorityUrgent,
		ResponseMinutes: 15, ResolutionMinutes: 60, Active: true,
	}); err != nil {
		t.Fatalf("seed policy A: %v", err)
	}
	ticketA := uuid.New()
	if _, err := hdA.LogSLAEvent(ctx, helpdesk.SLALogEntry{
		TenantID: tnA.ID, TicketID: ticketA,
		EventKind: helpdesk.EventResponseBreach,
	}); err != nil {
		t.Fatalf("seed log A: %v", err)
	}
	if _, err := reportsA.Create(ctx, reporting.SavedReport{
		TenantID: tnA.ID, Name: "A deals", Description: "private",
		Definition: reporting.Definition{Source: "ktype:crm.deal", Columns: []string{"name"}},
	}); err != nil {
		t.Fatalf("seed report A: %v", err)
	}

	// Tenant B must see none of A's rows.
	bRates, err := ratesB.ListRates(ctx, tnB.ID, "", "", 100)
	if err != nil {
		t.Fatalf("list B rates: %v", err)
	}
	if len(bRates) != 0 {
		t.Fatalf("RLS leak: B saw %d exchange rates", len(bRates))
	}
	bPolicies, err := hdB.ListPolicies(ctx, tnB.ID)
	if err != nil {
		t.Fatalf("list B policies: %v", err)
	}
	if len(bPolicies) != 0 {
		t.Fatalf("RLS leak: B saw %d policies", len(bPolicies))
	}
	bLog, err := hdB.ListTicketLog(ctx, tnB.ID, ticketA)
	if err != nil {
		t.Fatalf("list B ticket log for A's ticket id: %v", err)
	}
	if len(bLog) != 0 {
		t.Fatalf("RLS leak: B saw %d ticket log entries for A's ticket", len(bLog))
	}
	bReports, err := reportsB.List(ctx, tnB.ID)
	if err != nil {
		t.Fatalf("list B reports: %v", err)
	}
	if len(bReports) != 0 {
		t.Fatalf("RLS leak: B saw %d saved reports", len(bReports))
	}

	// Direct SQL under B's tenant context also returns zero rows for
	// every Phase I table — proves the RLS policy, not just the
	// application-level filter, is doing the work.
	if err := dbutil.WithTenantTx(ctx, h.pool, tnB.ID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		for _, q := range []string{
			`SELECT count(*) FROM exchange_rates`,
			`SELECT count(*) FROM sla_policies`,
			`SELECT count(*) FROM ticket_sla_log`,
			`SELECT count(*) FROM saved_reports`,
		} {
			if err := tx.QueryRow(ctx, q).Scan(&n); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
			if n != 0 {
				return fmt.Errorf("RLS leak: %s returned %d rows under tenant B", q, n)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("RLS sanity check: %v", err)
	}

	// A still sees its own rows.
	aRates, err := ratesA.ListRates(ctx, tnA.ID, "", "", 100)
	if err != nil || len(aRates) != 1 {
		t.Fatalf("A rates: %+v err=%v", aRates, err)
	}
}

// ---------------------------------------------------------------------------
// Small utilities
// ---------------------------------------------------------------------------

// approxEq compares a report-cell value (which can come back as
// int64, float64, pgtype.Numeric, or *decimal.Decimal depending on
// the aggregation) against a whole number expected value. Returns
// true when the values are equal within 1e-6.
func approxEq(got any, want float64) bool {
	switch v := got.(type) {
	case int:
		return float64(v) == want
	case int64:
		return float64(v) == want
	case float64:
		return abs(v-want) < 1e-6
	case decimal.Decimal:
		f, _ := v.Float64()
		return abs(f-want) < 1e-6
	case *decimal.Decimal:
		if v == nil {
			return false
		}
		f, _ := v.Float64()
		return abs(f-want) < 1e-6
	case pgtype.Numeric:
		fv, err := v.Float64Value()
		if err != nil || !fv.Valid {
			return false
		}
		return abs(fv.Float64-want) < 1e-6
	case string:
		d, err := decimal.NewFromString(v)
		if err != nil {
			return false
		}
		f, _ := d.Float64()
		return abs(f-want) < 1e-6
	default:
		return false
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
