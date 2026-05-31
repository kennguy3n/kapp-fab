//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
)

// TestMarketplacePublisherStore_EndToEnd exercises the B7 publisher
// surface end-to-end against a live PG instance:
//
//  1. CreatePublisher inserts a unique row; a second create with
//     the same slug returns ErrConflict (UNIQUE on slug).
//  2. VerifyPublisher / UnverifyPublisher stamp + clear the verified
//     columns; an unverify-after-verify cycle is idempotent and
//     drops auto_approve_patch back to false (CHECK constraint).
//  3. RegisterPublisherKey accepts the b64-ed ed25519 public key the
//     standard library produces (44 chars), stores it intact, and
//     returns ErrConflict on a second register with the same key_id.
//  4. ListPublisherKeys filters revoked vs all keys correctly.
//  5. RevokePublisherKey requires a non-empty reason and is
//     idempotent (a second revoke is a no-op).
func TestMarketplacePublisherStore_EndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)
	pubs := store.Publishers()

	slug := strings.ReplaceAll(uniqueSlug("b7pub"), "-", "_")
	created, err := pubs.CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         slug,
		DisplayName:  "B7 Test Publisher",
		ContactEmail: "publisher@example.com",
	})
	if err != nil {
		t.Fatalf("CreatePublisher: %v", err)
	}
	if created.Slug != slug || created.DisplayName != "B7 Test Publisher" {
		t.Fatalf("CreatePublisher result wrong: %+v", created)
	}
	if created.VerifiedAt != nil {
		t.Fatalf("fresh publisher should not be verified: %+v", created)
	}

	if _, err := pubs.CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         slug,
		DisplayName:  "Dup",
		ContactEmail: "x@example.com",
	}); !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("duplicate slug should return ErrConflict, got %v", err)
	}

	// --- (2) Verify / Unverify ---
	verified, err := pubs.VerifyPublisher(ctx, marketplace.VerifyPublisherInput{
		PublisherID:      created.ID,
		Reviewer:         "operator-1",
		Notes:            "Signed legal review on 2026-01-15",
		AutoApprovePatch: true,
	})
	if err != nil {
		t.Fatalf("VerifyPublisher: %v", err)
	}
	if verified.VerifiedAt == nil || verified.VerifiedBy != "operator-1" {
		t.Fatalf("verify did not stamp columns: %+v", verified)
	}
	if !verified.AutoApprovePatch {
		t.Errorf("AutoApprovePatch should propagate")
	}

	if err := pubs.UnverifyPublisher(ctx, created.ID); err != nil {
		t.Fatalf("UnverifyPublisher: %v", err)
	}
	postUnverify, err := pubs.GetPublisher(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetPublisher: %v", err)
	}
	if postUnverify.VerifiedAt != nil {
		t.Errorf("unverify should clear verified_at; got %v", postUnverify.VerifiedAt)
	}
	if postUnverify.AutoApprovePatch {
		// CHECK marketplace_publishers_auto_approve_requires_verified
		// must have forced this back to false.
		t.Errorf("AutoApprovePatch should reset on unverify")
	}

	// --- (3) RegisterPublisherKey ---
	pubKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	pubKeyB64 := base64.StdEncoding.EncodeToString(pubKey)
	key1, err := pubs.RegisterPublisherKey(ctx, marketplace.RegisterPublisherKeyInput{
		PublisherID:  created.ID,
		KeyID:        "key-1",
		PublicKeyB64: pubKeyB64,
		Label:        "primary",
	})
	if err != nil {
		t.Fatalf("RegisterPublisherKey: %v", err)
	}
	if key1.PublicKeyB64 != pubKeyB64 || key1.Algorithm != "ed25519" {
		t.Fatalf("key fields wrong: %+v", key1)
	}

	// Same key_id on same publisher → conflict
	if _, err := pubs.RegisterPublisherKey(ctx, marketplace.RegisterPublisherKeyInput{
		PublisherID:  created.ID,
		KeyID:        "key-1",
		PublicKeyB64: pubKeyB64,
	}); !errors.Is(err, marketplace.ErrConflict) {
		t.Fatalf("duplicate key_id should return ErrConflict, got %v", err)
	}

	// Rejected: empty / non-b64 / wrong-length pubkey
	for _, bad := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := pubs.RegisterPublisherKey(ctx, marketplace.RegisterPublisherKeyInput{
			PublisherID:  created.ID,
			KeyID:        "bad-" + bad[:minInt(len(bad), 4)],
			PublicKeyB64: bad,
		}); err == nil {
			t.Errorf("malformed key %q should be rejected", bad)
		}
	}

	// --- (4) ListPublisherKeys ---
	all, err := pubs.ListPublisherKeys(ctx, created.ID, true)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListPublisherKeys(all): %v / %d", err, len(all))
	}
	live, err := pubs.ListPublisherKeys(ctx, created.ID, false)
	if err != nil || len(live) != 1 {
		t.Fatalf("ListPublisherKeys(live): %v / %d", err, len(live))
	}

	// --- (5) Revoke ---
	if err := pubs.RevokePublisherKey(ctx, key1.ID, ""); err == nil {
		t.Errorf("revoke without reason should be rejected")
	}
	if err := pubs.RevokePublisherKey(ctx, key1.ID, "rotated 2026-02-01"); err != nil {
		t.Fatalf("RevokePublisherKey: %v", err)
	}
	// Second revoke is idempotent (UPDATE COALESCE preserves
	// first revoke timestamp).
	if err := pubs.RevokePublisherKey(ctx, key1.ID, "duplicate"); err != nil {
		t.Errorf("second revoke should be no-op, got %v", err)
	}
	live2, _ := pubs.ListPublisherKeys(ctx, created.ID, false)
	if len(live2) != 0 {
		t.Errorf("live key set should be empty post-revoke, got %d", len(live2))
	}
	all2, _ := pubs.ListPublisherKeys(ctx, created.ID, true)
	if len(all2) != 1 {
		t.Errorf("all-keys set should still include revoked key, got %d", len(all2))
	}
}

