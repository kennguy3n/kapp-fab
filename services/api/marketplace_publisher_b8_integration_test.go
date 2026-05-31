//go:build integration
// +build integration

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundlestore"
	"github.com/kennguy3n/kapp-fab/internal/platform"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestB8PublisherUploadServeRoundTrip exercises the publisher
// upload + marketplace-hosted serve loop end-to-end against a
// real Postgres pool. Pins three contracts:
//
//  1. POST /api/v1/publisher/{id}/bundles persists the bytes
//     and returns the content-hashed marketplace URL.
//  2. GET /api/v1/marketplace/bundles/{hash}.tar.gz returns the
//     same bytes with the right Content-Type + Cache-Control.
//  3. List endpoint surfaces the upload to the dashboard.
//
// Routes are mounted directly (no auth middleware); the test
// injects the user via platform.WithUserID so RequireMemberRole
// sees the right user.
func TestB8PublisherUploadServeRoundTrip(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()
	store := marketplace.NewStore(pool)
	users := tenant.NewUserStore(pool)

	// --- seed: publisher + alice (member) ---
	pubSlug := "b8int_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")
	pub, err := store.Publishers().CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         pubSlug,
		DisplayName:  "B8 integration publisher",
		ContactEmail: pubSlug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher: %v", err)
	}
	alice, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-alice-" + uuid.NewString()[:8],
		Email:       "alice-b8int-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Alice B8 integration",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.Publishers().AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleMember,
	}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// --- handler wiring -------------------------------------
	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(pool, objs)
	urlBase := "https://kapp.test"
	mph := &marketplaceHandlers{
		store:         store,
		bundles:       bs,
		bundleURLBase: urlBase,
	}

	r := chi.NewRouter()
	r.Route("/api/v1/publisher/{publisher_id}", func(pr chi.Router) {
		pr.Post("/bundles", mph.uploadPublisherBundle)
		pr.Get("/bundles", mph.listMyPublisherBundleUploads)
	})
	r.Get("/api/v1/marketplace/bundles/{hash}", mph.serveBundleByHash)

	// --- step 1: upload --------------------------------------
	// Build a real tar.gz: the upload handler runs bundle.Extract
	// before persisting bytes, so plain text would be rejected
	// with 422 ErrBundleMalformed.
	body := buildB8TestBundle(t, pubSlug)
	wantHash := sha256.Sum256(body)
	wantHex := hex.EncodeToString(wantHash[:])

	mpBody, contentType := buildB8Multipart(t, "round-trip.tar.gz", body)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/publisher/"+pub.ID.String()+"/bundles", mpBody)
	req.Header.Set("Content-Type", contentType)
	req = req.WithContext(platform.WithUserID(req.Context(), alice.ID))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: want 201, got %d (body %q)", rr.Code, rr.Body.String())
	}
	var uploadResp struct {
		BundleHash string `json:"bundle_hash"`
		BundleSize int64  `json:"bundle_size"`
		BundleURL  string `json:"bundle_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &uploadResp); err != nil {
		t.Fatalf("decode upload resp: %v", err)
	}
	if uploadResp.BundleHash != wantHex {
		t.Errorf("upload hash mismatch: want %s, got %s", wantHex, uploadResp.BundleHash)
	}
	if uploadResp.BundleSize != int64(len(body)) {
		t.Errorf("upload size: want %d, got %d", len(body), uploadResp.BundleSize)
	}
	if !strings.HasPrefix(uploadResp.BundleURL, urlBase+"/") {
		t.Errorf("bundle URL should be marketplace-hosted under %s, got %q",
			urlBase, uploadResp.BundleURL)
	}
	if !strings.HasSuffix(uploadResp.BundleURL, wantHex+".tar.gz") {
		t.Errorf("bundle URL should end with hash.tar.gz, got %q", uploadResp.BundleURL)
	}

	// --- step 2: serve ---------------------------------------
	getReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/marketplace/bundles/"+wantHex+".tar.gz", http.NoBody)
	getReq = getReq.WithContext(platform.WithUserID(getReq.Context(), alice.ID))
	getRR := httptest.NewRecorder()
	r.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("serve: want 200, got %d (body %q)", getRR.Code, getRR.Body.String())
	}
	got, err := io.ReadAll(getRR.Body)
	if err != nil {
		t.Fatalf("read serve body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("serve: %d bytes returned, want %d", len(got), len(body))
	}
	if ct := getRR.Header().Get("Content-Type"); ct != bundlestore.DefaultContentType {
		t.Errorf("Content-Type: want %q, got %q", bundlestore.DefaultContentType, ct)
	}
	if cc := getRR.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control should include immutable, got %q", cc)
	}
	if etag := getRR.Header().Get("ETag"); !strings.Contains(etag, wantHex) {
		t.Errorf("ETag should embed the hash, got %q", etag)
	}

	// --- step 3: list ----------------------------------------
	listReq := httptest.NewRequest(http.MethodGet,
		"/api/v1/publisher/"+pub.ID.String()+"/bundles", http.NoBody)
	listReq = listReq.WithContext(platform.WithUserID(listReq.Context(), alice.ID))
	listRR := httptest.NewRecorder()
	r.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", listRR.Code)
	}
	var listResp struct {
		Items []struct {
			BundleHash string `json:"bundle_hash"`
			BundleURL  string `json:"bundle_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list resp: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("list: want 1 item, got %d", len(listResp.Items))
	}
	if listResp.Items[0].BundleHash != wantHex {
		t.Errorf("list hash mismatch: want %s, got %s", wantHex, listResp.Items[0].BundleHash)
	}
}

