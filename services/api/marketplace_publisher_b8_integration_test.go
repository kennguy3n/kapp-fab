//go:build integration
// +build integration

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
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