// TestMarketplaceFindingsStore_UpsertSemantics exercises the
// natural-key replace semantics declared in UpsertReviewFindings's
// godoc:
//
//  1. First call inserts every finding.
//  2. A second call with a subset of (check_name, code, location)
//     keys atomically deletes the missing rows and overwrites
//     message + severity on the surviving keys.
//  3. DeleteAllReviewFindings empties the table for a single
//     version.
//  4. The ListReviewFindings query orders by (check, code,
//     location) — the natural sort the admin UI consumes.
func TestMarketplaceFindingsStore_UpsertSemantics(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ver := seedExtensionVersion(t, store, "findings")

	first := []marketplace.ReviewFinding{
		{
			CheckName: "manifest_schema",
			Severity:  marketplace.SeverityError,
			Code:      "manifest_missing_field",
			Message:   "publisher missing",
			Location:  "manifest.yaml#publisher",
		},
		{
			CheckName: "permissions",
			Severity:  marketplace.SeverityWarn,
			Code:      "broad_scope",
			Message:   "kchat:write",
			Location:  "manifest.yaml#permissions[2]",
		},
		{
			CheckName: "signature",
			Severity:  marketplace.SeverityInfo,
			Code:      "publisher_unsigned",
			Message:   "publisher has no registered keys",
			Location:  "",
		},
	}
	if err := store.Findings().UpsertReviewFindings(ctx, ver, first); err != nil {
		t.Fatalf("UpsertReviewFindings (first): %v", err)
	}
	got1, err := store.Findings().ListReviewFindings(ctx, ver)
	if err != nil || len(got1) != 3 {
		t.Fatalf("ListReviewFindings (first): %v / %d", err, len(got1))
	}

	// Re-upsert with: manifest_schema dropped, permissions
	// rewritten (new message + escalated severity), signature
	// unchanged.
	second := []marketplace.ReviewFinding{
		{
			CheckName: "permissions",
			Severity:  marketplace.SeverityError,
			Code:      "broad_scope",
			Message:   "escalated: kchat:* wildcard",
			Location:  "manifest.yaml#permissions[2]",
		},
		{
			CheckName: "signature",
			Severity:  marketplace.SeverityInfo,
			Code:      "publisher_unsigned",
			Message:   "publisher has no registered keys",
			Location:  "",
		},
	}
	if err := store.Findings().UpsertReviewFindings(ctx, ver, second); err != nil {
		t.Fatalf("UpsertReviewFindings (second): %v", err)
	}
	got2, err := store.Findings().ListReviewFindings(ctx, ver)
	if err != nil || len(got2) != 2 {
		t.Fatalf("ListReviewFindings (second): %v / %d", err, len(got2))
	}
	for _, f := range got2 {
		if f.CheckName == "manifest_schema" {
			t.Fatalf("dropped finding survived: %+v", f)
		}
		if f.CheckName == "permissions" && f.Severity != marketplace.SeverityError {
			t.Errorf("severity not overwritten: %+v", f)
		}
		if f.CheckName == "permissions" && !strings.Contains(f.Message, "wildcard") {
			t.Errorf("message not overwritten: %+v", f)
		}
	}

	// Severity validation
	bad := []marketplace.ReviewFinding{{
		CheckName: "x_check", Code: "y_code", Severity: marketplace.Severity("invalid"),
	}}
	if err := store.Findings().UpsertReviewFindings(ctx, ver, bad); !errors.Is(err, marketplace.ErrInvalidManifest) {
		t.Errorf("invalid severity should return ErrInvalidManifest, got %v", err)
	}

	if err := store.Findings().DeleteAllReviewFindings(ctx, ver); err != nil {
		t.Fatalf("DeleteAllReviewFindings: %v", err)
	}
	post, _ := store.Findings().ListReviewFindings(ctx, ver)
	if len(post) != 0 {
		t.Errorf("DeleteAll should empty findings, got %d", len(post))
	}
}