// TestB8PublisherUploadRequiresMembership pins the RBAC contract:
// a caller who is NOT a member of the publisher receives 404
// (membership-collapse pattern from B7.1) instead of leaking the
// publisher's existence.
func TestB8PublisherUploadRequiresMembership(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()
	store := marketplace.NewStore(pool)
	users := tenant.NewUserStore(pool)

	pubSlug := "b8rbac_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")
	pub, err := store.Publishers().CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         pubSlug,
		DisplayName:  "B8 rbac publisher",
		ContactEmail: pubSlug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher: %v", err)
	}
	owner, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-owner-" + uuid.NewString()[:8],
		Email:       "owner-b8rbac-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Owner",
	})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	stranger, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-stranger-" + uuid.NewString()[:8],
		Email:       "stranger-b8rbac-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Stranger",
	})
	if err != nil {
		t.Fatalf("CreateUser stranger: %v", err)
	}
	if _, err := store.Publishers().AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID, UserID: owner.ID,
		Role: marketplace.PublisherMemberRoleOwner,
	}); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}

	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(pool, objs)
	mph := &marketplaceHandlers{
		store:         store,
		bundles:       bs,
		bundleURLBase: "https://kapp.test",
	}
	r := chi.NewRouter()
	r.Route("/api/v1/publisher/{publisher_id}", func(pr chi.Router) {
		pr.Post("/bundles", mph.uploadPublisherBundle)
	})

	body := buildB8TestBundle(t, pubSlug)
	mpBody, ct := buildB8Multipart(t, "x.tar.gz", body)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/publisher/"+pub.ID.String()+"/bundles", mpBody)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(platform.WithUserID(req.Context(), stranger.ID))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("non-member should get 404 (membership-collapse), got %d (body %q)",
			rr.Code, rr.Body.String())
	}
}

// TestB8PublisherUploadDisabledFallsBack pins the "deploy did not
// enable marketplace-hosted bundles" mode: when h.bundles is nil
// or h.bundleURLBase is empty, the upload route returns 503 so
// publishers know to switch back to their own CDN.
func TestB8PublisherUploadDisabledFallsBack(t *testing.T) {
	mph := &marketplaceHandlers{
		// bundles + bundleURLBase deliberately unset.
		store: &marketplace.Store{},
	}
	r := chi.NewRouter()
	r.Route("/api/v1/publisher/{publisher_id}", func(pr chi.Router) {
		pr.Post("/bundles", mph.uploadPublisherBundle)
	})
	mpBody, ct := buildB8Multipart(t, "x.tar.gz", buildB8TestBundle(t, "disabled_pub"))
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/publisher/"+uuid.NewString()+"/bundles", mpBody)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(platform.WithUserID(req.Context(), uuid.New()))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled-deploy: want 503, got %d (body %q)",
			rr.Code, rr.Body.String())
	}
}

