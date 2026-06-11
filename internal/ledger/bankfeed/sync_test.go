package bankfeed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

func scheduledActionStub() scheduler.ScheduledAction {
	return scheduler.ScheduledAction{ActionType: ActionTypeBankFeedSync}
}

// ---- in-memory fakes -------------------------------------------------

type fakeProvider struct {
	name   string
	raw    []RawTransaction
	cursor string
	err    error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) InitiateConnect(context.Context, uuid.UUID, uuid.UUID, string) (string, error) {
	return "", nil
}
func (f *fakeProvider) CompleteConnect(context.Context, uuid.UUID, string) (*Connection, error) {
	return nil, nil
}
func (f *fakeProvider) FetchTransactions(_ context.Context, _ *Connection, _ time.Time) ([]RawTransaction, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.raw, f.cursor, nil
}
func (f *fakeProvider) Disconnect(context.Context, *Connection) error { return nil }

type fakeStore struct {
	// dedup keyed by external_ref to mimic ON CONFLICT DO NOTHING.
	seen     map[string]bool
	gotLines []ledger.BankTransaction
}

func (s *fakeStore) SyncBankTransactions(_ context.Context, _, _ uuid.UUID, lines []ledger.BankTransaction) ([]ledger.BankTransaction, error) {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	inserted := make([]ledger.BankTransaction, 0, len(lines))
	for i := range lines {
		ln := lines[i]
		if ln.ExternalRef == "" || s.seen[ln.ExternalRef] {
			continue
		}
		s.seen[ln.ExternalRef] = true
		if ln.ID == uuid.Nil {
			ln.ID = uuid.New()
		}
		inserted = append(inserted, ln)
	}
	s.gotLines = append(s.gotLines, inserted...)
	return inserted, nil
}

type fakeConns struct {
	active       []Connection
	advanced     map[uuid.UUID]string
	markedErrors map[uuid.UUID]string
}

func (c *fakeConns) ListActiveConnections(context.Context, uuid.UUID) ([]Connection, error) {
	return c.active, nil
}
func (c *fakeConns) AdvanceCursor(_ context.Context, _, id uuid.UUID, cursor string, _ time.Time) error {
	if c.advanced == nil {
		c.advanced = map[uuid.UUID]string{}
	}
	c.advanced[id] = cursor
	return nil
}
func (c *fakeConns) MarkError(_ context.Context, _, id uuid.UUID, msg string) error {
	if c.markedErrors == nil {
		c.markedErrors = map[uuid.UUID]string{}
	}
	c.markedErrors[id] = msg
	return nil
}

type fakeMatcher struct {
	// suggestions returned per transaction id, in order.
	byTxn    map[uuid.UUID][]ledger.Suggestion
	accepted []uuid.UUID
	queried  []uuid.UUID // txn ids SuggestMatches was called with, in order
	// dupOf maps a txn id to the earlier line DetectDuplicate should
	// report it as duplicating (nil/absent = not a duplicate).
	dupOf map[uuid.UUID]uuid.UUID
	// transferFor maps a txn id to the pair DetectTransfer should report
	// (absent = no transfer). Lets a test drive the resolve-before-suggest
	// pipeline without a database.
	transferFor map[uuid.UUID]*ledger.TransferPair
	dupQueried  []uuid.UUID
	xferQueried []uuid.UUID
}

func (m *fakeMatcher) SuggestMatches(_ context.Context, _, txnID uuid.UUID, _ ledger.MatchOptions) ([]ledger.Suggestion, error) {
	m.queried = append(m.queried, txnID)
	return m.byTxn[txnID], nil
}
func (m *fakeMatcher) AcceptSuggestion(_ context.Context, _, sugID, _ uuid.UUID) (*ledger.Suggestion, error) {
	m.accepted = append(m.accepted, sugID)
	return &ledger.Suggestion{ID: sugID, Status: ledger.SuggestionAccepted}, nil
}
func (m *fakeMatcher) DetectDuplicate(_ context.Context, _, txnID uuid.UUID) (*uuid.UUID, error) {
	m.dupQueried = append(m.dupQueried, txnID)
	if id, ok := m.dupOf[txnID]; ok {
		return &id, nil
	}
	return nil, nil
}
func (m *fakeMatcher) DetectTransfer(_ context.Context, _, txnID uuid.UUID) (*ledger.TransferPair, error) {
	m.xferQueried = append(m.xferQueried, txnID)
	return m.transferFor[txnID], nil
}