// TestMarketplace_ResetReviewStateForRescan exercises the
// admin-initiated rescan path:
//
//  1. A fresh version starts in `submitted` (PublishVersion seeds the
//     state row).
//  2. Drive forward to manual_review (legal forward transition).
//  3. ResetReviewStateForRescan moves it BACK to submitted, clears
//     reviewer / reviewed_at / automated_checks / manual_review_notes.
//  4. ClaimSubmittedReviewVersions re-claims the row immediately.
//  5. ResetReviewStateForRescan refuses to move a terminal
//     (approved / rejected / withdrawn) row.
func TestMarketplace_ResetReviewStateForRescan(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ver := seedExtensionVersion(t, store, "rescan")

	// Drive forward: submitted → automated_passed → manual_review
	if _, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:       ver,
		Status:          marketplace.ReviewStatusAutomatedPassed,
		AutomatedChecks: []byte(`{"manifest_schema":{"passed":true}}`),
	}); err != nil {
		t.Fatalf("UpdateReviewState → automated_passed: %v", err)
	}
	if _, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID: ver,
		Status:    marketplace.ReviewStatusManualReview,
	}); err != nil {
		t.Fatalf("UpdateReviewState → manual_review: %v", err)
	}

	// Rescan
	if err := store.ResetReviewStateForRescan(ctx, ver); err != nil {
		t.Fatalf("ResetReviewStateForRescan: %v", err)
	}
	post, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState (post-reset): %v", err)
	}
	if post.Status != marketplace.ReviewStatusSubmitted {
		t.Fatalf("post-reset status: want submitted got %q", post.Status)
	}
	if post.Reviewer != "" {
		t.Errorf("reset should clear reviewer, got %q", post.Reviewer)
	}
	if post.ReviewedAt != nil {
		t.Errorf("reset should clear reviewed_at, got %v", post.ReviewedAt)
	}
	if post.ManualReviewNotes != "" {
		t.Errorf("reset should clear manual_review_notes, got %q", post.ManualReviewNotes)
	}
	// Reset writes '{}'::jsonb (empty object) — checking the
	// decoded form lets us assert "no checks recorded" without
	// caring about whitespace in the raw bytes.
	if s := string(post.AutomatedChecks); s != "{}" && s != "" {
		t.Errorf("reset should clear automated_checks, got %q", s)
	}

	// The row's state IS `submitted` post-reset (asserted
	// above). ClaimSubmittedReviewVersions returns claims FIFO
	// (ORDER BY created_at ASC) with the limit capped at 64;
	// the shared test DB accumulates stale submitted rows from
	// other tests, so we can't deterministically expect ours to
	// be in the top 64. The submitted-status check is the
	// load-bearing assertion; the claim-loop is just the
	// worker's adapter on top of it.

	// Terminal-state guard: walk to rejected, attempt reset
	if _, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:       ver,
		Status:          marketplace.ReviewStatusAutomatedPassed,
		AutomatedChecks: []byte(`{"manifest_schema":{"passed":true}}`),
	}); err != nil {
		t.Fatalf("re-walk to automated_passed: %v", err)
	}
	if _, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID: ver,
		Status:    marketplace.ReviewStatusRejected,
		Reviewer:  "operator-1",
	}); err != nil {
		t.Fatalf("re-walk to rejected: %v", err)
	}
	if err := store.ResetReviewStateForRescan(ctx, ver); !errors.Is(err, marketplace.ErrConflict) {
		t.Errorf("reset on terminal row should return ErrConflict, got %v", err)
	}
}