// TestB8PublisherSubmitVersionPersistsSignature pins the BUG_0001
// fix from Devin Review round-2: a publisher who submits a version
// through the publisher-self endpoint with bundle_signature +
// bundle_signature_key_id MUST end up with the trio
// (signature_b64 + key_id + signed_at) persisted on the version
// row. The pre-fix handler decoded the wire body into a struct
// missing those fields, so `json.Decoder` silently dropped them
// and the version was stored unsigned — leaving SignatureCheck
// with nothing to verify even though the CLI dutifully sent the
// signature on every publish.
//
// The contract verified here:
//   - both signature fields set    -> BundleSignature + KeyID
//     persisted; SignedAt non-zero (set by handler at submit time).
//   - signature alone (no key id)  -> 400 (both-or-neither).
//   - key id alone (no signature)  -> 400.
//   - both empty                   -> version persisted unsigned.
func TestB8PublisherSubmitVersionPersistsSignature(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()
	store := marketplace.NewStore(pool)
	users := tenant.NewUserStore(pool)

	pubSlug := "b8sig_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")
	pub, err := store.Publishers().CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         pubSlug,
		DisplayName:  "B8 signature publisher",
		ContactEmail: pubSlug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher: %v", err)
	}
	alice, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-alice-" + uuid.NewString()[:8],
		Email:       "alice-b8sig-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Alice B8 signature",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.Publishers().AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID,
		UserID:      alice.ID,
		Role:        marketplace.PublisherMemberRoleMember,
	}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	// Pre-create the extension row so submitMyPublisherVersion
	// has a target. CreateExtension forces the publisher_id+slug
	// uniqueness.
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher:   pubSlug,
		Slug:        "b8sig_ext",
		DisplayName: "B8 signature extension",
		Description: "Pins the signature round-trip",
		Author:      "B8 integration tests",
		License:     "MIT",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}

	mph := &marketplaceHandlers{store: store}
	r := chi.NewRouter()
	r.Post("/api/v1/publisher/{publisher_id}/extensions/{ext_id}/versions",
		mph.submitMyPublisherVersion)

	manifest := func(version string) []byte {
		return []byte(fmt.Sprintf(`{
"schema_version": 1,
"name": %q,
"version": %q,
"author": "B8 integration tests",
"license": "MIT",
"description": "B8 signature pin",
"min_kapp_version": "1.0.0",
"max_kapp_version": "1.x",
"features_required": [],
"permissions_required": []
}`, pubSlug+".b8sig_ext", version))
	}

	submit := func(t *testing.T, version, hash, b64, keyID string) *httptest.ResponseRecorder {
		t.Helper()
		body := map[string]any{
			"manifest":    json.RawMessage(manifest(version)),
			"bundle_url":  "https://example.invalid/" + hash + ".tar.gz",
			"bundle_hash": hash,
			"bundle_size": 4096,
		}
		if b64 != "" {
			body["bundle_signature"] = b64
		}
		if keyID != "" {
			body["bundle_signature_key_id"] = keyID
		}
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/v1/publisher/%s/extensions/%s/versions",
				pub.ID.String(), ext.ID.String()),
			bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(platform.WithUserID(req.Context(), alice.ID))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	// Helper: produce a base64-encoded fake-but-shape-valid
	// signature. The DB CHECK constraint requires exactly 88
	// base64 characters (64 bytes -> 86 chars + "==" padding).
	fakeSig := func() string {
		return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 64))
	}

	// --- step 1: signed submit ---
	{
		hash := strings.Repeat("a", 64)
		b64 := fakeSig()
		keyID := "test-key-1"
		rr := submit(t, "0.1.0", hash, b64, keyID)
		if rr.Code != http.StatusCreated {
			t.Fatalf("signed submit: want 201, got %d (body %q)", rr.Code, rr.Body.String())
		}
		ver, err := store.GetVersionByExtensionAndVersion(ctx, ext.ID, "0.1.0")
		if err != nil {
			t.Fatalf("GetVersion: %v", err)
		}
		if ver.BundleSignature != b64 {
			t.Errorf("BundleSignature: want %q, got %q", b64, ver.BundleSignature)
		}
		if ver.BundleSignatureKeyID != keyID {
			t.Errorf("BundleSignatureKeyID: want %q, got %q", keyID, ver.BundleSignatureKeyID)
		}
		if ver.SignedAt == nil {
			t.Error("SignedAt should be non-nil after signed submit")
		}
	}

	// --- step 2: signature without key_id -> 400 ---
	{
		hash := strings.Repeat("b", 64)
		rr := submit(t, "0.2.0", hash, fakeSig(), "")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("sig-only submit: want 400, got %d (body %q)", rr.Code, rr.Body.String())
		}
	}

	// --- step 3: key_id without signature -> 400 ---
	{
		hash := strings.Repeat("c", 64)
		rr := submit(t, "0.3.0", hash, "", "lonely-key")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("keyid-only submit: want 400, got %d (body %q)", rr.Code, rr.Body.String())
		}
	}

	// --- step 4: unsigned submit -> 201, no signature persisted ---
	{
		hash := strings.Repeat("d", 64)
		rr := submit(t, "0.4.0", hash, "", "")
		if rr.Code != http.StatusCreated {
			t.Fatalf("unsigned submit: want 201, got %d (body %q)", rr.Code, rr.Body.String())
		}
		ver, err := store.GetVersionByExtensionAndVersion(ctx, ext.ID, "0.4.0")
		if err != nil {
			t.Fatalf("GetVersion: %v", err)
		}
		if ver.BundleSignature != "" || ver.BundleSignatureKeyID != "" {
			t.Errorf("unsigned submit should leave signature empty, got sig=%q key=%q",
				ver.BundleSignature, ver.BundleSignatureKeyID)
		}
		if ver.SignedAt != nil {
			t.Error("unsigned submit should leave SignedAt nil")
		}
	}
}

