//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dashboard"
	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestMultiCurrencyEndToEnd is the Phase N7 verification harness. It
// walks the entire multi-currency posting pipeline against a live
// database — there is no mocking of the rate store, the ledger, or
// the dashboard converter. The seven explicit steps in PROPOSAL.md
// Phase N7 are exercised in order so a regression in any one of
// them surfaces as a focused failure.
//
//  1. Create a tenant with `base_currency = "EUR"`.
//  2. Upsert a USD→EUR exchange rate.
//  3. Create + post a USD sales invoice, then verify every
//     foreign-currency journal_line has a populated `base_amount` in
//     EUR equal to (line amount × rate).
//  4. Create + post a partial USD payment, then verify the payment
//     journal entry's base_amount lines round-trip correctly and the
//     invoice's outstanding_amount is decremented.
//  5. Move the rate to a new (lower) USD→EUR value and run the
//     UnrealizedGainLossJob. Verify an FX revaluation entry was
//     posted whose delta equals (open USD balance × new rate) − prior
//     recorded base.
//  6. Compute the dashboard summary and verify the AR widget is folded
//     into EUR using the current rate (i.e. the dashboard rate
//     adapter is wired and not silently returning zero).
//  7. Query journal_lines grouped by currency and verify both the
//     original currency and base-currency amounts are present on
//     every row that crossed the FX boundary.
//
// The test is intentionally end-to-end: it does not bypass the
// poster (which is the path that writes base_amount) and it uses the
// same dashboard.Store + dashboardRateAdapter wiring that the API
// constructs at boot. That way a regression in any layer — schema,
// rate lookup, poster conversion, dashboard fold, revaluation job —
// fails this test.
func TestMultiCurrencyEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// --- Step 1: tenant with base_currency = EUR --------------------
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("multifx"), Name: "Multi-FX Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := h.tenants.SetBaseCurrency(ctx, tn.ID, "EUR"); err != nil {
		t.Fatalf("set base currency: %v", err)
	}

	// Register the finance KTypes once per tenant — Create() refuses to
	// build invoice / payment KRecords if the type is not on the
	// registry.
	if err := finance.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register finance ktypes: %v", err)
	}

	// The exchange-rate store is wired into PGStore via
	// WithExchangeRates so PostJournalEntry can call
	// computeBaseAmount. Without this wiring base_amount stays NULL
	// (the pre-000029 legacy path) and Step 3's assertion fails.
	rates := ledger.NewExchangeRateStore(h.pool)
	ledgerStore := ledger.NewPGStore(h.pool, h.publisher, h.auditor).WithExchangeRates(rates)
	invoicePoster := ledger.NewInvoicePoster(ledgerStore, h.records)
	paymentPoster := ledger.NewPaymentPoster(ledgerStore, h.records)

	// Seed the chart of accounts. The FX gain/loss accounts (4910,
	// 5910) are required by the UnrealizedGainLossJob — without them
	// the revaluation entry insert fails with a foreign-key
	// violation. Banking + AR/Revenue/Tax accounts cover the invoice
	// and payment posting paths.
	seedAccounts := []ledger.Account{
		{TenantID: tn.ID, Code: "1100", Name: "Accounts Receivable", Type: ledger.AccountTypeAsset, Active: true},
		{TenantID: tn.ID, Code: "1200", Name: "Bank — USD operating", Type: ledger.AccountTypeAsset, Active: true},
		{TenantID: tn.ID, Code: "2200", Name: "Tax Payable", Type: ledger.AccountTypeLiability, Active: true},
		{TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
		{TenantID: tn.ID, Code: ledger.AccountCodeUnrealizedFXGain, Name: "Unrealized FX Gain", Type: ledger.AccountTypeRevenue, Active: true},
		{TenantID: tn.ID, Code: ledger.AccountCodeUnrealizedFXLoss, Name: "Unrealized FX Loss", Type: ledger.AccountTypeExpense, Active: true},
	}
	for _, a := range seedAccounts {
		if _, err := ledgerStore.CreateAccount(ctx, a); err != nil {
			t.Fatalf("seed account %s: %v", a.Code, err)
		}
	}

	actor := uuid.New()

	// --- Step 2: upsert USD→EUR exchange rate -----------------------
	// Choose a rate distinct from 1.0 so every conversion produces an
	// observable difference — picking 0.95 means USD 1,000 → EUR 950.
	day1 := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	rate1 := decimal.NewFromFloat(0.95)
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "USD", ToCurrency: "EUR",
		RateDate: day1, Rate: rate1, Provider: "phase-n7",
	}); err != nil {
		t.Fatalf("upsert USD→EUR day1: %v", err)
	}

	// --- Step 3: post a USD invoice; verify base_amount in EUR ------
	// Subtotal 1,000 USD + 80 USD tax = 1,080 USD total. The poster
	// writes three lines:
	//   Dr 1100 (AR)        1080 USD  → base 1080 × 0.95 = 1026.00 EUR
	//   Cr 4000 (Revenue)   1000 USD  → base 1000 × 0.95 =  950.00 EUR
	//   Cr 2200 (Tax)         80 USD  → base   80 × 0.95 =   76.00 EUR
	subtotal := decimal.NewFromInt(1000)
	tax := decimal.NewFromInt(80)
	invID := createARInvoiceRecordWithDate(t, h, tn.ID, actor,
		"INV-N7-001", uuid.NewString(), subtotal, tax, "2200",
		day1.Format("2006-01-02"))

	invEntry, err := invoicePoster.PostSalesInvoice(ctx, tn.ID, invID, actor)
	if err != nil {
		t.Fatalf("post invoice: %v", err)
	}
	mustBaseAmountEquals(t, invEntry, "1100", decimal.NewFromInt(1080), rate1)
	mustBaseAmountEquals(t, invEntry, "4000", decimal.NewFromInt(-1000), rate1) // credit → negative net
	mustBaseAmountEquals(t, invEntry, "2200", decimal.NewFromInt(-80), rate1)

	// --- Step 4: post a partial USD payment; verify conversion ------
	// Pay 400 USD against the 1080 USD invoice → invoice outstanding
	// drops to 680 USD. The payment JE has two lines:
	//   Dr 1200 (Bank)    400 USD → base 400 × 0.95 = 380.00 EUR
	//   Cr 1100 (AR)      400 USD → base 400 × 0.95 = 380.00 EUR
	payAmount := decimal.NewFromInt(400)
	payID := createPaymentRecord(t, h, tn.ID, actor, paymentPayload{
		Reference:     "PAY-N7-001",
		PaymentType:   "receive",
		PartyType:     "customer",
		PartyID:       uuid.NewString(),
		Amount:        payAmount,
		Currency:      "USD",
		PaymentDate:   day1.Format("2006-01-02"),
		BankAccount:   "1200",
		ARAccountCode: "1100",
		Allocations: []map[string]any{
			{"invoice_id": invID.String(), "allocated_amount": payAmount.InexactFloat64()},
		},
	})
	payEntry, err := paymentPoster.PostPayment(ctx, tn.ID, payID, actor)
	if err != nil {
		t.Fatalf("post payment: %v", err)
	}
	mustBaseAmountEquals(t, payEntry, "1200", payAmount, rate1)
	mustBaseAmountEquals(t, payEntry, "1100", payAmount.Neg(), rate1)

	// Confirm the AR record's outstanding amount dropped by 400 USD.
	invRec, err := h.records.Get(ctx, tn.ID, invID)
	if err != nil {
		t.Fatalf("reload invoice: %v", err)
	}
	var invData struct {
		Outstanding decimal.Decimal `json:"outstanding_amount"`
	}
	if err := json.Unmarshal(invRec.Data, &invData); err != nil {
		t.Fatalf("unmarshal invoice: %v", err)
	}
	wantOutstanding := decimal.NewFromInt(680)
	if !invData.Outstanding.Equal(wantOutstanding) {
		t.Fatalf("outstanding after partial payment: got %s want %s",
			invData.Outstanding, wantOutstanding)
	}

	// --- Step 5: shift rate, run revaluation job --------------------
	// The job walks every open (account, currency) pair where the
	// account is asset OR liability. Our seed has three open
	// non-base-currency balances:
	//
	//   1100 (AR, asset):       foreignNet = +680 USD
	//                           recorded base = 1026 − 380 = +646 EUR
	//                           current base @ 0.90 = +612 EUR
	//                           delta = 612 − 646 = −34 EUR  →  LOSS
	//
	//   1200 (Bank, asset):     foreignNet = +400 USD
	//                           recorded base = +380 EUR
	//                           current base @ 0.90 = +360 EUR
	//                           delta = 360 − 380 = −20 EUR  →  LOSS
	//
	//   2200 (Tax payable, L):  foreignNet = −80 USD
	//                           recorded base = −76 EUR
	//                           current base @ 0.90 = −72 EUR
	//                           delta = −72 − (−76) = +4 EUR →  GAIN
	//
	// Net of all three: -34 -20 +4 = -50 EUR unrealised loss for the
	// period, posted across three entries (one per pair).
	day2 := day1.Add(7 * 24 * time.Hour)
	rate2 := decimal.NewFromFloat(0.90)
	if _, err := rates.UpsertRate(ctx, ledger.ExchangeRate{
		TenantID: tn.ID, FromCurrency: "USD", ToCurrency: "EUR",
		RateDate: day2, Rate: rate2, Provider: "phase-n7-day2",
	}); err != nil {
		t.Fatalf("upsert USD→EUR day2: %v", err)
	}
	job := ledger.NewUnrealizedGainLossJob(ledgerStore, rates, uuid.New())
	if err := job.Handle(ctx, tn.ID, scheduler.ScheduledAction{TenantID: tn.ID}); err != nil {
		t.Fatalf("run unrealized G/L: %v", err)
	}

	revalEntries := listEntriesBySource(t, h, tn.ID, "finance.fx_revaluation")
	if len(revalEntries) != 3 {
		t.Fatalf("expected 3 FX revaluation entries (1100, 1200, 2200), got %d",
			len(revalEntries))
	}
	wantPerAccount := map[string]struct {
		debitAcct string // the gain/loss leg
		credAcct  string // the open-account leg
		amount    decimal.Decimal
	}{
		"1100": {
			debitAcct: ledger.AccountCodeUnrealizedFXLoss,
			credAcct:  "1100",
			amount:    decimal.NewFromInt(34),
		},
		"1200": {
			debitAcct: ledger.AccountCodeUnrealizedFXLoss,
			credAcct:  "1200",
			amount:    decimal.NewFromInt(20),
		},
		"2200": {
			// Liability with a delta that moved the balance closer to
			// zero registers as a gain — the open-account leg is the
			// debit, the unrealised-gain account is the credit.
			debitAcct: "2200",
			credAcct:  ledger.AccountCodeUnrealizedFXGain,
			amount:    decimal.NewFromInt(4),
		},
	}
	got := indexRevaluationByOpenAccount(t, revalEntries)
	for openAcct, want := range wantPerAccount {
		entry, ok := got[openAcct]
		if !ok {
			t.Fatalf("missing FX revaluation entry for open account %s", openAcct)
		}
		dr := findLine(t, &entry, want.debitAcct).Debit
		cr := findLine(t, &entry, want.credAcct).Credit
		if !dr.Equal(want.amount) {
			t.Fatalf("reval %s: Dr %s = %s, want %s",
				openAcct, want.debitAcct, dr, want.amount)
		}
		if !cr.Equal(want.amount) {
			t.Fatalf("reval %s: Cr %s = %s, want %s",
				openAcct, want.credAcct, cr, want.amount)
		}
		// All revaluation lines are denominated in the base currency.
		for _, l := range entry.Lines {
			if l.Currency != "EUR" {
				t.Fatalf("revaluation line currency: got %s want EUR (entry %s)",
					l.Currency, openAcct)
			}
		}
	}

	// --- Step 6: dashboard summary folds AR into EUR ----------------
	// dashboardRateAdapter (services/api/dashboard_handlers.go) reads
	// the live rate; the foldToBase walks the per-currency map. With
	// rate2 = 0.90, the 680 USD open AR should fold to 612 EUR.
	dashStore := dashboard.NewStore(h.pool).WithConverter(dashboardConverter{rates: rates})
	summary, err := dashStore.ComputeSummary(ctx, tn.ID)
	if err != nil {
		t.Fatalf("dashboard summary: %v", err)
	}
	if summary.BaseCurrency != "EUR" {
		t.Fatalf("dashboard base_currency: got %s want EUR", summary.BaseCurrency)
	}
	wantAREUR := 612.0
	if absDelta(summary.OutstandingAR-wantAREUR) > 0.01 {
		t.Fatalf("dashboard OutstandingAR (EUR): got %.4f want %.4f",
			summary.OutstandingAR, wantAREUR)
	}

	// --- Step 7: journal_lines grouped by currency, both amounts ----
	// A direct read on the persisted rows confirms that every
	// foreign-currency line carries both `currency` (the original
	// USD value) and `base_amount` (the EUR equivalent). Aggregates
	// per currency surface as a single map — used here to assert
	// USD vs EUR coverage rather than to re-derive the totals.
	byCurrency := groupLinesByCurrency(ctx, t, h, tn.ID)
	usd, ok := byCurrency["USD"]
	if !ok {
		t.Fatalf("expected USD bucket in journal_lines, got %+v", byCurrency)
	}
	if usd.totalCount == 0 || usd.withBaseAmount == 0 {
		t.Fatalf("USD bucket missing base_amount coverage: %+v", usd)
	}
	if usd.withBaseAmount != usd.totalCount {
		t.Fatalf("USD bucket has %d lines but only %d carry base_amount — every foreign-currency line must populate base_amount",
			usd.totalCount, usd.withBaseAmount)
	}
	eur, ok := byCurrency["EUR"]
	if !ok {
		t.Fatalf("expected EUR bucket in journal_lines (revaluation entries), got %+v", byCurrency)
	}
	if eur.totalCount == 0 {
		t.Fatalf("EUR bucket empty — revaluation entry should have written EUR lines")
	}
}

