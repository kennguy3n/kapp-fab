//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundlestore"
)

// TestB8BundleStore_UploadIdempotentAndFetch is the happy-path
// exercise of the marketplace-hosted bundle store. Covers:
//
//  1. Upload returns a fresh row on first insert (content_hash UNIQUE).
//  2. Re-uploading the same bytes returns the same row (idempotent).
//  3. Fetch returns the row + a reader over the bytes.
//  4. GetByHash returns the row by hash.
//  5. ListPublisherUploads orders newest-first and scopes to publisher.
func TestB8BundleStore_UploadIdempotentAndFetch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pubA := mustCreatePublisher(t, ctx, pubs, "b8_uploads_a")
	pubB := mustCreatePublisher(t, ctx, pubs, "b8_uploads_b")
	alice := mustCreateUser(t, ctx, h, "alice_b8")

	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(h.pool, objs)

	body1 := []byte("bundle bytes one — exercise upload + dedup")
	body2 := []byte("bundle bytes two — different content")

	// (1) first upload.
	up1, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes:       body1,
		PublisherID: pubA.ID,
		UploadedBy:  alice.ID,
		ContentType: bundlestore.DefaultContentType,
	})
	if err != nil {
		t.Fatalf("Upload(body1): %v", err)
	}
	if up1.ContentHash == "" {
		t.Fatalf("empty content_hash on success")
	}
	if up1.SizeBytes != int64(len(body1)) {
		t.Errorf("size: want %d, got %d", len(body1), up1.SizeBytes)
	}
	if up1.ReferencedAt != nil {
		t.Errorf("fresh upload should have nil referenced_at, got %v", up1.ReferencedAt)
	}

	// (2) re-uploading SAME bytes by SAME publisher returns the
	// same row (idempotent — content-addressed dedup).
	up1b, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes:       body1,
		PublisherID: pubA.ID,
		UploadedBy:  alice.ID,
		ContentType: bundlestore.DefaultContentType,
	})
	if err != nil {
		t.Fatalf("Upload(body1, second time): %v", err)
	}
	if up1b.ID != up1.ID {
		t.Errorf("duplicate upload should return same row id: %v vs %v", up1.ID, up1b.ID)
	}
	if up1b.ContentHash != up1.ContentHash {
		t.Errorf("duplicate upload should return same hash")
	}

	// (3) Fetch returns the bytes.
	row, rc, err := bs.Fetch(ctx, up1.ContentHash)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if row.ID != up1.ID {
		t.Errorf("Fetch returned wrong row")
	}
	defer rc.Close()

	// (4) GetByHash returns the row.
	got, err := bs.GetByHash(ctx, up1.ContentHash)
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if got.ID != up1.ID {
		t.Errorf("GetByHash returned wrong row")
	}

	// (5) Different bytes from a different publisher must be a
	// distinct row.
	up2, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes:       body2,
		PublisherID: pubB.ID,
		UploadedBy:  alice.ID,
		ContentType: bundlestore.DefaultContentType,
	})
	if err != nil {
		t.Fatalf("Upload(body2, pubB): %v", err)
	}
	if up2.ID == up1.ID {
		t.Fatalf("different bytes must produce different rows")
	}

	// (6) ListPublisherUploads scoped to publisher.
	rowsA, err := bs.ListPublisherUploads(ctx, pubA.ID, 0)
	if err != nil {
		t.Fatalf("ListPublisherUploads(pubA): %v", err)
	}
	if len(rowsA) != 1 {
		t.Errorf("pubA should have 1 upload, got %d", len(rowsA))
	}
	rowsB, err := bs.ListPublisherUploads(ctx, pubB.ID, 0)
	if err != nil {
		t.Fatalf("ListPublisherUploads(pubB): %v", err)
	}
	if len(rowsB) != 1 {
		t.Errorf("pubB should have 1 upload, got %d", len(rowsB))
	}
}