type fakeRules struct{ rules []Rule }

func (r *fakeRules) ListRules(context.Context, uuid.UUID, *uuid.UUID) ([]Rule, error) {
	return r.rules, nil
}

// ---- tests -----------------------------------------------------------

func rawTxn(ref, desc, amt string, d time.Time) RawTransaction {
	return RawTransaction{
		ExternalID:  ref,
		ValueDate:   d,
		Description: desc,
		Amount:      decimal.RequireFromString(amt),
		Currency:    "USD",
	}
}

func TestSyncOneIngestsAndAdvancesCursor(t *testing.T) {
	tn := uuid.New()
	acct := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: acct, Provider: "fake"}
	prov := &fakeProvider{name: "fake", cursor: "cur-2", raw: []RawTransaction{
		rawTxn("e1", "Coffee", "-4.50", time.Now()),
		rawTxn("e2", "Salary", "1000", time.Now()),
	}}
	store := &fakeStore{}
	conns := &fakeConns{}
	h := newSyncHandlerForTest(conns, &fakeRules{}, NewRegistry(prov), store, nil)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.Fetched != 2 || res.Inserted != 2 {
		t.Fatalf("res = %+v; want fetched=2 inserted=2", res)
	}
	if conns.advanced[conn.ID] != "cur-2" {
		t.Errorf("cursor not advanced; got %q", conns.advanced[conn.ID])
	}
}

func TestSyncOneDedupesOnResync(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{
		rawTxn("dup", "X", "-1", time.Now()),
	}}
	store := &fakeStore{}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil)

	first, _ := h.SyncOne(context.Background(), tn, conn)
	second, _ := h.SyncOne(context.Background(), tn, conn)
	if first.Inserted != 1 {
		t.Fatalf("first inserted = %d; want 1", first.Inserted)
	}
	if second.Inserted != 0 {
		t.Fatalf("second inserted = %d; want 0 (deduped)", second.Inserted)
	}
}

// TestIngestRawIngestsCSVLines drives the CSV-upload entrypoint: lines
// arrive directly (no provider fetch) and must flow through the exact
// same ingest path as SyncOne. The CSV provider has no incremental
// cursor, so the connection cursor is advanced to empty.
func TestIngestRawIngestsCSVLines(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: ProviderCSV}
	store := &fakeStore{}
	conns := &fakeConns{}
	// IngestRaw does not consult the registry/provider, but the handler
	// still requires a non-nil registry to construct.
	h := newSyncHandlerForTest(conns, &fakeRules{}, NewRegistry(), store, nil)

	raw := []RawTransaction{
		rawTxn("c1", "Stripe payout", "1200", time.Now()),
		rawTxn("c2", "AWS", "-89.10", time.Now()),
	}
	res, err := h.IngestRaw(context.Background(), tn, conn, raw)
	if err != nil {
		t.Fatalf("IngestRaw: %v", err)
	}
	if res.Fetched != 2 || res.Inserted != 2 {
		t.Fatalf("res = %+v; want fetched=2 inserted=2", res)
	}
	if _, ok := conns.advanced[conn.ID]; !ok {
		t.Errorf("cursor/last_synced_at not advanced after CSV ingest")
	}
}

// TestIngestRawDedupesOnReupload pins the idempotency contract for the
// CSV route: re-uploading the same statement inserts nothing the second
// time because each line dedupes on its external ref.
func TestIngestRawDedupesOnReupload(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: ProviderCSV}
	store := &fakeStore{}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(), store, nil)

	raw := []RawTransaction{rawTxn("dup", "X", "-1", time.Now())}
	first, err := h.IngestRaw(context.Background(), tn, conn, raw)
	if err != nil {
		t.Fatalf("IngestRaw first: %v", err)
	}
	second, err := h.IngestRaw(context.Background(), tn, conn, raw)
	if err != nil {
		t.Fatalf("IngestRaw second: %v", err)
	}
	if first.Inserted != 1 {
		t.Fatalf("first inserted = %d; want 1", first.Inserted)
	}
	if second.Inserted != 0 {
		t.Fatalf("second inserted = %d; want 0 (deduped)", second.Inserted)
	}
}