// --- Helpers ---------------------------------------------------------------

// dashboardConverter narrows ExchangeRateStore to the dashboard.Converter
// interface. It exercises the same ExchangeRateStore.Convert path the
// production dashboardRateAdapter at services/api/dashboard_handlers.go
// drives, so a successful assertion here also verifies the rate-store
// wiring the HTTP API depends on. The from==to short-circuit and the
// nil-rates guard are convenience guards local to this helper: the
// production adapter omits them because ExchangeRateStore.Convert
// itself handles same-currency pairs and the adapter's rate-store
// field is always populated by deps.go at startup — neither shortcut
// is structurally necessary, and adding them to production would just
// duplicate work already done by the rate store.
type dashboardConverter struct {
	rates *ledger.ExchangeRateStore
}

func (c dashboardConverter) Convert(ctx context.Context, tenantID uuid.UUID, amount float64, from, to string) (float64, bool) {
	if from == to {
		return amount, true
	}
	dec, err := c.rates.Convert(ctx, tenantID, decimal.NewFromFloat(amount), from, to, time.Now().UTC())
	if err != nil {
		return amount, false
	}
	out, _ := dec.Float64()
	return out, true
}

// paymentPayload bundles the fields createPaymentRecord serialises to
// the finance.payment KRecord.
type paymentPayload struct {
	Reference     string
	PaymentType   string
	PartyType     string
	PartyID       string
	Amount        decimal.Decimal
	Currency      string
	PaymentDate   string
	BankAccount   string
	ARAccountCode string
	APAccountCode string
	Allocations   []map[string]any
}

