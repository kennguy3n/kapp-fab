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
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/finance"
	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// newTenantForFinance provisions a fresh tenant, registers the finance
// KTypes, and seeds a minimal chart of accounts covering both the AR
// and AP posting paths. Returning the same harness shared with Phase
// A/B tests keeps the integration surface uniform.
func newTenantForFinance(t *testing.T, h *harness) (*tenant.Tenant, *ledger.PGStore, *ledger.InvoicePoster) {
	t.Helper()
	ctx := context.Background()

	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("phasec"), Name: "Phase C Co", Cell: "test", Plan: "free",
	})
	if err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := finance.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register finance ktypes: %v", err)
	}

	store := ledger.NewPGStore(h.pool, h.publisher, h.auditor)
	poster := ledger.NewInvoicePoster(store, h.records)

	// Seed a minimal chart of accounts that covers sales, purchasing,
	// and tax postings. Every test that posts reuses these codes.
	seed := []ledger.Account{
		{TenantID: tn.ID, Code: "1100", Name: "Accounts Receivable", Type: ledger.AccountTypeAsset, Active: true},
		{TenantID: tn.ID, Code: "2100", Name: "Accounts Payable", Type: ledger.AccountTypeLiability, Active: true},
		{TenantID: tn.ID, Code: "2200", Name: "Tax Payable", Type: ledger.AccountTypeLiability, Active: true},
		{TenantID: tn.ID, Code: "4000", Name: "Revenue", Type: ledger.AccountTypeRevenue, Active: true},
		{TenantID: tn.ID, Code: "5000", Name: "Cost of Goods Sold", Type: ledger.AccountTypeExpense, Active: true},
		{TenantID: tn.ID, Code: "6000", Name: "Operating Expense", Type: ledger.AccountTypeExpense, Active: true},
	}
	for _, a := range seed {
		if _, err := store.CreateAccount(ctx, a); err != nil {
			t.Fatalf("seed account %s: %v", a.Code, err)
		}
	}
	return tn, store, poster
}