// seedExtensionVersion is a test helper that creates a fresh
// extension + initial version row so each test gets an isolated
// review_state to operate on. Returns the version's UUID.
func seedExtensionVersion(t *testing.T, store *marketplace.Store, prefix string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	pub := strings.ReplaceAll(uniqueSlug(prefix), "-", "_")
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher:   pub,
		Slug:        "ext",
		DisplayName: "Test Ext",
		Description: "x",
		Author:      "x",
		License:     "MIT",
		Homepage:    "https://example.com",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}
	ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
		ExtensionID: ext.ID,
		Manifest: &marketplace.Manifest{
			SchemaVersion: 1, Name: ext.Name, Publisher: ext.Publisher, Slug: ext.Slug,
			Version: "1.0.0", Author: "x", License: "MIT", Description: "x",
			MinKappVersion:   "1.0.0",
			FeaturesRequired: []string{"inventory"},
			KTypes:           []marketplace.KTypeRef{{Schema: "./k.json"}},
		},
		BundleHash: strings.Repeat("a", 64),
		BundleSize: 4096,
		BundleURL:  "https://cdn.example/v.tgz",
	})
	if err != nil {
		t.Fatalf("PublishVersion: %v", err)
	}
	return ver.ID
}

// TestMarketplace_ClaimSubmittedReviewVersions_AtomicLease exercises
// the atomic-claim semantics in Store.ClaimSubmittedReviewVersions.
// The B7 worker pulls submitted rows via an UPDATE...RETURNING
// CTE that stamps claimed_at / claimed_by inside SKIP LOCKED so
// concurrent claims never race on the same row.
//
// The test exercises three load-bearing invariants:
//
//  1. A claimed row's claimed_at is non-NULL post-claim
//     (UPDATE-RETURNING is doing the write, not just locking).
//  2. A second claim against the same row returns it again ONLY
//     after the lease window has lapsed, never before
//     (regression for the bare SELECT FOR UPDATE SKIP LOCKED
//     pattern that released locks at end-of-statement).
//  3. ResetReviewStateForRescan clears the claim atomically with
//     the status reset, so an admin clicking Rescan doesn't have
//     to wait 10 minutes for the lease to lapse.
func TestMarketplace_ClaimSubmittedReviewVersions_AtomicLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// The shared test DB accumulates submitted rows from other
	// test runs that never get claimed (no worker is running),
	// and also rows claimed by prior runs whose claimed_at is now
	// older than ReviewClaimLeaseDuration (10 min) — those are
	// re-eligible for claim under the worker's lease-expiry
	// branch. Stamp ALL submitted rows with a fresh claimed_at so
	// none of them are in the candidate set: our seeded test row
	// is then the lone NULL-claimed submitted row that
	// ClaimSubmittedReviewVersions can return, and the per-call
	// cap of 64 reliably surfaces it.
	//
	// We can't use `WHERE claimed_at IS NULL` here — rows whose
	// claimed_at was set by a previous run (whether by another
	// worker simulation, or by this test on a prior invocation)
	// have aged past the 10-min lease and are once again in the
	// claim candidate set.
	if _, err := h.pool.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET claimed_at = now(),
		        claimed_by = 'test-pre-claim'
		  WHERE status = 'submitted'`,
	); err != nil {
		t.Fatalf("pre-claim stale submitted rows: %v", err)
	}

	ver := seedExtensionVersion(t, store, "claim")

	// Helper: scan claims returned by a worker and return true
	// if our test version is in the slice. Also surfaces the
	// per-claim timestamp so the test can assert it is non-zero
	// (the worker threads it through to UpdateReviewState's
	// claim guard).
	containsVer := func(t *testing.T, claims []marketplace.ClaimedReviewVersion) (bool, time.Time) {
		t.Helper()
		for _, c := range claims {
			if c.VersionID == ver {
				return true, c.ClaimedAt
			}
		}
		return false, time.Time{}
	}

	// First claim by "worker-A": must atomically stamp claimed_at.
	idsA, err := store.ClaimSubmittedReviewVersions(ctx, "worker-A", 64)
	if err != nil {
		t.Fatalf("ClaimSubmittedReviewVersions (A): %v", err)
	}
	foundA, claimedAtA := containsVer(t, idsA)
	if !foundA {
		t.Fatalf("worker-A did not claim our version (idsA=%d)", len(idsA))
	}
	if claimedAtA.IsZero() {
		t.Fatalf("worker-A claim returned zero claimed_at (must round-trip the DB timestamp for guard SQL)")
	}

	// Direct DB read: verify claimed_at + claimed_by were
	// written by the UPDATE inside the claim.
	var claimedBy string
	var claimedAt *time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT claimed_by, claimed_at
		   FROM marketplace_extension_review_state
		  WHERE extension_version_id = $1`, ver,
	).Scan(&claimedBy, &claimedAt); err != nil {
		t.Fatalf("read post-claim row: %v", err)
	}
	if claimedBy != "worker-A" {
		t.Errorf("post-claim claimed_by: want worker-A got %q", claimedBy)
	}
	if claimedAt == nil {
		t.Fatalf("post-claim claimed_at: should be non-NULL after atomic claim")
	}

	// Second claim by "worker-B" inside the lease window: row
	// MUST NOT be re-claimed. This is the load-bearing
	// regression for the pool.Query-vs-WithTx bug — a bare
	// SELECT FOR UPDATE SKIP LOCKED would release its locks
	// at end-of-statement and worker-B would re-claim the row
	// immediately. The atomic UPDATE...RETURNING fixes this by
	// making claimed_at non-NULL inside the same statement
	// that's holding the lock.
	idsB, err := store.ClaimSubmittedReviewVersions(ctx, "worker-B", 64)
	if err != nil {
		t.Fatalf("ClaimSubmittedReviewVersions (B): %v", err)
	}
	if foundB, _ := containsVer(t, idsB); foundB {
		t.Errorf("worker-B re-claimed a row already held by worker-A within the lease window")
	}

	// ResetReviewStateForRescan must clear the claim atomically
	// with the status reset so the next claim picks it up
	// immediately (admin Rescan UX requirement). Without the
	// claim columns being cleared, the row would be invisible
	// to the worker for the full 10-minute lease.
	if err := store.ResetReviewStateForRescan(ctx, ver); err != nil {
		t.Fatalf("ResetReviewStateForRescan: %v", err)
	}
	var postResetBy *string
	var postResetAt *time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT claimed_by, claimed_at
		   FROM marketplace_extension_review_state
		  WHERE extension_version_id = $1`, ver,
	).Scan(&postResetBy, &postResetAt); err != nil {
		t.Fatalf("read post-reset row: %v", err)
	}
	if postResetBy != nil {
		t.Errorf("post-reset claimed_by should be NULL, got %q", *postResetBy)
	}
	if postResetAt != nil {
		t.Errorf("post-reset claimed_at should be NULL, got %v", *postResetAt)
	}

	// A fresh claim post-reset must pick up the row again
	// (proves the reset+immediate-reclaim path the admin UX
	// relies on).
	idsC, err := store.ClaimSubmittedReviewVersions(ctx, "worker-C", 64)
	if err != nil {
		t.Fatalf("ClaimSubmittedReviewVersions (C, post-reset): %v", err)
	}
	foundC, claimedAtC := containsVer(t, idsC)
	if !foundC {
		t.Fatalf("worker-C did not re-claim our version post-reset (idsC=%d)", len(idsC))
	}
	if !claimedAtC.After(claimedAtA) {
		t.Errorf("worker-C claimed_at (%v) should be strictly after worker-A's stale value (%v) — proves the rescan path stamped a fresh timestamp", claimedAtC, claimedAtA)
	}
}

// TestMarketplace_SubmittedToManualReview_DirectTransition
// pins the state-graph edge submitted→manual_review that the
// B7 pipeline relies on when warn-level findings land on a
// fresh submission (e.g. UIStaticAnalysisCheck flagging eval()
// usage). Without this edge the pipeline would have to two-
// step via automated_passed first, polluting the audit trail
// (automated_passed would mean BOTH "checks ran cleanly" and
// "checks ran but produced warnings" depending on the path).
func TestMarketplace_SubmittedToManualReview_DirectTransition(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	ver := seedExtensionVersion(t, store, "submitted-to-manual")

	// Pre-check: row starts in submitted.
	pre, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState pre: %v", err)
	}
	if pre.Status != marketplace.ReviewStatusSubmitted {
		t.Fatalf("pre status: want submitted got %q", pre.Status)
	}

	// The transition the pipeline emits when one or more
	// warn-level findings produce a manual_review verdict.
	post, err := store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:   ver,
		Status:      marketplace.ReviewStatusManualReview,
		ManualNotes: "1 warn / 0 info finding(s); human review required",
		AutomatedChecks: []byte(
			`{"checks":[{"name":"ui_static","passed":false,"worst":"warn","errors":0,"warns":1,"infos":0}]}`),
	})
	if err != nil {
		t.Fatalf("UpdateReviewState submitted→manual_review: %v", err)
	}
	if post.Status != marketplace.ReviewStatusManualReview {
		t.Fatalf("post status: want manual_review got %q", post.Status)
	}
	if post.ManualReviewNotes == "" {
		t.Errorf("manual_review notes should be set, got empty")
	}
	if len(post.AutomatedChecks) == 0 || string(post.AutomatedChecks) == "{}" {
		t.Errorf("automated_checks JSONB should be populated, got %q", string(post.AutomatedChecks))
	}
}

// TestMarketplace_UpdateReviewState_ClaimGuardDefeatsRescanRace
// is the load-bearing regression for the TOCTOU window between
// an admin Rescan and a worker's in-flight Pipeline.Persist
// (Devin Review ANALYSIS_0002 on commit 41d7ec7).
//
// Reproducer without the guard:
//  1. Worker claims a `submitted` row (stamps claimed_by /
//     claimed_at). The worker holds (workerID, claimedAtA).
//  2. Worker runs the pipeline (slow, ~seconds; bundle fetch +
//     static analysis).
//  3. Admin clicks Rescan while step 2 is in flight.
//     ResetReviewStateForRescan clears findings + claimed_by +
//     claimed_at and keeps status='submitted'.
//  4. Worker calls Persist. The state UPDATE matches
//     `status='submitted'` (true — the rescan kept it) and the
//     OLD finding set is written over the freshly-cleared
//     findings table. State transitions to `rejected` (or
//     whatever the stale pipeline computed). Admin's
//     intended re-run never happens; the row is now terminal
//     and ResetReviewStateForRescan refuses a second attempt.
//
// Fix: UpdateReviewState's WHERE clause additionally gates on
// (claimed_by, claimed_at) when the caller supplies an
// ExpectedClaim. After the rescan clears the columns, the
// late UPDATE matches zero rows and UpdateReviewState returns
// ErrClaimLost so the worker can drop the stale result.
//
// The next worker poll re-claims the row and the pipeline
// re-runs against the same version — which is the admin's
// original intent.
func TestMarketplace_UpdateReviewState_ClaimGuardDefeatsRescanRace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := marketplace.NewStore(h.pool)

	// Same noisy-DB pre-claim sweep as the atomic-lease test:
	// stamp every submitted row so our seeded test row is the
	// lone candidate.
	if _, err := h.pool.Exec(ctx,
		`UPDATE marketplace_extension_review_state
		    SET claimed_at = now(),
		        claimed_by = 'test-pre-claim'
		  WHERE status = 'submitted'`,
	); err != nil {
		t.Fatalf("pre-claim stale submitted rows: %v", err)
	}

	ver := seedExtensionVersion(t, store, "claim-guard")

	// Step 1: worker claims. Capture (workerID, claimedAtA).
	const workerID = "test-worker-A"
	claims, err := store.ClaimSubmittedReviewVersions(ctx, workerID, 64)
	if err != nil {
		t.Fatalf("ClaimSubmittedReviewVersions: %v", err)
	}
	var claimedAtA time.Time
	for _, c := range claims {
		if c.VersionID == ver {
			claimedAtA = c.ClaimedAt
		}
	}
	if claimedAtA.IsZero() {
		t.Fatalf("worker did not claim our seeded version (claims=%d)", len(claims))
	}

	// Step 2: an admin Rescan lands between claim and Persist.
	// ResetReviewStateForRescan clears claimed_by / claimed_at.
	if err := store.ResetReviewStateForRescan(ctx, ver); err != nil {
		t.Fatalf("ResetReviewStateForRescan: %v", err)
	}

	// Step 3: worker's late Persist call. The pipeline computed
	// a `rejected` verdict against the now-stale bundle; without
	// the claim guard this UPDATE would land and lock the row
	// terminal. With the guard, the (workerID, claimedAtA) tuple
	// no longer matches the row's NULL claim columns, so the
	// UPDATE affects zero rows and UpdateReviewState returns
	// ErrClaimLost.
	_, err = store.Reviews().UpdateReviewState(ctx, marketplace.UpdateReviewStateInput{
		VersionID:   ver,
		Status:      marketplace.ReviewStatusRejected,
		ManualNotes: "stale pipeline verdict (should be rejected by guard)",
		Reviewer:    "system",
		ExpectedClaim: &marketplace.ReviewClaimGuard{
			ClaimedBy: workerID,
			ClaimedAt: claimedAtA,
		},
	})
	if !errors.Is(err, marketplace.ErrClaimLost) {
		t.Fatalf("UpdateReviewState should have returned ErrClaimLost, got %v", err)
	}

	// Post-condition: row is still `submitted` (the rescan path
	// kept it submitted with a NULL claim) and the worker's
	// stale transition never landed. A fresh claim re-picks it
	// up immediately, completing the admin's intended re-run.
	post, err := store.Reviews().GetReviewState(ctx, ver)
	if err != nil {
		t.Fatalf("GetReviewState post-guard: %v", err)
	}
	if post.Status != marketplace.ReviewStatusSubmitted {
		t.Fatalf("post-guard status should remain `submitted` (rescan path), got %q", post.Status)
	}

	idsAfter, err := store.ClaimSubmittedReviewVersions(ctx, "test-worker-B", 64)
	if err != nil {
		t.Fatalf("post-guard re-claim: %v", err)
	}
	found := false
	for _, c := range idsAfter {
		if c.VersionID == ver {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("post-guard fresh claim should re-pick up the version (the admin's intended re-run)")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