// createPaymentRecord inserts a draft finance.payment KRecord using
// the same JSON shape the POS poster builds (internal/sales/pos.go),
// returning the new record id so PostPayment can pick it up.
func createPaymentRecord(t *testing.T, h *harness, tenantID, actorID uuid.UUID, p paymentPayload) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	amtF, _ := p.Amount.Float64()
	data := map[string]any{
		"payment_type": p.PaymentType,
		"party_type":   p.PartyType,
		"party_id":     p.PartyID,
		"amount":       amtF,
		"currency":     p.Currency,
		"payment_date": p.PaymentDate,
		"reference":    p.Reference,
		"allocations":  p.Allocations,
		"status":       "draft",
		"bank_account": p.BankAccount,
	}
	if p.ARAccountCode != "" {
		data["ar_account_code"] = p.ARAccountCode
	}
	if p.APAccountCode != "" {
		data["ap_account_code"] = p.APAccountCode
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal payment: %v", err)
	}
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypePayment,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create payment record: %v", err)
	}
	return rec.ID
}

// createARInvoiceRecordWithDate is a thin wrapper around the shared
// createARInvoiceRecord helper that lets a caller pin the issue_date
// (so the rate lookup picks the seeded day1 rate rather than today's
// missing rate). It mirrors the marshalling rules of the original
// helper — decimals as JSON numbers, currency forced to USD — so the
// finance.ar_invoice schema validation passes.
func createARInvoiceRecordWithDate(t *testing.T, h *harness, tenantID, actorID uuid.UUID, number, customer string, subtotal, tax decimal.Decimal, taxAcct, issueDate string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	total := subtotal.Add(tax)
	subF, _ := subtotal.Float64()
	taxF, _ := tax.Float64()
	totalF, _ := total.Float64()
	due := issueDate
	if d, err := time.Parse("2006-01-02", issueDate); err == nil {
		due = d.AddDate(0, 0, 30).Format("2006-01-02")
	}
	data := map[string]any{
		"customer_id":          customer,
		"invoice_number":       number,
		"issue_date":           issueDate,
		"due_date":             due,
		"subtotal":             subF,
		"tax_amount":           taxF,
		"total":                totalF,
		"currency":             "USD",
		"status":               "draft",
		"ar_account_code":      "1100",
		"revenue_account_code": "4000",
		"tax_account_code":     taxAcct,
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal invoice: %v", err)
	}
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypeARInvoice,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create invoice record: %v", err)
	}
	return rec.ID
}

