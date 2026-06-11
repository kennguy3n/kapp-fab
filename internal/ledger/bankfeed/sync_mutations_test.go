package bankfeed

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
)

// fakeChangeProvider implements both Provider and ChangeFetcher so the
// sync handler exercises the modified/removed path.
type fakeChangeProvider struct {
	name  string
	delta FetchDelta
	err   error
}

func (f *fakeChangeProvider) Name() string { return f.name }
func (f *fakeChangeProvider) InitiateConnect(context.Context, uuid.UUID, uuid.UUID, string) (string, error) {
	return "", nil
}
func (f *fakeChangeProvider) CompleteConnect(context.Context, uuid.UUID, string) (*Connection, error) {
	return nil, nil
}
func (f *fakeChangeProvider) FetchTransactions(_ context.Context, _ *Connection, _ time.Time) ([]RawTransaction, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.delta.Added, f.delta.Cursor, nil
}
func (f *fakeChangeProvider) FetchChanges(_ context.Context, _ *Connection, _ time.Time) (FetchDelta, error) {
	if f.err != nil {
		return FetchDelta{}, f.err
	}
	return f.delta, nil
}
func (f *fakeChangeProvider) Disconnect(context.Context, *Connection) error { return nil }

// fakeMutationStore embeds fakeStore (for ingest) and records the
// modified/removed deltas handed to the mutation applier, returning a
// configurable MutationResult.
type fakeMutationStore struct {
	*fakeStore
	gotMods    []ledger.BankTxnMutation
	gotRemoved []string
	result     *ledger.MutationResult
	calls      int
}

func newFakeMutationStore(result *ledger.MutationResult) *fakeMutationStore {
	return &fakeMutationStore{fakeStore: &fakeStore{}, result: result}
}

func (s *fakeMutationStore) ApplyBankTransactionMutations(_ context.Context, _, _ uuid.UUID, modified []ledger.BankTxnMutation, removed []string) (*ledger.MutationResult, error) {
	s.calls++
	s.gotMods = append(s.gotMods, modified...)
	s.gotRemoved = append(s.gotRemoved, removed...)
	if s.result != nil {
		return s.result, nil
	}
	return &ledger.MutationResult{Updated: len(modified), Voided: len(removed)}, nil
}

func TestSyncOneAppliesModifiedAndRemoved(t *testing.T) {
	tn := uuid.New()
	acct := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: acct, Provider: "plaidish"}
	d := time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC)
	prov := &fakeChangeProvider{name: "plaidish", delta: FetchDelta{
		Added:    []RawTransaction{rawTxn("a1", "New coffee", "-4.50", d)},
		Modified: []RawTransaction{rawTxn("m1", "Returned payment", "-12.00", d)},
		Removed:  []string{"r1"},
		Cursor:   "cur-2",
	}}
	store := newFakeMutationStore(&ledger.MutationResult{Updated: 1, Voided: 1, Unwound: 1})
	conns := &fakeConns{}
	h := newSyncHandlerForTest(conns, &fakeRules{}, NewRegistry(prov), store, nil)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("ApplyBankTransactionMutations called %d times; want 1", store.calls)
	}
	if len(store.gotMods) != 1 || store.gotMods[0].ExternalRef != "m1" {
		t.Fatalf("gotMods = %+v; want one mod for m1", store.gotMods)
	}
	if got := store.gotMods[0]; got.Description != "Returned payment" ||
		!got.Amount.Equal(decimal.RequireFromString("-12.00")) || !got.ValueDate.Equal(d) || got.Currency != "USD" {
		t.Fatalf("mod fields not propagated: %+v", got)
	}
	if len(store.gotRemoved) != 1 || store.gotRemoved[0] != "r1" {
		t.Fatalf("gotRemoved = %+v; want [r1]", store.gotRemoved)
	}
	if res.Inserted != 1 || res.Updated != 1 || res.Voided != 1 || res.Unwound != 1 {
		t.Fatalf("res = %+v; want inserted=1 updated=1 voided=1 unwound=1", res)
	}
	if conns.advanced[conn.ID] != "cur-2" {
		t.Errorf("cursor not advanced; got %q", conns.advanced[conn.ID])
	}
}

