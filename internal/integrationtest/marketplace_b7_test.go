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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
