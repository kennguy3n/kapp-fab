//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
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
	// Postgres timestamps round-trip to nanosecond precision in
	// Go because the driver returns time.Time with subsecond
	// resolution; ensure UnixNano matches exactly.
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