func TestSyncOneSkipsPendingLines(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	settled := rawTxn("e1", "Coffee", "-4.50", time.Now())
	pending := rawTxn("e2", "Pending auth", "-9.99", time.Now())
	pending.Pending = true
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{settled, pending}}
	store := &fakeStore{}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.Fetched != 2 || res.Inserted != 1 || res.Skipped != 1 {
		t.Fatalf("res = %+v; want fetched=2 inserted=1 skipped=1", res)
	}
	// The pending external_ref must not have been ingested, so the later
	// settled version is free to land.
	if store.seen["e2"] {
		t.Error("pending line e2 should not be ingested")
	}
	if !store.seen["e1"] {
		t.Error("settled line e1 should be ingested")
	}
}

func TestSyncOneContentHashFallbackForEmptyExternalID(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	// Provider supplies no stable id; the handler must synthesize a stable
	// content hash so the line ingests and re-sync dedupes.
	noID := rawTxn("", "Bank fee", "-2.00", time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC))
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{noID}}
	store := &fakeStore{}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil)

	first, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if first.Inserted != 1 {
		t.Fatalf("first inserted = %d; want 1 (content-hash ref)", first.Inserted)
	}
	if len(store.gotLines) != 1 || !strings.HasPrefix(store.gotLines[0].ExternalRef, "ch:") {
		t.Fatalf("expected a ch:-prefixed external_ref; got %+v", store.gotLines)
	}
	// Same content re-fetched dedupes via the synthesized ref.
	second, _ := h.SyncOne(context.Background(), tn, conn)
	if second.Inserted != 0 {
		t.Fatalf("second inserted = %d; want 0 (content-hash dedupe)", second.Inserted)
	}
}

func TestSyncOneAutoAcceptsWhenRuleAndConfidenceClear(t *testing.T) {
	tn := uuid.New()
	acct := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: acct, Provider: "fake"}
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{
		rawTxn("e1", "UBER trip", "-20", time.Now()),
		rawTxn("e2", "Mystery", "-99", time.Now()),
	}}
	store := &fakeStore{}
	// Pre-insert to learn the generated txn IDs: run ingest first via store.
	// Instead, drive the matcher off whatever IDs the store assigns by
	// returning a high-confidence suggestion for ALL txns; the rule gates
	// which ones get auto-accepted.
	matcher := &fakeMatcher{byTxn: map[uuid.UUID][]ledger.Suggestion{}}
	// Wrap store to capture assigned IDs and prime matcher suggestions.
	primingStore := &primingFakeStore{inner: store, matcher: matcher}

	rules := &fakeRules{rules: []Rule{
		ruleWith(CondDescriptionContains, "uber", func(r *Rule) { r.AutoApprove = true }),
	}}
	h := newSyncHandlerForTest(&fakeConns{}, rules, NewRegistry(prov), primingStore, matcher)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.AutoMatched != 1 {
		t.Fatalf("AutoMatched = %d; want 1 (only the UBER line)", res.AutoMatched)
	}
	if len(matcher.accepted) != 1 {
		t.Fatalf("accepted %d suggestions; want 1", len(matcher.accepted))
	}
}

// TestSyncOneCounterpartyRuleMatchesViaRawTransaction proves the provider-
// supplied Counterparty survives to rule evaluation. The Description here does
// NOT contain the counterparty value, so a CondCounterparty rule can only fire
// if Counterparty is preserved (it is not stored on bank_transactions). Before
// the byRef fix the reconstructed RawTransaction dropped Counterparty and this
// rule would silently never match.
func TestSyncOneCounterpartyRuleMatchesViaRawTransaction(t *testing.T) {
	tn := uuid.New()
	acct := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: acct, Provider: "fake"}
	rt := RawTransaction{
		ExternalID:   "e1",
		ValueDate:    time.Now(),
		Description:  "POS 8841 REF99", // deliberately omits "uber eats"
		Amount:       decimal.RequireFromString("-20"),
		Currency:     "USD",
		Counterparty: "Uber Eats",
	}
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{rt}}
	matcher := &fakeMatcher{byTxn: map[uuid.UUID][]ledger.Suggestion{}}
	store := &primingFakeStore{inner: &fakeStore{}, matcher: matcher}
	rules := &fakeRules{rules: []Rule{
		ruleWith(CondCounterparty, "uber eats", func(r *Rule) { r.AutoApprove = true }),
	}}
	h := newSyncHandlerForTest(&fakeConns{}, rules, NewRegistry(prov), store, matcher)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.AutoMatched != 1 {
		t.Fatalf("AutoMatched = %d; want 1 (counterparty rule must match on Counterparty, not Description)", res.AutoMatched)
	}
}