// mustBaseAmountEquals asserts that the journal line on `account` has
// base_amount equal to net × rate, where net is the signed
// (debit − credit) figure. Used to keep Step 3 / Step 4 assertions
// compact without losing readability of the expected math.
func mustBaseAmountEquals(t *testing.T, entry *ledger.JournalEntry, account string, signedNet, rate decimal.Decimal) {
	t.Helper()
	line := findLine(t, entry, account)
	if line.BaseAmount == nil {
		t.Fatalf("line %s: base_amount is nil (rates store likely not wired)", account)
	}
	want := signedNet.Mul(rate)
	if !line.BaseAmount.Equal(want) {
		t.Fatalf("line %s base_amount: got %s want %s (net %s × rate %s)",
			account, line.BaseAmount, want, signedNet, rate)
	}
}

// findLine returns the first journal_line on `account` in `entry`,
// failing the test with a contextual message if no such line exists.
// Returning a zero-value JournalLine on miss would cascade into
// misleading downstream assertions (e.g. mustBaseAmountEquals would
// surface `base_amount is nil (rates store likely not wired)` for
// what is really a missing-account problem; the revaluation block
// would compare `decimal.Zero` against the expected debit/credit
// figure and report a wrong-amount failure when the line simply isn't
// there). Fail-fast turns those red herrings into the actual root
// cause.
func findLine(t *testing.T, entry *ledger.JournalEntry, account string) ledger.JournalLine {
	t.Helper()
	for _, l := range entry.Lines {
		if l.AccountCode == account {
			return l
		}
	}
	t.Fatalf("journal entry %s: no line for account %s (have %s)",
		entry.ID, account, summariseAccounts(entry.Lines))
	return ledger.JournalLine{}
}