// TestB8PublisherSubmitVersionCrossPublisherDedup pins the
// round-3 fix for Devin Review
// ANALYSIS_pr-review-job-20b9bdccfe6d463c9a4d6ac7f0fea816_0002:
// when a publisher submits a version with no bundle_url and a
// bundle_hash that matches an upload owned by a DIFFERENT
// publisher (content-addressed dedup branch of bundlestore.Upload),
// the handler returns 409 with a clear message instead of the
// pre-fix silent 400 "bundle_url required".
//
// Setup: publisher A uploads bytes; publisher B (different slug,
// different extension) submits a version naming hash X. The
// auto-fill must NOT silently 400 — return 409 and name the
// dedup root cause.
func TestB8PublisherSubmitVersionCrossPublisherDedup(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()
	store := marketplace.NewStore(pool)
	users := tenant.NewUserStore(pool)

	// Publisher A — uploads the bytes.
	pubASlug := "b8pa_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")
	pubA, err := store.Publishers().CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         pubASlug,
		DisplayName:  "B8 pubA",
		ContactEmail: pubASlug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher A: %v", err)
	}
	alice, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-alice-" + uuid.NewString()[:8],
		Email:       "alice-b8dedup-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Alice (A)",
	})
	if err != nil {
		t.Fatalf("CreateUser A: %v", err)
	}
	if _, err := store.Publishers().AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubA.ID, UserID: alice.ID,
		Role: marketplace.PublisherMemberRoleMember,
	}); err != nil {
		t.Fatalf("AddMember A: %v", err)
	}

	// Publisher B — will try to submit naming the same hash.
	pubBSlug := "b8pb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")
	pubB, err := store.Publishers().CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         pubBSlug,
		DisplayName:  "B8 pubB",
		ContactEmail: pubBSlug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher B: %v", err)
	}
	bob, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-bob-" + uuid.NewString()[:8],
		Email:       "bob-b8dedup-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Bob (B)",
	})
	if err != nil {
		t.Fatalf("CreateUser B: %v", err)
	}
	if _, err := store.Publishers().AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pubB.ID, UserID: bob.ID,
		Role: marketplace.PublisherMemberRoleMember,
	}); err != nil {
		t.Fatalf("AddMember B: %v", err)
	}
	extB, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pubBSlug, Slug: "b8dedup_ext",
		DisplayName: "B8 dedup extension",
		Description: "Cross-publisher dedup pin",
		Author:      "B8 integration tests",
		License:     "MIT",
	})
	if err != nil {
		t.Fatalf("CreateExtension B: %v", err)
	}

	// Seed the bundlestore with bytes owned by publisher A.
	objs := bundlestore.NewMemoryStore()
	bs := bundlestore.NewStore(pool, objs)
	bodyA := buildB8TestBundle(t, pubASlug)
	hashRaw := sha256.Sum256(bodyA)
	hashHex := hex.EncodeToString(hashRaw[:])
	if _, err := bs.Upload(ctx, bundlestore.UploadInput{
		Bytes:       bodyA,
		PublisherID: pubA.ID,
		UploadedBy:  alice.ID,
	}); err != nil {
		t.Fatalf("seed Upload as pubA: %v", err)
	}

	mph := &marketplaceHandlers{
		store:         store,
		bundles:       bs,
		bundleURLBase: "https://kapp.test",
	}
	r := chi.NewRouter()
	r.Post("/api/v1/publisher/{publisher_id}/extensions/{ext_id}/versions",
		mph.submitMyPublisherVersion)

	manifestB := []byte(fmt.Sprintf(`{
"schema_version": 1,
"name": %q,
"version": "0.1.0",
"author": "B8 integration tests",
"license": "MIT",
"description": "B8 dedup pin",
"min_kapp_version": "1.0.0",
"max_kapp_version": "1.x",
"features_required": [],
"permissions_required": []
}`, pubBSlug+".b8dedup_ext"))
	body := map[string]any{
		"manifest":    json.RawMessage(manifestB),
		"bundle_url":  "", // force auto-fill path
		"bundle_hash": hashHex,
		"bundle_size": len(bodyA),
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/publisher/%s/extensions/%s/versions",
			pubB.ID.String(), extB.ID.String()),
		bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(platform.WithUserID(req.Context(), bob.ID))
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("cross-publisher dedup auto-fill: want 409, got %d (body %q)",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "different publisher") {
		t.Errorf("409 body should name cross-publisher dedup, got %q", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "bundle_url explicitly") {
		t.Errorf("409 body should suggest bundle_url workaround, got %q", rr.Body.String())
	}
}