// primingFakeStore wraps fakeStore so each inserted line gets a
// high-confidence suggestion registered under its generated id.
type primingFakeStore struct {
	inner   *fakeStore
	matcher *fakeMatcher
}

func (s *primingFakeStore) SyncBankTransactions(ctx context.Context, tenantID, acct uuid.UUID, lines []ledger.BankTransaction) ([]ledger.BankTransaction, error) {
	inserted, err := s.inner.SyncBankTransactions(ctx, tenantID, acct, lines)
	if err != nil {
		return nil, err
	}
	for i := range inserted {
		s.matcher.byTxn[inserted[i].ID] = []ledger.Suggestion{{
			ID:         uuid.New(),
			Confidence: 0.95,
			Status:     ledger.SuggestionSuggested,
		}}
	}
	return inserted, nil
}

func TestSyncOneNoAutoAcceptBelowThreshold(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{
		rawTxn("e1", "UBER trip", "-20", time.Now()),
	}}
	matcher := &fakeMatcher{byTxn: map[uuid.UUID][]ledger.Suggestion{}}
	store := &primingLowConfStore{inner: &fakeStore{}, matcher: matcher}
	rules := &fakeRules{rules: []Rule{
		ruleWith(CondDescriptionContains, "uber", func(r *Rule) { r.AutoApprove = true }),
	}}
	h := newSyncHandlerForTest(&fakeConns{}, rules, NewRegistry(prov), store, matcher)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.AutoMatched != 0 {
		t.Fatalf("AutoMatched = %d; want 0 (confidence below threshold)", res.AutoMatched)
	}
	if res.Suggested != 1 {
		t.Fatalf("Suggested = %d; want 1", res.Suggested)
	}
}

type primingLowConfStore struct {
	inner   *fakeStore
	matcher *fakeMatcher
}

func (s *primingLowConfStore) SyncBankTransactions(ctx context.Context, tenantID, acct uuid.UUID, lines []ledger.BankTransaction) ([]ledger.BankTransaction, error) {
	inserted, err := s.inner.SyncBankTransactions(ctx, tenantID, acct, lines)
	if err != nil {
		return nil, err
	}
	for i := range inserted {
		s.matcher.byTxn[inserted[i].ID] = []ledger.Suggestion{{ID: uuid.New(), Confidence: 0.6}}
	}
	return inserted, nil
}

// resolverPrimingStore primes a high-confidence suggestion for every
// inserted line and, by description prefix, registers a duplicate or a
// transfer outcome on the matcher so the resolve-before-suggest pipeline
// can be exercised end-to-end without a database.
type resolverPrimingStore struct {
	inner   *fakeStore
	matcher *fakeMatcher
}

func (s *resolverPrimingStore) SyncBankTransactions(ctx context.Context, tenantID, acct uuid.UUID, lines []ledger.BankTransaction) ([]ledger.BankTransaction, error) {
	inserted, err := s.inner.SyncBankTransactions(ctx, tenantID, acct, lines)
	if err != nil {
		return nil, err
	}
	for i := range inserted {
		id := inserted[i].ID
		s.matcher.byTxn[id] = []ledger.Suggestion{{ID: uuid.New(), Confidence: 0.95, Status: ledger.SuggestionSuggested}}
		switch {
		case strings.HasPrefix(inserted[i].Description, "DUP"):
			s.matcher.dupOf[id] = uuid.New()
		case strings.HasPrefix(inserted[i].Description, "XFER"):
			s.matcher.transferFor[id] = &ledger.TransferPair{ID: uuid.New(), CreditTxnID: id}
		}
	}
	return inserted, nil
}