// createARInvoiceRecord inserts a draft finance.ar_invoice KRecord with
// the supplied header + totals. Returns the created record id.
func createARInvoiceRecord(t *testing.T, h *harness, tenantID, actorID uuid.UUID, number, customer string, subtotal, tax decimal.Decimal, taxAcct string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	total := subtotal.Add(tax)
	// KType validation treats `number` fields as JSON numbers (not
	// stringified decimals). We marshal the schema-visible totals as
	// float64 so the KRecord store accepts them; the ledger receiver
	// uses decimal.Decimal on its own struct, where
	// shopspring/decimal's UnmarshalJSON accepts both numeric and
	// string encodings.
	subF, _ := subtotal.Float64()
	taxF, _ := tax.Float64()
	totalF, _ := total.Float64()
	data := map[string]any{
		"customer_id":          customer,
		"invoice_number":       number,
		"issue_date":           "2026-01-15",
		"due_date":             "2026-02-14",
		"subtotal":             subF,
		"tax_amount":           taxF,
		"total":                totalF,
		"currency":             "USD",
		"status":               "draft",
		"ar_account_code":      "1100",
		"revenue_account_code": "4000",
	}
	if taxAcct != "" {
		data["tax_account_code"] = taxAcct
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

// createAPBillRecord is the AP analog of createARInvoiceRecord.
func createAPBillRecord(t *testing.T, h *harness, tenantID, actorID uuid.UUID, number, supplier string, subtotal, tax decimal.Decimal, taxAcct string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	total := subtotal.Add(tax)
	subF, _ := subtotal.Float64()
	taxF, _ := tax.Float64()
	totalF, _ := total.Float64()
	data := map[string]any{
		"supplier_id":          supplier,
		"bill_number":          number,
		"issue_date":           "2026-01-20",
		"due_date":             "2026-02-19",
		"subtotal":             subF,
		"tax_amount":           taxF,
		"total":                totalF,
		"currency":             "USD",
		"status":               "draft",
		"ap_account_code":      "2100",
		"expense_account_code": "6000",
	}
	if taxAcct != "" {
		data["tax_account_code"] = taxAcct
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal bill: %v", err)
	}
	rec, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     finance.KTypeAPBill,
		Data:      body,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("create bill record: %v", err)
	}
	return rec.ID
}

// TestSalesInvoicePostsBalancedJournal exercises the AR posting path:
// draft invoice → PostSalesInvoice → balanced JE + patched record +
// lifecycle event. The three-leg journal (AR / Revenue / Tax) is the
// canonical shape a tax-bearing invoice produces.
func TestSalesInvoicePostsBalancedJournal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, poster := newTenantForFinance(t, h)

	actor := uuid.New()
	customer := uuid.NewString()
	invoiceID := createARInvoiceRecord(t, h, tn.ID, actor, "INV-1001", customer,
		decimal.NewFromInt(1000), decimal.NewFromInt(100), "2200")

	entry, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor)
	if err != nil {
		t.Fatalf("post sales invoice: %v", err)
	}
	if entry == nil || entry.ID == uuid.Nil {
		t.Fatalf("posted entry missing id: %+v", entry)
	}
	if entry.SourceKType != finance.KTypeARInvoice || entry.SourceID == nil || *entry.SourceID != invoiceID {
		t.Fatalf("source linkage wrong: ktype=%q id=%v", entry.SourceKType, entry.SourceID)
	}

	// Journal must be balanced (debits == credits) with exactly the
	// three legs: AR debit, Revenue credit, Tax credit.
	var debit, credit decimal.Decimal
	legs := map[string]struct{ debit, credit decimal.Decimal }{}
	for _, line := range entry.Lines {
		debit = debit.Add(line.Debit)
		credit = credit.Add(line.Credit)
		legs[line.AccountCode] = struct{ debit, credit decimal.Decimal }{line.Debit, line.Credit}
	}
	if !debit.Equal(credit) {
		t.Fatalf("journal unbalanced: debits=%s credits=%s", debit, credit)
	}
	if got := legs["1100"].debit; !got.Equal(decimal.NewFromInt(1100)) {
		t.Fatalf("AR debit = %s; want 1100", got)
	}
	if got := legs["4000"].credit; !got.Equal(decimal.NewFromInt(1000)) {
		t.Fatalf("Revenue credit = %s; want 1000", got)
	}
	if got := legs["2200"].credit; !got.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("Tax credit = %s; want 100", got)
	}

	// Invoice record patched to status=posted + journal_entry_id set.
	rec, err := h.records.Get(ctx, tn.ID, invoiceID)
	if err != nil {
		t.Fatalf("reload invoice: %v", err)
	}
	var patched struct {
		Status         string `json:"status"`
		JournalEntryID string `json:"journal_entry_id"`
	}
	if err := json.Unmarshal(rec.Data, &patched); err != nil {
		t.Fatalf("decode invoice: %v", err)
	}
	if patched.Status != "posted" {
		t.Fatalf("status = %q; want posted", patched.Status)
	}
	if patched.JournalEntryID != entry.ID.String() {
		t.Fatalf("journal_entry_id = %q; want %s", patched.JournalEntryID, entry.ID)
	}

	// Trial balance is a secondary guard — not the main assertion, but
	// covers the case where the JE commit succeeded at the row level
	// but the store mis-applied sums.
	tb, err := store.TrialBalance(ctx, tn.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	if !tb.Residual.IsZero() {
		t.Fatalf("trial balance residual = %s; want 0", tb.Residual)
	}

	// Posting produces both the generic JE event and the AR-lifecycle
	// event. The audit trail gets a corresponding pair.
	counts, err := eventCountsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if counts["finance.journal.posted"] != 1 {
		t.Fatalf("finance.journal.posted = %d; want 1 (%v)", counts["finance.journal.posted"], counts)
	}
	if counts["finance.sales_invoice.posted"] != 1 {
		t.Fatalf("finance.sales_invoice.posted = %d; want 1 (%v)", counts["finance.sales_invoice.posted"], counts)
	}
}