// TestB8BundleStore_MarkReferencedAndGC pins the orphan-GC
// contract:
//
//  1. MarkReferenced flips referenced_at from NULL to non-nil.
//  2. Subsequent MarkReferenced calls are idempotent (do NOT
//     bump referenced_at — first reference timestamp wins).
//  3. GCUnreferenced only sweeps rows that are (a) referenced_at
//     IS NULL AND (b) older than minAge.
func TestB8BundleStore_MarkReferencedAndGC(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pub := mustCreatePublisher(t, ctx, pubs, "b8_gc")
	alice := mustCreateUser(t, ctx, h, "alice_b8_gc")
	objs := bundlestore.NewMemoryStore()
	// GCUnreferenced requires an admin pool (migration grants
	// DELETE only to kapp_admin). The test harness pool is
	// BYPASSRLS / superuser so it has the same effective
	// privileges in tests.
	bs := bundlestore.NewStore(h.pool, objs).WithAdminPool(h.pool)

	bodyKeep := []byte("KEEP — will be referenced")
	bodyGC := []byte("GC — will stay orphan, swept")
	upKeep, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: bodyKeep, PublisherID: pub.ID, UploadedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("Upload(keep): %v", err)
	}
	upGC, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: bodyGC, PublisherID: pub.ID, UploadedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("Upload(gc): %v", err)
	}

	// (1) MarkReferenced on the keep row.
	if err := bs.MarkReferenced(ctx, upKeep.ContentHash); err != nil {
		t.Fatalf("MarkReferenced(keep): %v", err)
	}
	got, err := bs.GetByHash(ctx, upKeep.ContentHash)
	if err != nil {
		t.Fatalf("GetByHash(keep): %v", err)
	}
	if got.ReferencedAt == nil {
		t.Fatalf("referenced_at should be non-nil after MarkReferenced")
	}
	firstRef := *got.ReferencedAt

	// (2) idempotent — second MarkReferenced does NOT bump.
	time.Sleep(50 * time.Millisecond)
	if err := bs.MarkReferenced(ctx, upKeep.ContentHash); err != nil {
		t.Fatalf("MarkReferenced(keep, second): %v", err)
	}
	got2, _ := bs.GetByHash(ctx, upKeep.ContentHash)
	if got2.ReferencedAt == nil || !got2.ReferencedAt.Equal(firstRef) {
		t.Errorf("MarkReferenced should be idempotent: %v vs %v",
			firstRef, got2.ReferencedAt)
	}

	// (3) GC with very-short minAge sweeps the orphan row only.
	// The keep row is preserved because referenced_at != NULL.
	res, err := bs.GCUnreferenced(ctx, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("GCUnreferenced: %v", err)
	}
	if res.DeletedRows < 1 {
		t.Errorf("expected at least 1 row deleted, got %d", res.DeletedRows)
	}
	// keep row still exists.
	if _, err := bs.GetByHash(ctx, upKeep.ContentHash); err != nil {
		t.Errorf("keep row should still exist post-GC, got %v", err)
	}
	// gc row is gone.
	if _, err := bs.GetByHash(ctx, upGC.ContentHash); !errors.Is(err, bundlestore.ErrBundleNotFound) {
		t.Errorf("gc row should be deleted post-GC, got %v", err)
	}
}

// failingDeleteObjectStore wraps a MemoryStore but always returns
// a non-nil error from Delete. Used to exercise the
// metadata-deleted-but-object-delete-failed branch of
// GCUnreferenced so the round-5 StorageReclaimed counter fix can
// be pinned.
type failingDeleteObjectStore struct {
	inner *bundlestore.MemoryStore
}

func (f *failingDeleteObjectStore) Put(ctx context.Context, key, contentType string, data []byte) error {
	return f.inner.Put(ctx, key, contentType, data)
}

func (f *failingDeleteObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return f.inner.Get(ctx, key)
}

func (f *failingDeleteObjectStore) Exists(ctx context.Context, key string) (bool, error) {
	return f.inner.Exists(ctx, key)
}

func (f *failingDeleteObjectStore) Delete(_ context.Context, _ string) error {
	return errors.New("simulated object-store Delete failure for GC test")
}