// TestSyncOneResolvesDuplicatesAndTransfersBeforeSuggesting proves the
// pipeline ordering: a line flagged a duplicate or paired as a transfer is
// counted and removed from suggestion generation, while a normal line still
// flows to SuggestMatches.
func TestSyncOneResolvesDuplicatesAndTransfersBeforeSuggesting(t *testing.T) {
	tn := uuid.New()
	acct := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: acct, Provider: "fake"}
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{
		rawTxn("d1", "DUP duplicate line", "-12.50", time.Now()),
		rawTxn("x1", "XFER to savings", "-500", time.Now()),
		rawTxn("n1", "Normal coffee", "-4.50", time.Now()),
	}}
	matcher := &fakeMatcher{
		byTxn:       map[uuid.UUID][]ledger.Suggestion{},
		dupOf:       map[uuid.UUID]uuid.UUID{},
		transferFor: map[uuid.UUID]*ledger.TransferPair{},
	}
	store := &resolverPrimingStore{inner: &fakeStore{}, matcher: matcher}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, matcher)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if res.Duplicates != 1 {
		t.Fatalf("Duplicates = %d; want 1", res.Duplicates)
	}
	if res.Transfers != 1 {
		t.Fatalf("Transfers = %d; want 1", res.Transfers)
	}
	// Exactly the normal line reaches SuggestMatches; the duplicate and
	// transfer legs are resolved earlier and skipped.
	if len(matcher.queried) != 1 {
		t.Fatalf("SuggestMatches called %d times; want 1 (only the normal line)", len(matcher.queried))
	}
	if res.Suggested != 1 {
		t.Fatalf("Suggested = %d; want 1", res.Suggested)
	}
	// Every line is probed for duplicates; transfer detection runs only
	// after the duplicate check clears (so the dup line is not probed).
	if len(matcher.dupQueried) != 3 {
		t.Fatalf("DetectDuplicate called %d times; want 3 (all lines)", len(matcher.dupQueried))
	}
	if len(matcher.xferQueried) != 2 {
		t.Fatalf("DetectTransfer called %d times; want 2 (non-duplicate lines)", len(matcher.xferQueried))
	}
}

func TestSyncOneProviderErrorPropagates(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	prov := &fakeProvider{name: "fake", err: errors.New("boom")}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), &fakeStore{}, nil)
	if _, err := h.SyncOne(context.Background(), tn, conn); err == nil {
		t.Fatal("expected provider error to propagate")
	}
}

func TestSyncOneUnknownProvider(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, Provider: "ghost"}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(NewCSVProvider()), &fakeStore{}, nil)
	if _, err := h.SyncOne(context.Background(), tn, conn); err == nil {
		t.Fatal("expected error for unregistered provider")
	}
}

// TestSyncOneAndIngestRawGuardAgainstUnwiredHandler proves the manual
// entry points fail with a descriptive error rather than panicking when
// invoked on a partially-constructed handler (e.g. a future caller or a
// test that forgot to wire the store). This mirrors Handle's own wiring
// guard so all three entry points behave consistently.
func TestSyncOneAndIngestRawGuardAgainstUnwiredHandler(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: ProviderCSV}

	// Bare handler: nil registry and nil store. Neither call may panic.
	bare := &SyncHandler{}
	if _, err := bare.SyncOne(context.Background(), tn, conn); err == nil {
		t.Error("SyncOne on unwired handler: expected error, got nil")
	}
	if _, err := bare.IngestRaw(context.Background(), tn, conn, nil); err == nil {
		t.Error("IngestRaw on unwired handler: expected error, got nil")
	}

	// Registry present but store still nil: SyncOne must reject before it
	// reaches ingestDelta's store writes.
	noStore := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(NewCSVProvider()), nil, nil)
	if _, err := noStore.SyncOne(context.Background(), tn, conn); err == nil {
		t.Error("SyncOne with nil store: expected error, got nil")
	}
	if _, err := noStore.IngestRaw(context.Background(), tn, conn, nil); err == nil {
		t.Error("IngestRaw with nil store: expected error, got nil")
	}

	// Registry and store present but conns nil: both entry points must
	// reject before ingestDelta dereferences h.conns for the cursor
	// advance (the case the symmetric guard must also cover).
	noConns := &SyncHandler{registry: NewRegistry(NewCSVProvider()), store: &fakeStore{}}
	if _, err := noConns.SyncOne(context.Background(), tn, conn); err == nil {
		t.Error("SyncOne with nil conns: expected error, got nil")
	}
	if _, err := noConns.IngestRaw(context.Background(), tn, conn, nil); err == nil {
		t.Error("IngestRaw with nil conns: expected error, got nil")
	}

	// A nil connection must also be rejected rather than dereferenced.
	wired := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(NewCSVProvider()), &fakeStore{}, nil)
	if _, err := wired.SyncOne(context.Background(), tn, nil); err == nil {
		t.Error("SyncOne with nil connection: expected error, got nil")
	}
	if _, err := wired.IngestRaw(context.Background(), tn, nil, nil); err == nil {
		t.Error("IngestRaw with nil connection: expected error, got nil")
	}
}

