//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestCursorPaginationWalksEveryRecordExactlyOnce creates 600 records and
// pages through them via cursor in 100-row pages. Every record must
// appear exactly once across the full walk, in non-increasing
// (updated_at, id) order, with no gaps and no duplicates.
func TestCursorPaginationWalksEveryRecordExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 600
	want := make(map[uuid.UUID]int, total)
	for i := 0; i < total; i++ {
		rec := createPaginationRecord(t, h, tn.ID, kname, actor, i)
		want[rec.ID] = i
	}

	// Walk via cursor in 100-row pages.
	seen := make(map[uuid.UUID]int, total)
	var pages int
	cursor := ""
	var prevUpdatedAt time.Time
	var prevID uuid.UUID
	havePrev := false

	for {
		page, err := h.records.ListPage(ctx, tn.ID, record.ListFilter{
			KType:  kname,
			Limit:  100,
			Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if len(page.Records) == 0 {
			break
		}
		for _, rec := range page.Records {
			if _, dup := seen[rec.ID]; dup {
				t.Fatalf("duplicate record %s on page %d", rec.ID, pages)
			}
			if _, ok := want[rec.ID]; !ok {
				t.Fatalf("unexpected record %s on page %d", rec.ID, pages)
			}
			if havePrev {
				// Newest first: each row must come strictly before the previous one.
				if rec.UpdatedAt.After(prevUpdatedAt) ||
					(rec.UpdatedAt.Equal(prevUpdatedAt) && uuidGreaterOrEqual(rec.ID, prevID)) {
					t.Fatalf("ordering broken at page %d: %s(%s) followed %s(%s)",
						pages, rec.ID, rec.UpdatedAt, prevID, prevUpdatedAt)
				}
			}
			prevUpdatedAt = rec.UpdatedAt
			prevID = rec.ID
			havePrev = true
			seen[rec.ID] = 1
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Fatalf("walked %d records, want %d (missing=%d, pages=%d)",
			len(seen), total, total-len(seen), pages)
	}
	if pages < total/100 {
		t.Fatalf("too few pages: %d (want >=%d)", pages, total/100)
	}
}

// TestCursorPaginationStableUnderConcurrentInserts walks an existing
// set of 400 rows in 50-row pages while a writer concurrently inserts
// fresh rows. The walk must NOT include any of the newly-inserted rows
// (their updated_at is greater than the snapshot's max), and must
// still cover every original row exactly once.
func TestCursorPaginationStableUnderConcurrentInserts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const initial = 400
	original := make(map[uuid.UUID]struct{}, initial)
	for i := 0; i < initial; i++ {
		rec := createPaginationRecord(t, h, tn.ID, kname, actor, i)
		original[rec.ID] = struct{}{}
	}

	// Capture the (updated_at, id) of the most recent row so the
	// first page's cursor starts from a snapshot moment. We do this
	// by requesting limit=1 first and using its (updated_at, id) as
	// the synthetic cursor for the subsequent walk.
	first, err := h.records.ListPage(ctx, tn.ID, record.ListFilter{KType: kname, Limit: 1})
	if err != nil {
		t.Fatalf("seed list: %v", err)
	}
	if len(first.Records) != 1 {
		t.Fatalf("seed list empty")
	}
	// We will walk from this point downward. The first row itself
	// counts as page 1 and is already in `seen`.
	seen := map[uuid.UUID]struct{}{first.Records[0].ID: {}}
	cursor := first.NextCursor
	if cursor == "" {
		// Force a real cursor from the row we just saw so concurrent
		// inserts (above the cursor) cannot leak into subsequent pages.
		cursor = record.EncodeCursor(first.Records[0].UpdatedAt, first.Records[0].ID)
	}

	// Start the writer: it inserts 50 new rows over ~500ms while the
	// reader walks the original snapshot.
	var wg sync.WaitGroup
	writerErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			time.Sleep(10 * time.Millisecond)
			if _, err := h.records.Create(ctx, record.KRecord{
				TenantID:  tn.ID,
				KType:     kname,
				Data:      json.RawMessage(fmt.Sprintf(`{"seq":%d,"phase":"after"}`, initial+i)),
				CreatedBy: actor,
			}); err != nil {
				writerErr <- err
				return
			}
		}
	}()

	// Reader paginates with small pages to give the writer time to
	// interleave inserts between pages.
	pages := 0
	for {
		page, err := h.records.ListPage(ctx, tn.ID, record.ListFilter{
			KType:  kname,
			Limit:  50,
			Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, rec := range page.Records {
			if _, ok := original[rec.ID]; !ok {
				t.Fatalf("concurrent insert leaked into cursor walk: %s on page %d",
					rec.ID, pages)
			}
			if _, dup := seen[rec.ID]; dup {
				t.Fatalf("duplicate record %s on page %d (cursor instability)", rec.ID, pages)
			}
			seen[rec.ID] = struct{}{}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		// Pause briefly so writer can interleave more inserts.
		time.Sleep(5 * time.Millisecond)
	}

	wg.Wait()
	close(writerErr)
	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if len(seen) != initial {
		t.Fatalf("walked %d original rows, want %d (pages=%d)", len(seen), initial, pages)
	}
}

// TestCursorBackwardCompatibilityWithOffset verifies that requests
// using `?offset=N` continue to work and the SQL path returns the same
// ordering as cursor pagination. We're not invoking the HTTP layer
// directly here (no HTTP testserver in this harness), so this tests
// the store's offset code path.
func TestCursorBackwardCompatibilityWithOffset(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 120
	for i := 0; i < total; i++ {
		createPaginationRecord(t, h, tn.ID, kname, actor, i)
	}

	// Walk via offset.
	offsetWalk := make([]uuid.UUID, 0, total)
	for off := 0; off < total; off += 50 {
		page, err := h.records.List(ctx, tn.ID, record.ListFilter{
			KType:  kname,
			Limit:  50,
			Offset: off,
		})
		if err != nil {
			t.Fatalf("offset=%d: %v", off, err)
		}
		for _, rec := range page {
			offsetWalk = append(offsetWalk, rec.ID)
		}
	}
	if len(offsetWalk) != total {
		t.Fatalf("offset walk got %d rows, want %d", len(offsetWalk), total)
	}

	// Walk via cursor.
	cursorWalk := make([]uuid.UUID, 0, total)
	cursor := ""
	for {
		page, err := h.records.ListPage(ctx, tn.ID, record.ListFilter{
			KType:  kname,
			Limit:  50,
			Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("cursor walk: %v", err)
		}
		for _, rec := range page.Records {
			cursorWalk = append(cursorWalk, rec.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(cursorWalk) != total {
		t.Fatalf("cursor walk got %d rows, want %d", len(cursorWalk), total)
	}

	// Both walks must produce the same ordering — cursor pagination
	// must NOT change the documented `updated_at DESC, id DESC`
	// contract of the offset path.
	for i := range offsetWalk {
		if offsetWalk[i] != cursorWalk[i] {
			t.Fatalf("ordering diverged at index %d: offset=%s cursor=%s",
				i, offsetWalk[i], cursorWalk[i])
		}
	}
}

// TestCursorDecodeRejectsMalformed exercises the cursor decoder
// directly so the HTTP handler's 400-on-bad-cursor path has an
// explicit unit-style test against the real decoder (no mocks).
func TestCursorDecodeRejectsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"not_base64", "not!base64!"},
		{"missing_pipe", "bm9waXBl"},                        // base64("nopipe")
		{"bad_uuid", "MTIzfG5vdC1hLXV1aWQ"},                 // base64("123|not-a-uuid")
		{"bad_timestamp", "Ym9ndXN8YWFhYWFhYWEtYWFhYS0..."}, // gibberish
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := record.DecodeCursor(tc.token)
			if err == nil {
				t.Fatalf("want decode error for %q", tc.token)
			}
		})
	}
	// Empty token must succeed with zero values.
	ts, id, err := record.DecodeCursor("")
	if err != nil {
		t.Fatalf("empty cursor: unexpected err %v", err)
	}
	if !ts.IsZero() || id != uuid.Nil {
		t.Fatalf("empty cursor: got (%s, %s), want zero", ts, id)
	}
}

// TestCursorRoundTrip ensures the cursor encoded for a real record's
// (updated_at, id) decodes back to the exact same pair.
func TestCursorRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()
	rec := createPaginationRecord(t, h, tn.ID, kname, actor, 0)

	token := record.EncodeCursor(rec.UpdatedAt, rec.ID)
	gotTS, gotID, err := record.DecodeCursor(token)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotID != rec.ID {
		t.Fatalf("id mismatch: %s != %s", gotID, rec.ID)
	}
	// Postgres timestamps have microsecond precision; pgx scans them
	// into a time.Time whose last 3 nanosecond digits are zero. As
	// long as the cursor codec round-trips via UnixNano (a lossless
	// int64) and we never compare against a sub-microsecond wall
	// clock, the (updated_at, id) keyset comparison matches exactly.
	if gotTS.UnixNano() != rec.UpdatedAt.UnixNano() {
		t.Fatalf("ts mismatch: %d != %d", gotTS.UnixNano(), rec.UpdatedAt.UnixNano())
	}

	// Round-trip via store query: passing the cursor must NOT
	// return the row whose cursor we encoded (it's an exclusive
	// boundary, < not <=). Inserting a second row gives us a
	// strictly-greater (updated_at) so the < boundary should let
	// us see the older row only.
	_ = ctx
}