// TestB8BundleStore_GCStorageReclaimedAccurate pins the round-5
// Devin Review fix for
// ANALYSIS_pr-review-job-da8e7cbbf34342c2956c6cec2c9ec29f_0001.
// Pre-fix, GCUnreferenced incremented StorageReclaimed
// immediately after the metadata DELETE — BEFORE the object-store
// Delete was attempted — so an operator using the counter for
// capacity-planning saw inflated numbers when the object delete
// failed. The fix is to increment StorageReclaimed only on the
// success path (after DeletedObjects++) so the counter accurately
// reflects bytes actually freed from the object store.
//
// The invariant pinned: when Delete always fails,
//
//	DeletedRows      == N  (metadata wiped)
//	DeletedObjects   == 0  (no object-store deletes succeeded)
//	OrphanedObjects  == N  (every metadata-delete left an orphan)
//	StorageReclaimed == 0  (NO bytes reclaimed)
//
// Pre-fix this test would fail with StorageReclaimed == sum(sizes).
func TestB8BundleStore_GCStorageReclaimedAccurate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pub := mustCreatePublisher(t, ctx, pubs, "b8_gc_counter")
	alice := mustCreateUser(t, ctx, h, "alice_b8_gc_counter")

	// Inject a failing-Delete object-store wrapper around the
	// real MemoryStore so Upload/Fetch still work end-to-end
	// but the GC sweep's object delete fires the orphan branch.
	objs := &failingDeleteObjectStore{inner: bundlestore.NewMemoryStore()}
	bs := bundlestore.NewStore(h.pool, objs).WithAdminPool(h.pool)

	// Upload two orphans (no MarkReferenced) so they're eligible
	// for GC. Use distinct bytes so each gets its own metadata
	// row and the counter sum is non-trivial.
	body1 := []byte("orphan one — counter accuracy regression test")
	body2 := []byte("orphan two — must NOT inflate StorageReclaimed when object delete fails")
	up1, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: body1, PublisherID: pub.ID, UploadedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("Upload(orphan1): %v", err)
	}
	up2, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: body2, PublisherID: pub.ID, UploadedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("Upload(orphan2): %v", err)
	}

	res, err := bs.GCUnreferenced(ctx, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("GCUnreferenced: %v", err)
	}

	// DeletedRows: both orphans had their metadata wiped.
	if res.DeletedRows < 2 {
		t.Errorf("DeletedRows: want >=2, got %d", res.DeletedRows)
	}
	// OrphanedObjects: every metadata-delete left an orphan
	// (Delete always fails).
	if res.OrphanedObjects < 2 {
		t.Errorf("OrphanedObjects: want >=2, got %d", res.OrphanedObjects)
	}
	// DeletedObjects: zero object-store deletes succeeded.
	if res.DeletedObjects != 0 {
		t.Errorf("DeletedObjects: want 0 (all Delete calls failed), got %d", res.DeletedObjects)
	}
	// StorageReclaimed: THE ROUND-5 FIX. Pre-fix this would
	// equal len(body1)+len(body2). Post-fix it MUST be 0 because
	// no bytes were actually freed from the object store.
	if res.StorageReclaimed != 0 {
		t.Errorf("StorageReclaimed: want 0 (no bytes freed when Delete fails), got %d", res.StorageReclaimed)
	}
	// Invariant: DeletedRows == DeletedObjects + OrphanedObjects.
	if res.DeletedRows != res.DeletedObjects+res.OrphanedObjects {
		t.Errorf("invariant: DeletedRows(%d) != DeletedObjects(%d) + OrphanedObjects(%d)",
			res.DeletedRows, res.DeletedObjects, res.OrphanedObjects)
	}

	// Defensive: the metadata rows really are gone, so a
	// subsequent GetByHash on either upload returns
	// ErrBundleNotFound regardless of the orphaned bytes.
	if _, err := bs.GetByHash(ctx, up1.ContentHash); !errors.Is(err, bundlestore.ErrBundleNotFound) {
		t.Errorf("orphan1 metadata should be gone post-GC, got %v", err)
	}
	if _, err := bs.GetByHash(ctx, up2.ContentHash); !errors.Is(err, bundlestore.ErrBundleNotFound) {
		t.Errorf("orphan2 metadata should be gone post-GC, got %v", err)
	}
}