// TestPurchaseBillPostsBalancedJournal is the AP analog of the sales
// invoice test: Debit Expense / Debit Tax / Credit AP.
func TestPurchaseBillPostsBalancedJournal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, _, poster := newTenantForFinance(t, h)

	actor := uuid.New()
	supplier := uuid.NewString()
	billID := createAPBillRecord(t, h, tn.ID, actor, "BILL-2001", supplier,
		decimal.NewFromInt(500), decimal.NewFromInt(50), "2200")

	entry, err := poster.PostPurchaseBill(ctx, tn.ID, billID, actor)
	if err != nil {
		t.Fatalf("post purchase bill: %v", err)
	}
	if entry.SourceKType != finance.KTypeAPBill || entry.SourceID == nil || *entry.SourceID != billID {
		t.Fatalf("source linkage wrong: ktype=%q id=%v", entry.SourceKType, entry.SourceID)
	}

	var debit, credit decimal.Decimal
	legs := map[string]struct{ debit, credit decimal.Decimal }{}
	for _, line := range entry.Lines {
		debit = debit.Add(line.Debit)
		credit = credit.Add(line.Credit)
		legs[line.AccountCode] = struct{ debit, credit decimal.Decimal }{line.Debit, line.Credit}
	}
	if !debit.Equal(credit) {
		t.Fatalf("journal unbalanced: debits=%s credits=%s", debit, credit)
	}
	if got := legs["6000"].debit; !got.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("Expense debit = %s; want 500", got)
	}
	if got := legs["2200"].debit; !got.Equal(decimal.NewFromInt(50)) {
		t.Fatalf("Tax debit = %s; want 50", got)
	}
	if got := legs["2100"].credit; !got.Equal(decimal.NewFromInt(550)) {
		t.Fatalf("AP credit = %s; want 550", got)
	}

	rec, err := h.records.Get(ctx, tn.ID, billID)
	if err != nil {
		t.Fatalf("reload bill: %v", err)
	}
	var patched struct {
		Status         string `json:"status"`
		JournalEntryID string `json:"journal_entry_id"`
	}
	if err := json.Unmarshal(rec.Data, &patched); err != nil {
		t.Fatalf("decode bill: %v", err)
	}
	if patched.Status != "posted" || patched.JournalEntryID != entry.ID.String() {
		t.Fatalf("bill not patched: status=%q je=%q", patched.Status, patched.JournalEntryID)
	}

	counts, err := eventCountsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if counts["finance.ap_bill.posted"] != 1 {
		t.Fatalf("finance.ap_bill.posted = %d; want 1", counts["finance.ap_bill.posted"])
	}
}