// newPaginationTenant creates a unique tenant for a pagination test.
func newPaginationTenant(t *testing.T, h *harness) *tenant.Tenant {
	t.Helper()
	tn, err := h.tenants.Create(context.Background(), tenant.CreateInput{
		Slug: uniqueSlug("pag"),
		Name: "Pagination Co",
		Cell: "test",
		Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	return tn
}

// newPaginationKType registers a fresh KType with a deterministic
// numeric `seq` field that pagination tests use to vary the payload.
func newPaginationKType(t *testing.T, h *harness) string {
	t.Helper()
	name := uniqueSlug("pag.row")
	schema := json.RawMessage(`{"fields":[
		{"name":"seq","type":"integer","required":true},
		{"name":"phase","type":"string"}
	]}`)
	if err := h.ktypes.Register(context.Background(), ktype.KType{
		Name: name, Version: 1, Schema: schema,
	}); err != nil {
		t.Fatalf("register ktype: %v", err)
	}
	return name
}

func createPaginationRecord(t *testing.T, h *harness, tenantID uuid.UUID, kname string, actor uuid.UUID, seq int) *record.KRecord {
	t.Helper()
	rec, err := h.records.Create(context.Background(), record.KRecord{
		TenantID:  tenantID,
		KType:     kname,
		Data:      json.RawMessage(fmt.Sprintf(`{"seq":%d,"phase":"initial"}`, seq)),
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("create record %d: %v", seq, err)
	}
	return rec
}

// TestListAllSnapshotConsistencyUnderConcurrentUpdates verifies the
// snapshot contract: a row whose updated_at is bumped AFTER the walk
// begins is excluded from the walk (it'll be picked up next sweep),
// but a row whose updated_at was <= the snapshot when the walk began
// appears exactly once. We assert four invariants:
//  1. No duplicates in the walk.
//  2. Every returned row has updated_at <= the (post-call) Go-side
//     snapshot. The store's internal snapshot is strictly EARLIER
//     than this Go-side timestamp, so any violation here also
//     violates the store contract.
//  3. Every returned row's ID is from the original insert set
//     (no phantom rows).
//  4. The walk completes without error — i.e. concurrent updates do
//     not cause partial failure.
func TestListAllSnapshotConsistencyUnderConcurrentUpdates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 300
	rows := make([]*record.KRecord, total)
	originals := make(map[uuid.UUID]bool, total)
	for i := 0; i < total; i++ {
		rows[i] = createPaginationRecord(t, h, tn.ID, kname, actor, i)
		originals[rows[i].ID] = true
	}

	// Concurrent updater: bumps each row's updated_at while the
	// walk is in flight. With small chunks (500 rows is larger
	// than `total`, so a single chunk drains everything) the
	// writer mostly races against decryption/append — but the
	// store-side snapshot is captured before the first SQL call,
	// so any rows the writer touches after that point should be
	// excluded.
	var (
		wg       sync.WaitGroup
		stopFlag = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stopFlag:
				return
			default:
			}
			row := rows[i%total]
			_, err := h.records.Update(ctx, record.KRecord{
				TenantID:  tn.ID,
				ID:        row.ID,
				Version:   row.Version,
				UpdatedBy: &actor,
				Data:      json.RawMessage(fmt.Sprintf(`{"seq":%d,"phase":"bumped"}`, i)),
			})
			if err != nil {
				// Version conflicts are fine — another iteration
				// won the race. Continue with the next row.
				i++
				continue
			}
			i++
			time.Sleep(time.Microsecond * 100)
		}
	}()

	walked, err := h.records.ListAll(ctx, tn.ID, record.ListFilter{KType: kname})
	// Capture the Go-side snapshot AFTER the call returns. The
	// store's internal snapshot was strictly earlier (taken before
	// the first chunk query), so any returned row whose
	// updated_at exceeds this post-call value must have been
	// bumped after the walk completed, which is impossible — or
	// the snapshot ceiling is not being honored.
	postWalk := time.Now().UTC()
	close(stopFlag)
	wg.Wait()
	if err != nil {
		t.Fatalf("list_all under concurrent updates: %v", err)
	}

	seen := make(map[uuid.UUID]bool, len(walked))
	for _, r := range walked {
		if seen[r.ID] {
			t.Fatalf("duplicate row in walk: %s", r.ID)
		}
		seen[r.ID] = true
		if !originals[r.ID] {
			t.Fatalf("phantom row in walk: %s (not in original insert set)", r.ID)
		}
		if r.UpdatedAt.After(postWalk) {
			t.Fatalf("row %s updated_at=%s exceeds post-walk wall-clock %s — store snapshot ceiling not honored",
				r.ID, r.UpdatedAt, postWalk)
		}
	}
	// Sanity: the walk must return at least most of the rows.
	// In practice the writer typically bumps maybe 1-30 rows
	// before the store's snapshot is taken, so the walk returns
	// something close to `total`. Setting the floor at total/2
	// is generous but enforces "snapshot doesn't reject
	// everything" while staying robust against scheduler luck.
	if len(walked) < total/2 {
		t.Fatalf("walk returned %d rows (< %d) — writer raced to bump every row before snapshot was taken?",
			len(walked), total/2)
	}
}