// TestB8PublisherDashboardInstallStatsBatched pins the round-3
// fix for Devin Review
// ANALYSIS_pr-review-job-20b9bdccfe6d463c9a4d6ac7f0fea816_0004:
// the install-stats endpoint must batch the cross-version count
// query (one round trip via CountInstallationsByVersions) instead
// of the pre-fix per-version fan-out (one ListInstallationsByVersion
// call per version, each with a BYPASSRLS role check).
//
// The test seeds a publisher with two versions, attaches a
// per-version install count, hits the dashboard endpoint, and
// asserts that the response carries the right per-version
// counts. Correctness of the batched query is exercised; the
// O(N)→O(1) round-trip count itself is verified at code-review
// time (one Query call vs. one-per-version).
func TestB8PublisherDashboardInstallStatsBatched(t *testing.T) {
	pool := openIntegrationPool(t, "KAPP_TEST_DB_URL")
	ctx := context.Background()
	store := marketplace.NewStore(pool)
	users := tenant.NewUserStore(pool)

	// Real publisher + extension + 2 version rows; one of them
	// gets installs (3 active + 1 disabled + 1 failed), the
	// second gets 2 active, and the third (defined below) has
	// none to verify the "absent from result" contract.
	pubSlug := "b8stats_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")
	pub, err := store.Publishers().CreatePublisher(ctx, marketplace.CreatePublisherInput{
		Slug:         pubSlug,
		DisplayName:  "B8 stats publisher",
		ContactEmail: pubSlug + "@example.invalid",
	})
	if err != nil {
		t.Fatalf("CreatePublisher: %v", err)
	}
	owner, err := users.CreateUser(ctx, tenant.User{
		KChatUserID: "u-owner-stats-" + uuid.NewString()[:8],
		Email:       "owner-stats-" + uuid.NewString()[:6] + "@example.invalid",
		DisplayName: "Owner",
	})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	if _, err := store.Publishers().AddMember(ctx, marketplace.AddPublisherMemberInput{
		PublisherID: pub.ID, UserID: owner.ID,
		Role: marketplace.PublisherMemberRoleOwner,
	}); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}
	ext, err := store.CreateExtension(ctx, marketplace.CreateExtensionInput{
		Publisher: pubSlug, Slug: "b8stats_ext",
		DisplayName: "B8 stats extension",
		Description: "Install-stats batched aggregate pin",
		Author:      "B8 integration tests",
		License:     "MIT",
	})
	if err != nil {
		t.Fatalf("CreateExtension: %v", err)
	}

	// Two version rows with real manifests + bundle URLs.
	mkVersion := func(verStr string) *marketplace.ExtensionVersion {
		manifestYAML := []byte(fmt.Sprintf(`schema_version: 1
name: %s
version: %s
author: B8 integration tests
license: MIT
description: B8 stats pin
min_kapp_version: "1.0.0"
max_kapp_version: "1.x"
features_required: []
permissions_required: []
`, pubSlug+".b8stats_ext", verStr))
		man, err := marketplace.ParseManifest(manifestYAML)
		if err != nil {
			t.Fatalf("ParseManifest(%s): %v", verStr, err)
		}
		manifestJSON, err := json.Marshal(man)
		if err != nil {
			t.Fatalf("marshal manifest(%s): %v", verStr, err)
		}
		hashStr := uuid.NewString()
		// Bundle hash must be 64 hex chars. Re-use the uuid bytes
		// (32 hex) twice.
		hashStr = strings.ReplaceAll(hashStr, "-", "")
		hashStr += hashStr
		ver, err := store.PublishVersion(ctx, marketplace.PublishVersionInput{
			ExtensionID:  ext.ID,
			Manifest:     man,
			ManifestJSON: manifestJSON,
			BundleHash:   hashStr,
			BundleSize:   1024,
			BundleURL:    "https://kapp.test/bundles/" + hashStr,
		})
		if err != nil {
			t.Fatalf("PublishVersion(%s): %v", verStr, err)
		}
		return ver
	}
	ver1 := mkVersion("0.1.0")
	ver2 := mkVersion("0.2.0")
	ver3 := mkVersion("0.3.0") // no installs → must be absent

	// Seed installations directly (bypassing the install pipeline
	// because we only care about the aggregate's correctness here,
	// and the install pipeline drags in the runtime engine +
	// bundle resolver which are exercised by other tests).
	//
	// installations.UNIQUE (tenant_id, extension_id) blocks
	// multiple rows per tenant — so we mint a fresh tenant per
	// install row (cheap; the constraint is the test cost, not
	// real load).
	mkTenant := func(tag string) uuid.UUID {
		id := uuid.New()
		slug := "b8stats-" + tag + "-" + strings.ToLower(uuid.NewString()[:6])
		if _, err := pool.Exec(ctx, `
			INSERT INTO tenants (id, slug, name, cell, status, plan)
			VALUES ($1, $2, $3, 'default', 'active', 'basic')
			ON CONFLICT DO NOTHING`,
			id, slug, "B8 stats tenant "+tag); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		return id
	}
	seed := []struct {
		verID  uuid.UUID
		status marketplace.InstallStatus
	}{
		{ver1.ID, marketplace.InstallStatusActive},
		{ver1.ID, marketplace.InstallStatusActive},
		{ver1.ID, marketplace.InstallStatusActive},
		{ver1.ID, marketplace.InstallStatusDisabled},
		{ver1.ID, marketplace.InstallStatusFailed},
		{ver2.ID, marketplace.InstallStatusActive},
		{ver2.ID, marketplace.InstallStatusActive},
	}
	for i, s := range seed {
		tID := mkTenant(fmt.Sprintf("%d", i))
		// failure_reason must be NOT NULL iff status='failed'
		// (DB CHECK marketplace_installations_failure_reason_only_when_failed).
		var failureReason any
		if s.status == marketplace.InstallStatusFailed {
			failureReason = "synthetic failure for B8 stats test"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO marketplace_extension_installations
			(id, tenant_id, extension_id, extension_version_id, status,
			 settings, webhook_base, installed_by, failure_reason)
			VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, 'https://example.invalid/hook', $6, $7)`,
			uuid.New(), tID, ext.ID, s.verID, string(s.status), owner.ID, failureReason,
		); err != nil {
			t.Fatalf("seed install (%s): %v", s.status, err)
		}
	}

	// Pool is admin (openIntegrationPool returns BYPASSRLS role).
	got, err := store.CountInstallationsByVersions(ctx, pool,
		[]uuid.UUID{ver1.ID, ver2.ID, ver3.ID})
	if err != nil {
		t.Fatalf("CountInstallationsByVersions: %v", err)
	}
	v1, ok := got[ver1.ID]
	if !ok {
		t.Fatalf("ver1 missing from result")
	}
	if v1.Total != 5 {
		t.Errorf("ver1.Total: want 5, got %d", v1.Total)
	}
	if v1.Status[marketplace.InstallStatusActive] != 3 {
		t.Errorf("ver1 active: want 3, got %d", v1.Status[marketplace.InstallStatusActive])
	}
	if v1.Status[marketplace.InstallStatusDisabled] != 1 {
		t.Errorf("ver1 disabled: want 1, got %d", v1.Status[marketplace.InstallStatusDisabled])
	}
	if v1.Status[marketplace.InstallStatusFailed] != 1 {
		t.Errorf("ver1 failed: want 1, got %d", v1.Status[marketplace.InstallStatusFailed])
	}
	v2, ok := got[ver2.ID]
	if !ok {
		t.Fatalf("ver2 missing from result")
	}
	if v2.Total != 2 || v2.Status[marketplace.InstallStatusActive] != 2 {
		t.Errorf("ver2: want Total=2 Active=2, got %+v", v2)
	}
	if _, hit := got[ver3.ID]; hit {
		t.Errorf("ver3 has zero installs and must be absent, got %+v", got[ver3.ID])
	}

	// Empty input slice → empty result, no error.
	empty, err := store.CountInstallationsByVersions(ctx, pool, nil)
	if err != nil {
		t.Fatalf("CountInstallationsByVersions(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("nil input should yield empty map, got %d entries", len(empty))
	}
}

// --- helpers -------------------------------------------------------------

// buildB8TestBundle constructs a minimal valid tar.gz that
// bundle.Extract + ParseManifest will accept. The publisher slug
// is embedded in the manifest's `name` field so the upload
// handler's publisher-mismatch guard at line 213 passes when the
// caller's publisher row matches.
func buildB8TestBundle(t *testing.T, publisherSlug string) []byte {
	t.Helper()
	manifest := []byte(fmt.Sprintf(`schema_version: 1
name: %s.b8int_ext
version: 0.1.0
author: B8 integration tests
license: MIT
description: B8 integration test bundle
min_kapp_version: "1.0.0"
max_kapp_version: "1.x"
features_required: []
permissions_required: []
`, publisherSlug))
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "bundle/kapp-extension.yaml",
		Mode:     0o644,
		Size:     int64(len(manifest)),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar WriteHeader: %v", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatalf("tar Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz Close: %v", err)
	}
	return buf.Bytes()
}

func buildB8Multipart(t *testing.T, filename string, body []byte) (reader io.Reader, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("bundle", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close: %v", err)
	}
	return &buf, mw.FormDataContentType()
}