// TestTrialBalanceSumsToZero posts several unrelated journal entries
// directly (no invoice wrapper) and asserts the trial balance residual
// stays at zero — i.e. every individual entry satisfies the
// debit-equals-credit invariant and the report aggregation is correct.
func TestTrialBalanceSumsToZero(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, _ := newTenantForFinance(t, h)

	actor := uuid.New()
	// Three distinct postings to exercise account-level fan-out.
	postings := [][]ledger.JournalLine{
		{
			{AccountCode: "1100", Debit: decimal.NewFromInt(300), Currency: "USD"},
			{AccountCode: "4000", Credit: decimal.NewFromInt(300), Currency: "USD"},
		},
		{
			{AccountCode: "6000", Debit: decimal.NewFromInt(150), Currency: "USD"},
			{AccountCode: "2100", Credit: decimal.NewFromInt(150), Currency: "USD"},
		},
		{
			{AccountCode: "5000", Debit: decimal.NewFromInt(75), Currency: "USD"},
			{AccountCode: "2200", Credit: decimal.NewFromInt(75), Currency: "USD"},
		},
	}
	for i, lines := range postings {
		if _, err := store.PostJournalEntry(ctx, ledger.JournalEntry{
			TenantID:  tn.ID,
			Memo:      fmt.Sprintf("test-%d", i),
			CreatedBy: actor,
			Lines:     lines,
		}); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}

	tb, err := store.TrialBalance(ctx, tn.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	if !tb.Residual.IsZero() {
		t.Fatalf("residual = %s; want 0 (debit=%s credit=%s)", tb.Residual, tb.TotalDebit, tb.TotalCredit)
	}
	if !tb.TotalDebit.Equal(decimal.NewFromInt(525)) {
		t.Fatalf("total debit = %s; want 525", tb.TotalDebit)
	}
	if !tb.TotalCredit.Equal(decimal.NewFromInt(525)) {
		t.Fatalf("total credit = %s; want 525", tb.TotalCredit)
	}
}

// TestPeriodLockoutRejectsEdits locks a fiscal period and verifies
// subsequent postings into that window fail with ErrPeriodLocked.
// Postings outside the window still succeed.
func TestPeriodLockoutRejectsEdits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, _ := newTenantForFinance(t, h)

	actor := uuid.New()
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	if _, err := store.UpsertPeriod(ctx, ledger.FiscalPeriod{
		TenantID:    tn.ID,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
	}); err != nil {
		t.Fatalf("upsert period: %v", err)
	}
	if _, err := store.LockPeriod(ctx, tn.ID, periodStart, actor); err != nil {
		t.Fatalf("lock period: %v", err)
	}

	inside := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	if _, err := store.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID: tn.ID, PostedAt: inside, Memo: "locked-window", CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "1100", Debit: decimal.NewFromInt(10), Currency: "USD"},
			{AccountCode: "4000", Credit: decimal.NewFromInt(10), Currency: "USD"},
		},
	}); !errors.Is(err, ledger.ErrPeriodLocked) {
		t.Fatalf("posting inside locked period: want ErrPeriodLocked, got %v", err)
	}

	// Outside the locked period: posting must succeed.
	outside := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)
	if _, err := store.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID: tn.ID, PostedAt: outside, Memo: "open-window", CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "1100", Debit: decimal.NewFromInt(20), Currency: "USD"},
			{AccountCode: "4000", Credit: decimal.NewFromInt(20), Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("posting outside locked period: %v", err)
	}

	// A lock event + audit entry must have been emitted when the period
	// was locked. This guards against the update committing without
	// the side effects that finance dashboards subscribe to.
	counts, err := eventCountsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if counts["finance.period.locked"] != 1 {
		t.Fatalf("finance.period.locked = %d; want 1", counts["finance.period.locked"])
	}
	actions, err := auditActionsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, a := range actions {
		if a == "finance.period.locked" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("audit missing finance.period.locked (%v)", actions)
	}
}

// TestAuditLogCapturesPostings asserts that every ledger posting —
// direct journal entry, sales invoice, purchase bill — writes an
// audit_log row that identifies the source KType / id so forensic
// queries can reconstruct the business operation behind each JE.
func TestAuditLogCapturesPostings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, poster := newTenantForFinance(t, h)

	actor := uuid.New()
	// 1. Direct journal entry.
	if _, err := store.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID: tn.ID, Memo: "manual", CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "1100", Debit: decimal.NewFromInt(10), Currency: "USD"},
			{AccountCode: "4000", Credit: decimal.NewFromInt(10), Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("post manual: %v", err)
	}

	// 2. Sales invoice.
	invoiceID := createARInvoiceRecord(t, h, tn.ID, actor, "INV-3001", uuid.NewString(),
		decimal.NewFromInt(200), decimal.Zero, "")
	if _, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor); err != nil {
		t.Fatalf("post invoice: %v", err)
	}

	// 3. Purchase bill.
	billID := createAPBillRecord(t, h, tn.ID, actor, "BILL-3001", uuid.NewString(),
		decimal.NewFromInt(80), decimal.Zero, "")
	if _, err := poster.PostPurchaseBill(ctx, tn.ID, billID, actor); err != nil {
		t.Fatalf("post bill: %v", err)
	}

	actions, err := auditActionsForTenant(ctx, h.pool, tn.ID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	// All three postings hit the generic journal audit action; the
	// invoice/bill legs additionally emit their own lifecycle action.
	required := []string{
		"finance.journal.posted",
		"finance.sales_invoice.posted",
		"finance.ap_bill.posted",
	}
	counts := map[string]int{}
	for _, a := range actions {
		counts[a]++
	}
	if counts["finance.journal.posted"] < 3 {
		t.Fatalf("finance.journal.posted audit count = %d; want >= 3 (%v)", counts["finance.journal.posted"], actions)
	}
	for _, want := range required[1:] {
		if counts[want] != 1 {
			t.Fatalf("%s audit count = %d; want 1 (%v)", want, counts[want], actions)
		}
	}
}