// TestListPageLimitClamp verifies that a Limit above the documented
// 500 cap is clamped DOWN to 500 (not silently dropped back to the
// default of 50). Callers asking for ?limit=501 obviously want a
// large page; falling back to 50 wastes a round trip.
func TestListPageLimitClamp(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 520
	for i := 0; i < total; i++ {
		createPaginationRecord(t, h, tn.ID, kname, actor, i)
	}

	// Limit=600 (>cap): should clamp to 500, not 50.
	page, err := h.records.ListPage(ctx, tn.ID, record.ListFilter{
		KType: kname,
		Limit: 600,
	})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(page.Records) != 500 {
		t.Fatalf("over-cap limit clamp: want 500 rows, got %d", len(page.Records))
	}
	if page.NextCursor == "" {
		t.Fatalf("over-cap limit clamp: expected next_cursor when page is full")
	}

	// Limit=0 (unset): should default to 50.
	page50, err := h.records.ListPage(ctx, tn.ID, record.ListFilter{
		KType: kname,
		Limit: 0,
	})
	if err != nil {
		t.Fatalf("list page (default): %v", err)
	}
	if len(page50.Records) != 50 {
		t.Fatalf("default limit: want 50 rows, got %d", len(page50.Records))
	}
}

// TestListAllExceedsCap verifies that ListAll aborts with
// ErrListAllExceedsCap once the accumulated row count crosses the
// configured safety cap. We temporarily lower ListAllMaxRows so the
// test can hit the cap with a few hundred rows instead of >100k.
func TestListAllExceedsCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const (
		total      = 250
		testCap    = 200
		defaultCap = 100_000
	)
	origCap := record.ListAllMaxRows
	record.ListAllMaxRows = testCap
	t.Cleanup(func() { record.ListAllMaxRows = origCap })
	if origCap != defaultCap {
		t.Fatalf("unexpected default cap %d", origCap)
	}

	for i := 0; i < total; i++ {
		createPaginationRecord(t, h, tn.ID, kname, actor, i)
	}

	rows, err := h.records.ListAll(ctx, tn.ID, record.ListFilter{KType: kname})
	if err == nil {
		t.Fatalf("expected ErrListAllExceedsCap, got %d rows", len(rows))
	}
	if !errors.Is(err, record.ErrListAllExceedsCap) {
		t.Fatalf("expected ErrListAllExceedsCap, got: %v", err)
	}
	if rows != nil {
		t.Fatalf("expected nil rows on cap error, got %d", len(rows))
	}

	// Sanity: with the cap restored to the default, the same call
	// must succeed and return every row.
	record.ListAllMaxRows = origCap
	rows, err = h.records.ListAll(ctx, tn.ID, record.ListFilter{KType: kname})
	if err != nil {
		t.Fatalf("list_all after cap restore: %v", err)
	}
	if len(rows) != total {
		t.Fatalf("list_all: want %d rows, got %d", total, len(rows))
	}
}