func TestHandleMarksErrorAndContinues(t *testing.T) {
	tn := uuid.New()
	okConn := Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "ok"}
	badConn := Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "bad"}
	reg := NewRegistry(
		&fakeProvider{name: "ok", cursor: "c", raw: []RawTransaction{rawTxn("e1", "x", "-1", time.Now())}},
		&fakeProvider{name: "bad", err: errors.New("revoked")},
	)
	conns := &fakeConns{active: []Connection{okConn, badConn}}
	h := newSyncHandlerForTest(conns, &fakeRules{}, reg, &fakeStore{}, nil)

	if err := h.Handle(context.Background(), tn, scheduledActionStub()); err != nil {
		t.Fatalf("Handle should swallow per-connection errors: %v", err)
	}
	if conns.advanced[okConn.ID] != "c" {
		t.Errorf("healthy connection did not advance")
	}
	if _, ok := conns.markedErrors[badConn.ID]; !ok {
		t.Errorf("failed connection was not marked with last_error")
	}
}

func TestSyncOneFirstSyncUsesLookbackWindow(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	var gotSince time.Time
	prov := &sinceCapturingProvider{name: "fake", capture: &gotSince}
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), &fakeStore{}, nil).
		WithClock(func() time.Time { return now }).
		WithLookback(30 * 24 * time.Hour)
	if _, err := h.SyncOne(context.Background(), tn, conn); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	want := now.Add(-30 * 24 * time.Hour)
	if !gotSince.Equal(want) {
		t.Errorf("since = %v; want lookback %v", gotSince, want)
	}
}

func TestSyncOnePrefersLastSyncOverLookback(t *testing.T) {
	tn := uuid.New()
	last := time.Date(2024, 5, 30, 0, 0, 0, 0, time.UTC)
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake", LastSyncAt: &last}
	var gotSince time.Time
	prov := &sinceCapturingProvider{name: "fake", capture: &gotSince}
	now := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), &fakeStore{}, nil).
		WithClock(func() time.Time { return now }).
		WithLookback(90 * 24 * time.Hour)
	if _, err := h.SyncOne(context.Background(), tn, conn); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if !gotSince.Equal(last) {
		t.Errorf("since = %v; want last_sync_at %v", gotSince, last)
	}
}

type sinceCapturingProvider struct {
	name    string
	capture *time.Time
}

func (p *sinceCapturingProvider) Name() string { return p.name }
func (p *sinceCapturingProvider) InitiateConnect(context.Context, uuid.UUID, uuid.UUID, string) (string, error) {
	return "", nil
}
func (p *sinceCapturingProvider) CompleteConnect(context.Context, uuid.UUID, string) (*Connection, error) {
	return nil, nil
}
func (p *sinceCapturingProvider) FetchTransactions(_ context.Context, _ *Connection, since time.Time) ([]RawTransaction, string, error) {
	*p.capture = since
	return nil, "", nil
}
func (p *sinceCapturingProvider) Disconnect(context.Context, *Connection) error { return nil }