// TestRLSIsolatesFinanceData provisions two tenants, seeds accounts +
// journal entries in tenant A, and verifies tenant B sees none of
// them. This is the Phase C analog of TestRLSDealIsolation: the
// finance typed tables inherit the same tenant_isolation RLS policy
// applied across the kernel.
func TestRLSIsolatesFinanceData(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	aTN, aStore, _ := newTenantForFinance(t, h)
	bTN, bStore, _ := newTenantForFinance(t, h)

	actor := uuid.New()
	if _, err := aStore.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID: aTN.ID, Memo: "A-only", CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "1100", Debit: decimal.NewFromInt(42), Currency: "USD"},
			{AccountCode: "4000", Credit: decimal.NewFromInt(42), Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("post A: %v", err)
	}

	// Tenant B can only see its own chart-of-accounts seeds (six rows);
	// none of tenant A's entries or accounts leak through.
	bAccounts, err := bStore.ListAccounts(ctx, bTN.ID, ledger.AccountFilter{})
	if err != nil {
		t.Fatalf("list B accounts: %v", err)
	}
	for _, a := range bAccounts {
		if a.TenantID != bTN.ID {
			t.Fatalf("RLS leak: B saw account from tenant %s", a.TenantID)
		}
	}

	bEntries, err := bStore.ListJournalEntries(ctx, bTN.ID, ledger.JournalEntryFilter{})
	if err != nil {
		t.Fatalf("list B entries: %v", err)
	}
	if len(bEntries) != 0 {
		t.Fatalf("RLS leak: B saw %d journal entries (want 0)", len(bEntries))
	}

	// A still sees its posting.
	aEntries, err := aStore.ListJournalEntries(ctx, aTN.ID, ledger.JournalEntryFilter{})
	if err != nil {
		t.Fatalf("list A entries: %v", err)
	}
	if len(aEntries) != 1 {
		t.Fatalf("A should see 1 entry, saw %d", len(aEntries))
	}

	// Trial balance run against B must be empty (all zeros) — the RLS
	// filter keeps the aggregate query from accidentally joining A's
	// rows. We still expect rows for B's own chart of accounts (with
	// zero debit/credit).
	tb, err := bStore.TrialBalance(ctx, bTN.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("trial balance B: %v", err)
	}
	for _, r := range tb.Rows {
		if !r.Debit.IsZero() || !r.Credit.IsZero() {
			t.Fatalf("RLS leak: B account %s shows non-zero activity (debit=%s credit=%s)", r.AccountCode, r.Debit, r.Credit)
		}
	}
}

// TestTrialBalanceIncludesZeroBalanceAccountsWhenLinesOutOfRange pins
// the fix for the previous `LEFT JOIN journal_entries ... AND
// posted_at <= $2` shape, which dropped chart-of-accounts rows whose
// only journal lines were posted strictly after asOf. Those accounts
// legitimately have a zero balance at the as-of date and must still
// appear on the trial balance.
func TestTrialBalanceIncludesZeroBalanceAccountsWhenLinesOutOfRange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, _ := newTenantForFinance(t, h)

	actor := uuid.New()
	asOf := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)

	// Post the entry after asOf. "1100" (AR) and "4000" (Revenue) are
	// seeded in the chart; both must still surface on the TB as zero.
	future := asOf.Add(24 * time.Hour)
	if _, err := store.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID: tn.ID, PostedAt: future, Memo: "future", CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "1100", Debit: decimal.NewFromInt(500), Currency: "USD"},
			{AccountCode: "4000", Credit: decimal.NewFromInt(500), Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("post future: %v", err)
	}

	tb, err := store.TrialBalance(ctx, tn.ID, asOf)
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	rows := map[string]ledger.TrialBalanceRow{}
	for _, r := range tb.Rows {
		rows[r.AccountCode] = r
	}
	for _, code := range []string{"1100", "4000", "2100", "6000"} {
		r, ok := rows[code]
		if !ok {
			t.Fatalf("trial balance missing account %s; got rows=%v", code, tb.Rows)
		}
		if !r.Debit.IsZero() || !r.Credit.IsZero() {
			t.Fatalf("account %s expected zero activity (asOf before JE); got debit=%s credit=%s", code, r.Debit, r.Credit)
		}
	}
	if !tb.Residual.IsZero() {
		t.Fatalf("residual = %s; want 0", tb.Residual)
	}
}