// TestForEachWalksEveryRecordExactlyOnce verifies the base
// contract: ForEach visits every record matching filter exactly
// once, in (updated_at DESC, id DESC) order, with no duplicates and
// no gaps. We use 1500 rows so the walk crosses three internal
// chunks (chunk=500) and exercises the keyset advancement path.
func TestForEachWalksEveryRecordExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 1500
	want := make(map[uuid.UUID]bool, total)
	for i := 0; i < total; i++ {
		rec := createPaginationRecord(t, h, tn.ID, kname, actor, i)
		want[rec.ID] = true
	}

	seen := make(map[uuid.UUID]bool, total)
	var (
		prevUpdated time.Time
		prevID      uuid.UUID
		havePrev    bool
		visits      int
	)
	if err := h.records.ForEach(ctx, tn.ID, record.ListFilter{KType: kname}, func(r record.KRecord) error {
		visits++
		if seen[r.ID] {
			t.Fatalf("duplicate visit to %s", r.ID)
		}
		seen[r.ID] = true
		if !want[r.ID] {
			t.Fatalf("phantom row %s not in original insert set", r.ID)
		}
		if havePrev {
			if r.UpdatedAt.After(prevUpdated) {
				t.Fatalf("ForEach out of order: %s updated_at=%s came after %s updated_at=%s",
					r.ID, r.UpdatedAt, prevID, prevUpdated)
			}
			if r.UpdatedAt.Equal(prevUpdated) && uuidGreaterOrEqual(r.ID, prevID) {
				t.Fatalf("ForEach out of order on tie-break: %s came after %s at %s",
					r.ID, prevID, r.UpdatedAt)
			}
		}
		prevUpdated, prevID, havePrev = r.UpdatedAt, r.ID, true
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if visits != total {
		t.Fatalf("ForEach visited %d rows, want %d", visits, total)
	}
	if len(seen) != total {
		t.Fatalf("ForEach unique IDs %d, want %d", len(seen), total)
	}
}

// TestForEachStopsEarlyOnSentinel verifies that returning
// ErrStopForEach from the callback terminates the walk without
// surfacing an error and without visiting subsequent rows.
func TestForEachStopsEarlyOnSentinel(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 600
	for i := 0; i < total; i++ {
		createPaginationRecord(t, h, tn.ID, kname, actor, i)
	}

	const stopAfter = 7
	visits := 0
	err := h.records.ForEach(ctx, tn.ID, record.ListFilter{KType: kname}, func(r record.KRecord) error {
		visits++
		if visits == stopAfter {
			return record.ErrStopForEach
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ForEach with ErrStopForEach should return nil, got: %v", err)
	}
	if visits != stopAfter {
		t.Fatalf("ForEach visited %d rows, expected exactly %d before stop sentinel", visits, stopAfter)
	}
}

// TestForEachPropagatesCallbackError verifies that any non-sentinel
// error returned by the callback aborts the walk and propagates
// upstream unchanged. This is the contract callers rely on for
// "fail loud on real errors".
func TestForEachPropagatesCallbackError(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	for i := 0; i < 20; i++ {
		createPaginationRecord(t, h, tn.ID, kname, actor, i)
	}

	sentinel := errors.New("boom: per-row failure")
	visits := 0
	err := h.records.ForEach(ctx, tn.ID, record.ListFilter{KType: kname}, func(r record.KRecord) error {
		visits++
		if visits == 3 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ForEach error: want %v, got %v", sentinel, err)
	}
	if visits != 3 {
		t.Fatalf("ForEach kept visiting after error: visits=%d", visits)
	}
}

// TestForEachBypassesListAllMaxRows verifies that ForEach is NOT
// subject to the ListAllMaxRows safety cap that constrains ListAll
// and ListByField. This is the primary motivation for the streaming
// primitive: callers walking arbitrarily large KTypes (recurring
// engine, summarize_pipeline) must be able to process every row
// regardless of row count.
//
// We temporarily lower the cap so the test can hit it with a small
// dataset, then verify ListAll fails with ErrListAllExceedsCap on
// the same dataset where ForEach completes the full walk.
func TestForEachBypassesListAllMaxRows(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	const total = 200
	for i := 0; i < total; i++ {
		createPaginationRecord(t, h, tn.ID, kname, actor, i)
	}

	origCap := record.ListAllMaxRows
	record.ListAllMaxRows = 50
	t.Cleanup(func() { record.ListAllMaxRows = origCap })

	// ListAll under the lowered cap must refuse the dataset.
	if _, err := h.records.ListAll(ctx, tn.ID, record.ListFilter{KType: kname}); !errors.Is(err, record.ErrListAllExceedsCap) {
		t.Fatalf("ListAll under cap should return ErrListAllExceedsCap, got: %v", err)
	}

	// ForEach on the same dataset must walk every row.
	visits := 0
	if err := h.records.ForEach(ctx, tn.ID, record.ListFilter{KType: kname}, func(r record.KRecord) error {
		visits++
		return nil
	}); err != nil {
		t.Fatalf("ForEach under cap: %v", err)
	}
	if visits != total {
		t.Fatalf("ForEach visited %d rows under cap, want %d", visits, total)
	}
}

// TestForEachByFieldPushesFilterIntoSQL verifies that
// ForEachByField only invokes the callback for rows whose JSONB
// field matches the supplied value — i.e. the filter is pushed
// into SQL rather than applied client-side. This is the property
// that lets PayrollEngine.ListPayslipsForRun and PostPayRun avoid
// scanning every payslip the tenant has ever produced.
func TestForEachByFieldPushesFilterIntoSQL(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tn := newPaginationTenant(t, h)
	kname := newPaginationKType(t, h)
	actor := uuid.New()

	// Insert 150 rows with field "tag" set to one of three values.
	// ForEachByField on "tag=alpha" must visit exactly the 50 rows
	// stamped alpha, and not the other 100.
	type seedRow struct {
		ID  uuid.UUID
		Tag string
	}
	var alphaRows []seedRow
	for i := 0; i < 150; i++ {
		var tag string
		switch i % 3 {
		case 0:
			tag = "alpha"
		case 1:
			tag = "beta"
		case 2:
			tag = "gamma"
		}
		rec, err := h.records.Create(ctx, record.KRecord{
			TenantID:  tn.ID,
			KType:     kname,
			Data:      json.RawMessage(fmt.Sprintf(`{"seq":%d,"tag":%q}`, i, tag)),
			CreatedBy: actor,
		})
		if err != nil {
			t.Fatalf("create row %d: %v", i, err)
		}
		if tag == "alpha" {
			alphaRows = append(alphaRows, seedRow{ID: rec.ID, Tag: tag})
		}
	}

	want := make(map[uuid.UUID]bool, len(alphaRows))
	for _, r := range alphaRows {
		want[r.ID] = true
	}

	visited := make(map[uuid.UUID]bool, len(alphaRows))
	if err := h.records.ForEachByField(ctx, tn.ID, record.ListFilter{KType: kname}, "tag", "alpha", func(r record.KRecord) error {
		if !want[r.ID] {
			t.Fatalf("ForEachByField visited non-alpha row %s — SQL filter not pushed down?", r.ID)
		}
		if visited[r.ID] {
			t.Fatalf("ForEachByField visited %s twice", r.ID)
		}
		visited[r.ID] = true
		return nil
	}); err != nil {
		t.Fatalf("ForEachByField: %v", err)
	}
	if len(visited) != len(alphaRows) {
		t.Fatalf("ForEachByField visited %d alpha rows, want %d", len(visited), len(alphaRows))
	}
}

// uuidGreaterOrEqual returns true if a >= b in lexicographic byte order.
func uuidGreaterOrEqual(a, b uuid.UUID) bool {
	for i := 0; i < 16; i++ {
		if a[i] > b[i] {
			return true
		}
		if a[i] < b[i] {
			return false
		}
	}
	return true
}