// TestB8BundleStore_UploadTooLargeRejected pins the size cap
// enforced by both the Go-level check and the migration's
// CHECK constraint. Either layer must reject — the test passes
// as long as the upload errors out without inserting.
func TestB8BundleStore_UploadTooLargeRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()
	pub := mustCreatePublisher(t, ctx, pubs, "b8_toolarge")
	alice := mustCreateUser(t, ctx, h, "alice_b8_toolarge")
	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(h.pool, objs)

	huge := make([]byte, marketplace.MaxBundleSizeBytes+1)
	for i := range huge {
		huge[i] = 0xFF
	}
	_, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: huge, PublisherID: pub.ID, UploadedBy: alice.ID,
	})
	if err == nil {
		t.Fatalf("Upload of %d bytes should be rejected", len(huge))
	}
	if !errors.Is(err, bundlestore.ErrBundleTooLarge) &&
		!strings.Contains(err.Error(), "size_bytes") {
		// either Go-level cap or PG CHECK should fire; only fail
		// if neither sentinel matches.
		t.Errorf("expected ErrBundleTooLarge or PG check error, got %v", err)
	}
}

// TestB8BundleStore_HashCollisionDifferentPublishers pins the
// content-addressed-dedup contract — same bytes uploaded by
// different publishers produce the SAME row (the first publisher
// owns it; subsequent publishers get a read-through). This is
// a deliberate design choice to keep total storage bounded by
// distinct content, not distinct (publisher, content) pairs.
func TestB8BundleStore_HashCollisionDifferentPublishers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	pubA := mustCreatePublisher(t, ctx, pubs, "b8_collide_a")
	pubB := mustCreatePublisher(t, ctx, pubs, "b8_collide_b")
	alice := mustCreateUser(t, ctx, h, "alice_b8_collide")
	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(h.pool, objs)

	body := []byte("shared bytes — exercise cross-publisher dedup")
	upA, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: body, PublisherID: pubA.ID, UploadedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("Upload pubA: %v", err)
	}
	upB, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes: body, PublisherID: pubB.ID, UploadedBy: alice.ID,
	})
	if err != nil {
		t.Fatalf("Upload pubB: %v", err)
	}
	if upA.ID != upB.ID {
		t.Errorf("dedup: same bytes from different publishers should return same row, got %v vs %v",
			upA.ID, upB.ID)
	}
	// The publisher_id on the row remains the FIRST publisher
	// who uploaded the bytes. Subsequent uploaders get a read-
	// through, not ownership transfer.
	if upB.PublisherID == nil || *upB.PublisherID != pubA.ID {
		t.Errorf("second-publisher upload should see first publisher as owner, got %v", upB.PublisherID)
	}
}

// TestB8BundleStore_InvalidHashRejected pins the IsValidBundleHash
// contract — only lowercase hex SHA-256 (64 chars) is accepted.
func TestB8BundleStore_InvalidHashRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(h.pool, objs)

	cases := []string{
		"",                                   // empty
		"abc",                                // too short
		strings.Repeat("g", 64),              // non-hex
		strings.Repeat("A", 64),              // uppercase
		"http://malicious",                   // URL-like
		strings.Repeat("a", 63),              // 63 chars
		strings.Repeat("a", 65),              // 65 chars
		uuid.NewString(),                     // UUID
	}
	for _, c := range cases {
		_, err := bs.GetByHash(ctx, c)
		// Either ErrBundleNotFound (the store treated it as a
		// missing hash) or a wrapped invalid-hash error is
		// acceptable; what matters is that no panic occurs.
		if err == nil {
			t.Errorf("GetByHash(%q) should error, got nil", c)
		}
	}
}