// TestIncomeStatementIncludesZeroBalanceAccountsOutsideRange is the
// IncomeStatement analog of the trial-balance regression: revenue and
// expense accounts whose only activity sits outside [from,to] must
// still appear with a zero net amount so the statement reflects the
// full chart.
func TestIncomeStatementIncludesZeroBalanceAccountsOutsideRange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, _ := newTenantForFinance(t, h)

	actor := uuid.New()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)

	// Activity one day after `to` — must be excluded from the statement.
	if _, err := store.PostJournalEntry(ctx, ledger.JournalEntry{
		TenantID: tn.ID, PostedAt: to.Add(24 * time.Hour), Memo: "feb", CreatedBy: actor,
		Lines: []ledger.JournalLine{
			{AccountCode: "6000", Debit: decimal.NewFromInt(300), Currency: "USD"},
			{AccountCode: "2100", Credit: decimal.NewFromInt(300), Currency: "USD"},
		},
	}); err != nil {
		t.Fatalf("post out-of-range: %v", err)
	}

	is, err := store.IncomeStatement(ctx, tn.ID, from, to)
	if err != nil {
		t.Fatalf("income statement: %v", err)
	}
	codes := map[string]ledger.IncomeStatementRow{}
	for _, r := range is.Revenue {
		codes[r.AccountCode] = r
	}
	for _, r := range is.Expense {
		codes[r.AccountCode] = r
	}
	// All revenue + expense accounts from the seeded chart must be
	// present even though none had in-range activity.
	for _, code := range []string{"4000", "5000", "6000"} {
		r, ok := codes[code]
		if !ok {
			t.Fatalf("income statement missing account %s; got revenue=%+v expense=%+v", code, is.Revenue, is.Expense)
		}
		if !r.Amount.IsZero() {
			t.Fatalf("account %s amount = %s; want 0 (activity is outside range)", code, r.Amount)
		}
	}
	if !is.NetIncome.IsZero() {
		t.Fatalf("net income = %s; want 0", is.NetIncome)
	}
}