func TestSyncOneSkipsPendingAndIDLessModified(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "plaidish"}
	d := time.Now()
	pending := rawTxn("m1", "pending modify", "-3.00", d)
	pending.Pending = true
	idless := rawTxn("", "no id", "-1.00", d) // can't address an existing row
	prov := &fakeChangeProvider{name: "plaidish", delta: FetchDelta{
		Modified: []RawTransaction{pending, idless},
		Cursor:   "c",
	}}
	store := newFakeMutationStore(nil)
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if len(store.gotMods) != 0 {
		t.Fatalf("gotMods = %+v; want none (pending + id-less skipped)", store.gotMods)
	}
	if res.Skipped != 1 {
		t.Fatalf("res.Skipped = %d; want 1 (the pending modify; id-less is silently dropped)", res.Skipped)
	}
}

func TestSyncOneModifiedMissingIngestedAsNew(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "plaidish"}
	d := time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC)
	prov := &fakeChangeProvider{name: "plaidish", delta: FetchDelta{
		Modified: []RawTransaction{rawTxn("m1", "Late settle", "-7.25", d)},
		Cursor:   "c",
	}}
	// The store reports m1 had no prior row, so the handler must ingest it
	// as a fresh addition rather than dropping the statement data.
	store := newFakeMutationStore(&ledger.MutationResult{ModifiedMissing: []string{"m1"}})
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if !store.seen["m1"] {
		t.Error("missing modified line m1 should be ingested as new")
	}
	if res.Inserted != 1 {
		t.Fatalf("res.Inserted = %d; want 1 (modified-as-new)", res.Inserted)
	}
}

func TestSyncOneRematchGetsSuggestions(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "plaidish"}
	d := time.Now()
	prov := &fakeChangeProvider{name: "plaidish", delta: FetchDelta{
		Modified: []RawTransaction{rawTxn("m1", "Amount corrected", "-30.00", d)},
		Cursor:   "c",
	}}
	rematchID := uuid.New()
	store := newFakeMutationStore(&ledger.MutationResult{
		Updated: 1,
		Rematch: []ledger.MutatedLine{{ID: rematchID, ExternalRef: "m1"}},
	})
	matcher := &fakeMatcher{byTxn: map[uuid.UUID][]ledger.Suggestion{}}
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, matcher)

	if _, err := h.SyncOne(context.Background(), tn, conn); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	found := false
	for _, id := range matcher.queried {
		if id == rematchID {
			found = true
		}
	}
	if !found {
		t.Fatalf("matcher was not asked to re-suggest the re-opened line %s; queried=%v", rematchID, matcher.queried)
	}
}

func TestSyncOneApplyMutationsDisabledSkipsPath(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "plaidish"}
	d := time.Now()
	prov := &fakeChangeProvider{name: "plaidish", delta: FetchDelta{
		Modified: []RawTransaction{rawTxn("m1", "x", "-1", d)},
		Removed:  []string{"r1"},
		Cursor:   "c",
	}}
	store := newFakeMutationStore(nil)
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil).
		WithApplyMutations(false)

	res, err := h.SyncOne(context.Background(), tn, conn)
	if err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("applier called %d times; want 0 with mutations disabled", store.calls)
	}
	if res.Updated != 0 || res.Voided != 0 {
		t.Fatalf("res = %+v; want no mutations applied", res)
	}
}

func TestSyncOneNonChangeFetcherSkipsMutationPath(t *testing.T) {
	tn := uuid.New()
	conn := &Connection{ID: uuid.New(), TenantID: tn, BankAccountID: uuid.New(), Provider: "fake"}
	// Plain fakeProvider does NOT implement ChangeFetcher, so fetchDelta
	// yields additions only and the applier is never invoked.
	prov := &fakeProvider{name: "fake", cursor: "c", raw: []RawTransaction{rawTxn("e1", "x", "-1", time.Now())}}
	store := newFakeMutationStore(nil)
	h := newSyncHandlerForTest(&fakeConns{}, &fakeRules{}, NewRegistry(prov), store, nil)

	if _, err := h.SyncOne(context.Background(), tn, conn); err != nil {
		t.Fatalf("SyncOne: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("applier called %d times; want 0 for a non-ChangeFetcher provider", store.calls)
	}
}