// summariseAccounts returns a comma-separated list of the account
// codes present on the given lines, used only for findLine's failure
// diagnostic so the assertion error tells the reader exactly which
// accounts WERE posted on the entry.
func summariseAccounts(lines []ledger.JournalLine) string {
	if len(lines) == 0 {
		return "(no lines)"
	}
	accounts := make([]string, 0, len(lines))
	for _, l := range lines {
		accounts = append(accounts, l.AccountCode)
	}
	return strings.Join(accounts, ", ")
}

// listEntriesBySource reloads every journal entry that posted under
// the supplied source_ktype, hydrating its lines. The harness's
// shared records / ledger store don't expose a single helper for this
// (ListJournalEntries with a filter only returns headers), so we
// fetch headers + line rows in a single WithTenantTx pass.
func listEntriesBySource(t *testing.T, h *harness, tenantID uuid.UUID, sourceKType string) []ledger.JournalEntry {
	t.Helper()
	var out []ledger.JournalEntry
	err := dbutil.WithTenantTx(context.Background(), h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, posted_at, COALESCE(memo, '')
			   FROM journal_entries
			  WHERE tenant_id = $1 AND source_ktype = $2
			  ORDER BY posted_at`,
			tenantID, sourceKType,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		var headers []ledger.JournalEntry
		for rows.Next() {
			var je ledger.JournalEntry
			je.TenantID = tenantID
			je.SourceKType = sourceKType
			if err := rows.Scan(&je.ID, &je.PostedAt, &je.Memo); err != nil {
				return err
			}
			headers = append(headers, je)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range headers {
			lineRows, err := tx.Query(ctx,
				`SELECT account_code, debit, credit, currency,
				        COALESCE(memo, ''), base_amount
				   FROM journal_lines
				  WHERE tenant_id = $1 AND entry_id = $2
				  ORDER BY id`,
				tenantID, headers[i].ID,
			)
			if err != nil {
				return err
			}
			for lineRows.Next() {
				var l ledger.JournalLine
				l.TenantID = tenantID
				l.EntryID = headers[i].ID
				var base *decimal.Decimal
				if err := lineRows.Scan(&l.AccountCode, &l.Debit, &l.Credit, &l.Currency, &l.Memo, &base); err != nil {
					lineRows.Close()
					return err
				}
				l.BaseAmount = base
				headers[i].Lines = append(headers[i].Lines, l)
			}
			// Mirror the outer-loop pattern at line 536: a transient
			// DB error mid-iteration would otherwise be silently
			// swallowed by lineRows.Close(), surfacing as a misleading
			// "missing line" assertion downstream instead of the
			// actual driver error.
			lineErr := lineRows.Err()
			lineRows.Close()
			if lineErr != nil {
				return lineErr
			}
		}
		out = headers
		return nil
	})
	if err != nil {
		t.Fatalf("list entries by source %s: %v", sourceKType, err)
	}
	return out
}

// currencyBucket aggregates the journal_lines breakdown we assert on
// in Step 7. `withBaseAmount` is the count of rows where base_amount
// IS NOT NULL; for any non-base-currency bucket this must equal
// totalCount, otherwise the conversion is broken for some posts.
type currencyBucket struct {
	currency       string
	totalCount     int
	withBaseAmount int
	sumDebit       decimal.Decimal
	sumCredit      decimal.Decimal
}

// groupLinesByCurrency scans journal_lines for the tenant and returns
// per-currency stats. Direct DB read instead of a store helper so the
// assertion captures the persisted shape (including the nullable
// base_amount column) rather than a derived in-memory aggregation.
func groupLinesByCurrency(ctx context.Context, t *testing.T, h *harness, tenantID uuid.UUID) map[string]currencyBucket {
	t.Helper()
	out := make(map[string]currencyBucket)
	err := dbutil.WithTenantTx(ctx, h.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT currency,
			        COUNT(*),
			        COUNT(base_amount),
			        COALESCE(SUM(debit), 0),
			        COALESCE(SUM(credit), 0)
			   FROM journal_lines
			  WHERE tenant_id = $1
			  GROUP BY currency`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b currencyBucket
			if err := rows.Scan(&b.currency, &b.totalCount, &b.withBaseAmount, &b.sumDebit, &b.sumCredit); err != nil {
				return err
			}
			out[b.currency] = b
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("group journal_lines by currency: %v", err)
	}
	return out
}

// indexRevaluationByOpenAccount keys each FX revaluation entry by the
// non-gain/loss leg ("the open account"). Either leg can be the
// debit or credit depending on the delta's sign, so the helper picks
// whichever side is not the unrealised-gain / unrealised-loss
// account.
func indexRevaluationByOpenAccount(t *testing.T, entries []ledger.JournalEntry) map[string]ledger.JournalEntry {
	t.Helper()
	out := make(map[string]ledger.JournalEntry, len(entries))
	for _, e := range entries {
		var openAcct string
		for _, l := range e.Lines {
			if l.AccountCode != ledger.AccountCodeUnrealizedFXGain &&
				l.AccountCode != ledger.AccountCodeUnrealizedFXLoss {
				openAcct = l.AccountCode
				break
			}
		}
		if openAcct == "" {
			t.Fatalf("revaluation entry %s has no open-account leg: %+v", e.ID, e.Lines)
		}
		out[openAcct] = e
	}
	return out
}

// absDelta is the float64 absolute-value helper used by the dashboard
// step-6 assertion. Named to avoid colliding with `abs` defined in
// phase_i_test.go.
func absDelta(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