// TestPostSalesInvoiceIsIdempotent simulates the retry-after-partial-
// failure path: the first call posts the JE but we bypass the record
// patch by reverting the record's status back to draft, then retry the
// poster. With the fix in place the retry must (a) not insert a second
// JE and (b) must succeed — reusing the existing entry.
func TestPostSalesInvoiceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, poster := newTenantForFinance(t, h)

	actor := uuid.New()
	customer := uuid.NewString()
	invoiceID := createARInvoiceRecord(t, h, tn.ID, actor, "INV-RETRY-1", customer,
		decimal.NewFromInt(400), decimal.Zero, "")

	firstEntry, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}

	// Simulate a partial-failure scenario where the JE committed but
	// the record patch was rolled back: rewind the record to draft and
	// clear journal_entry_id, then replay the post action.
	rec, err := h.records.Get(ctx, tn.ID, invoiceID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	rewind, _ := json.Marshal(map[string]any{
		"status":           "draft",
		"journal_entry_id": "",
	})
	if _, err := h.records.Update(ctx, record.KRecord{
		ID: rec.ID, TenantID: tn.ID, Version: rec.Version,
		Data: rewind, UpdatedBy: &actor,
	}); err != nil {
		t.Fatalf("rewind record: %v", err)
	}

	// Retry. Must reuse the original JE rather than double-post.
	retryEntry, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor)
	if err != nil {
		t.Fatalf("retry post: %v", err)
	}
	if retryEntry.ID != firstEntry.ID {
		t.Fatalf("retry posted new JE %s; want reuse of %s", retryEntry.ID, firstEntry.ID)
	}

	// Exactly one JE ever existed for this invoice.
	srcID := invoiceID
	entries, err := store.ListJournalEntries(ctx, tn.ID, ledger.JournalEntryFilter{
		SourceKType: finance.KTypeARInvoice,
		SourceID:    &srcID,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d JEs for invoice; want exactly 1 (IDs %+v)", len(entries), entries)
	}

	// Final record state: status posted, journal_entry_id points at the
	// (single) JE.
	final, err := h.records.Get(ctx, tn.ID, invoiceID)
	if err != nil {
		t.Fatalf("reload final: %v", err)
	}
	var fields struct {
		Status         string `json:"status"`
		JournalEntryID string `json:"journal_entry_id"`
	}
	if err := json.Unmarshal(final.Data, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fields.Status != "posted" || fields.JournalEntryID != firstEntry.ID.String() {
		t.Fatalf("final record: status=%q je=%q; want posted / %s", fields.Status, fields.JournalEntryID, firstEntry.ID)
	}
}

// TestPostPurchaseBillIsIdempotent mirrors the sales-invoice retry
// test for AP bills.
func TestPostPurchaseBillIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, poster := newTenantForFinance(t, h)

	actor := uuid.New()
	supplier := uuid.NewString()
	billID := createAPBillRecord(t, h, tn.ID, actor, "BILL-RETRY-1", supplier,
		decimal.NewFromInt(220), decimal.Zero, "")

	firstEntry, err := poster.PostPurchaseBill(ctx, tn.ID, billID, actor)
	if err != nil {
		t.Fatalf("first post: %v", err)
	}

	rec, err := h.records.Get(ctx, tn.ID, billID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	rewind, _ := json.Marshal(map[string]any{
		"status":           "draft",
		"journal_entry_id": "",
	})
	if _, err := h.records.Update(ctx, record.KRecord{
		ID: rec.ID, TenantID: tn.ID, Version: rec.Version,
		Data: rewind, UpdatedBy: &actor,
	}); err != nil {
		t.Fatalf("rewind record: %v", err)
	}

	retryEntry, err := poster.PostPurchaseBill(ctx, tn.ID, billID, actor)
	if err != nil {
		t.Fatalf("retry post: %v", err)
	}
	if retryEntry.ID != firstEntry.ID {
		t.Fatalf("retry posted new JE %s; want reuse of %s", retryEntry.ID, firstEntry.ID)
	}

	srcID := billID
	entries, err := store.ListJournalEntries(ctx, tn.ID, ledger.JournalEntryFilter{
		SourceKType: finance.KTypeAPBill,
		SourceID:    &srcID,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d JEs for bill; want exactly 1", len(entries))
	}
}

// TestPostSalesInvoiceConcurrentPostersOnlyOneWins races two
// PostSalesInvoice callers against the same draft invoice. The partial
// unique index on (tenant_id, source_ktype, source_id) plus the
// GetJournalEntryBySource pre-check/reload path must ensure that at
// most one JE is ever inserted; both callers return successfully and
// agree on the same JE id.
func TestPostSalesInvoiceConcurrentPostersOnlyOneWins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tn, store, poster := newTenantForFinance(t, h)

	actor := uuid.New()
	customer := uuid.NewString()
	invoiceID := createARInvoiceRecord(t, h, tn.ID, actor, "INV-RACE-1", customer,
		decimal.NewFromInt(777), decimal.Zero, "")

	type result struct {
		entryID uuid.UUID
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			entry, err := poster.PostSalesInvoice(ctx, tn.ID, invoiceID, actor)
			if entry != nil {
				results <- result{entryID: entry.ID, err: err}
			} else {
				results <- result{err: err}
			}
		}()
	}

	var ids []uuid.UUID
	var errs []error
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			errs = append(errs, r.err)
		}
		if r.entryID != uuid.Nil {
			ids = append(ids, r.entryID)
		}
	}

	// Both goroutines either succeed (agreeing on the same JE) or one
	// returns record.ErrVersionConflict because the other already
	// patched the record first — both outcomes are acceptable. What's
	// NOT acceptable is two distinct JEs in the ledger.
	srcID := invoiceID
	entries, err := store.ListJournalEntries(ctx, tn.ID, ledger.JournalEntryFilter{
		SourceKType: finance.KTypeARInvoice,
		SourceID:    &srcID,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("concurrent posters produced %d JEs; want exactly 1 (errs=%v ids=%v)", len(entries), errs, ids)
	}
	winnerID := entries[0].ID
	for _, id := range ids {
		if id != winnerID {
			t.Fatalf("poster returned JE %s but only %s exists in the ledger", id, winnerID)
		}
	}
}
