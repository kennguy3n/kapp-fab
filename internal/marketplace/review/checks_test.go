package review_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/review"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/sign"
)

// --- Fixture builders -------------------------------------------------

type bundleBuilder struct {
	files map[string][]byte
}

func newBundle() *bundleBuilder {
	return &bundleBuilder{files: make(map[string][]byte)}
}

func (b *bundleBuilder) file(name string, body []byte) *bundleBuilder {
	b.files[name] = body
	return b
}

// build produces a .tar.gz body with files under the canonical
// single-root "extroot/" directory (matching what untarGzip
// expects). The returned bytes are runnable through
// review.LoadReviewBundle.
func (b *bundleBuilder) build(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Sort keys so the tar layout is deterministic across builds.
	keys := make([]string, 0, len(b.files))
	for k := range b.files {
		keys = append(keys, k)
	}
	// stable order — strings.Sort would be fine here too.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		body := b.files[k]
		hdr := &tar.Header{
			Name: "extroot/" + k,
			Mode: 0o644,
			Size: int64(len(body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tw.WriteHeader %q: %v", k, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("tw.Write %q: %v", k, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}
	return buf.Bytes()
}

// goodManifest returns a manifest YAML that exercises every
// section the checks inspect. The publisher is "acme", the slug
// is "shipping", one KType (ext.acme.box), one webhook endpoint,
// no agent tools / no posting hooks / no UI extensions (those are
// individually exercised in dedicated sub-tests).
func goodManifest() string {
	return strings.Join([]string{
		"schema_version: 1",
		"name: acme.shipping",
		"version: 1.0.0",
		"author: ACME Logistics",
		"license: Apache-2.0",
		"description: ACME shipping integration for kapp.",
		"min_kapp_version: 1.0.0",
		"features_required: [inventory, sales]",
		"permissions_required: [inventory.read, sales.order.read]",
		"ktypes:",
		"  - schema: ./ktypes/box.json",
		"webhooks_consumed:",
		"  - event: krecord.create",
		"    endpoint: https://acme.example/hooks/box",
		"",
	}, "\n")
}

func goodKTypeSchema() string {
	return `{
"name": "ext.acme.box",
"version": 1,
"properties": {"sku": {"type": "string"}}
}`
}

// loadVersion sets up a version row pointing at the in-memory
// source's bundle ID. Returns the bundle bytes (so a test can
// also hash them for the version row) and the version row.
func loadVersion(t *testing.T, bb *bundleBuilder, signed bool, priv ed25519.PrivateKey, keyID string) ([]byte, *marketplace.ExtensionVersion) {
	t.Helper()
	body := bb.build(t)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	v := &marketplace.ExtensionVersion{
		ID:          uuid.New(),
		ExtensionID: uuid.New(),
		Version:     "1.0.0",
		BundleHash:  hash,
		BundleURL:   "https://example/bundle.tar.gz",
	}
	if signed {
		sigB64, err := sign.Sign(body, priv)
		if err != nil {
			t.Fatalf("sign.Sign: %v", err)
		}
		v.BundleSignature = sigB64
		v.BundleSignatureKeyID = keyID
		now := time.Now().UTC()
		v.SignedAt = &now
	}
	return body, v
}

// --- BundleSizeCheck --------------------------------------------------

func TestBundleSizeCheck_HashMismatch(t *testing.T) {
	t.Parallel()
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	// Tamper the version's recorded hash so it no longer matches
	// the actual bytes.
	ver.BundleHash = strings.Repeat("a", 64)
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.BundleSizeCheck{}})
	if !hasCode(got, "bundle.hash.mismatch") {
		t.Fatalf("expected bundle.hash.mismatch, got %+v", got)
	}
}

func TestBundleSizeCheck_OK(t *testing.T) {
	t.Parallel()
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.BundleSizeCheck{}})
	if len(got) != 0 {
		t.Fatalf("expected zero findings, got %+v", got)
	}
}

// --- PermissionScopeCheck ---------------------------------------------

func TestPermissionScopeCheck_UnknownAndPrivileged(t *testing.T) {
	t.Parallel()
	m := strings.Replace(goodManifest(),
		"permissions_required: [inventory.read, sales.order.read]",
		"permissions_required: [inventory.read, sales.order.delete, inventory.admin]",
		1)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(m)).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.PermissionScopeCheck{}})
	if !hasCode(got, "permission.privileged") {
		t.Fatalf("expected permission.privileged finding for sales.order.delete and inventory.admin, got %+v", got)
	}
	// Count: two privileged scopes → two findings.
	count := 0
	for _, f := range got {
		if f.Code == "permission.privileged" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("expected 2 privileged findings, got %d (%+v)", count, got)
	}
}

// --- KTypeNamespaceCheck ----------------------------------------------

func TestKTypeNamespaceCheck_PublisherMismatch(t *testing.T) {
	t.Parallel()
	// The publisher segment in the KType name "ext.bobby.box"
	// does not match the manifest publisher "acme".
	schema := `{"name": "ext.bobby.box", "version": 1}`
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(schema))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.KTypeNamespaceCheck{}})
	if !hasCode(got, "ktype.namespace.mismatch") {
		t.Fatalf("expected ktype.namespace.mismatch, got %+v", got)
	}
}

func TestKTypeNamespaceCheck_OK(t *testing.T) {
	t.Parallel()
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.KTypeNamespaceCheck{}})
	if len(got) != 0 {
		t.Fatalf("expected zero findings, got %+v", got)
	}
}

// --- EndpointSchemeCheck ----------------------------------------------

func TestEndpointSchemeCheck_HTTPRejected(t *testing.T) {
	t.Parallel()
	// Surreptitiously bypass the validator by reading the
	// good manifest, mutating the endpoint to http://, and
	// feeding the parsed result directly into a hand-built
	// Bundle (skipping ParseManifest's reject). The
	// pipeline's role here is defence-in-depth.
	mani := &marketplace.Manifest{
		WebhooksConsumed: []marketplace.WebhookRef{{
			Event:    "krecord.create",
			Endpoint: "http://insecure.example/hooks/x",
		}},
		AgentTools: []marketplace.AgentToolRef{{
			Endpoint: "https://acme.example/tools/echo",
		}},
		PostingHooks: []marketplace.PostingHookRef{{
			Endpoint: "https://acme.example/hooks/post",
		}},
	}
	rb := &review.Bundle{
		Manifest: mani,
		Files:    map[string][]byte{},
	}
	got := review.EndpointSchemeCheck{}.Run(context.Background(), rb)
	if !hasCode(got, "endpoint.scheme.not_https") {
		t.Fatalf("expected endpoint.scheme.not_https, got %+v", got)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 finding, got %d (%+v)", len(got), got)
	}
	if got[0].Location != "webhooks_consumed[0].endpoint" {
		t.Fatalf("expected location webhooks_consumed[0].endpoint, got %q", got[0].Location)
	}
}

func TestEndpointSchemeCheck_PlaceholderAccepted(t *testing.T) {
	t.Parallel()
	mani := &marketplace.Manifest{
		AgentTools: []marketplace.AgentToolRef{{
			Endpoint: marketplace.PlaceholderWebhookBase + "/tools/echo",
		}},
	}
	rb := &review.Bundle{Manifest: mani, Files: map[string][]byte{}}
	got := review.EndpointSchemeCheck{}.Run(context.Background(), rb)
	if len(got) != 0 {
		t.Fatalf("expected zero findings for placeholder endpoint, got %+v", got)
	}
}

// --- IconCheck --------------------------------------------------------

func TestIconCheck_PNG(t *testing.T) {
	t.Parallel()
	pngBody := append([]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0}, 32)...)
	m := strings.Replace(goodManifest(),
		"description: ACME shipping integration for kapp.",
		"description: ACME shipping integration for kapp.\nicon: ./assets/logo.png",
		1)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(m)).
		file("ktypes/box.json", []byte(goodKTypeSchema())).
		file("assets/logo.png", pngBody)
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.IconCheck{}})
	if len(got) != 0 {
		t.Fatalf("expected zero findings for valid PNG, got %+v", got)
	}
}

func TestIconCheck_MissingFile(t *testing.T) {
	t.Parallel()
	m := strings.Replace(goodManifest(),
		"description: ACME shipping integration for kapp.",
		"description: ACME shipping integration for kapp.\nicon: ./assets/logo.png",
		1)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(m)).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.IconCheck{}})
	if !hasCode(got, "icon.missing") {
		t.Fatalf("expected icon.missing, got %+v", got)
	}
}

func TestIconCheck_ExtensionMismatch(t *testing.T) {
	t.Parallel()
	svgBody := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"></svg>`)
	// Name file as .png but body is SVG → warn-severity finding.
	m := strings.Replace(goodManifest(),
		"description: ACME shipping integration for kapp.",
		"description: ACME shipping integration for kapp.\nicon: ./assets/logo.png",
		1)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(m)).
		file("ktypes/box.json", []byte(goodKTypeSchema())).
		file("assets/logo.png", svgBody)
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.IconCheck{}})
	if !hasCode(got, "icon.extension_mismatch") {
		t.Fatalf("expected icon.extension_mismatch, got %+v", got)
	}
}

// --- UIStaticAnalysisCheck --------------------------------------------

func TestUIStaticAnalysisCheck_FlagsUnsafeGlobals(t *testing.T) {
	t.Parallel()
	js := []byte(`function init() { fetch('/api/x'); document.title = 'hello'; }`)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema())).
		file("ui/main.js", js)
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.UIStaticAnalysisCheck{}})
	if !hasCode(got, "ui.unsafe_global.fetch") {
		t.Fatalf("expected ui.unsafe_global.fetch, got %+v", got)
	}
	if !hasCode(got, "ui.unsafe_global.document") {
		t.Fatalf("expected ui.unsafe_global.document, got %+v", got)
	}
}

func TestUIStaticAnalysisCheck_IgnoresSubstrings(t *testing.T) {
	t.Parallel()
	// "myfetcher" contains "fetch" but is not the identifier; the
	// boundary heuristic must NOT fire.
	js := []byte(`const myfetcher = (url) => doRequest(url);`)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema())).
		file("ui/main.js", js)
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, nil, []review.Check{review.UIStaticAnalysisCheck{}})
	for _, f := range got {
		if strings.HasPrefix(f.Code, "ui.unsafe_global.") {
			t.Fatalf("expected no ui.unsafe_global findings on substring, got %+v", got)
		}
	}
}

// --- SignatureCheck ---------------------------------------------------

func TestSignatureCheck_UnsignedNoKeys_Info(t *testing.T) {
	t.Parallel()
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	got := runChecks(t, src, ver, emptyPolicyLoader(), []review.Check{review.SignatureCheck{}})
	if !hasCode(got, "signature.publisher_unsigned") {
		t.Fatalf("expected signature.publisher_unsigned, got %+v", got)
	}
	if got[0].Severity != marketplace.SeverityInfo {
		t.Fatalf("expected info severity, got %q", got[0].Severity)
	}
}

func TestSignatureCheck_RequiredButMissing(t *testing.T) {
	t.Parallel()
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	policy := review.PolicyLoaderFunc(func(_ context.Context, _ *marketplace.ExtensionVersion) (*review.PolicyContext, error) {
		return &review.PolicyContext{
			Publisher: &marketplace.Publisher{ID: uuid.New(), Slug: "acme"},
			NonRevokedKeys: []marketplace.PublisherKey{{
				KeyID:        "kid-1",
				PublicKeyB64: sign.EncodePublicKey(pub),
			}},
		}, nil
	})

	got := runChecks(t, src, ver, policy, []review.Check{review.SignatureCheck{}})
	if !hasCode(got, "signature.required") {
		t.Fatalf("expected signature.required, got %+v", got)
	}
}

func TestSignatureCheck_ValidSignature(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, true, priv, "kid-1")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	policy := review.PolicyLoaderFunc(func(_ context.Context, _ *marketplace.ExtensionVersion) (*review.PolicyContext, error) {
		return &review.PolicyContext{
			Publisher: &marketplace.Publisher{ID: uuid.New(), Slug: "acme"},
			NonRevokedKeys: []marketplace.PublisherKey{{
				KeyID:        "kid-1",
				PublicKeyB64: sign.EncodePublicKey(pub),
			}},
		}, nil
	})

	got := runChecks(t, src, ver, policy, []review.Check{review.SignatureCheck{}})
	if len(got) != 0 {
		t.Fatalf("expected zero findings for valid signature, got %+v", got)
	}
}

func TestSignatureCheck_TamperedBundle(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, true, priv, "kid-1")
	// Build a fresh bundle with different content but reuse
	// the (now-invalid) signature taken from `ver`.
	bb2 := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema())).
		file("README", []byte("EXTRA"))
	body2 := bb2.build(t)
	// The catalog still records the hash of body (matching the
	// signature), but the source returns body2 (a different
	// archive). BundleSizeCheck flags the hash mismatch; the
	// SignatureCheck flags the verify failure. Use only the
	// signature check here; the bundle is loaded fresh from
	// body2 so the verify computes against the tampered bytes.
	_ = body
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body2)

	policy := review.PolicyLoaderFunc(func(_ context.Context, _ *marketplace.ExtensionVersion) (*review.PolicyContext, error) {
		return &review.PolicyContext{
			Publisher: &marketplace.Publisher{ID: uuid.New(), Slug: "acme"},
			NonRevokedKeys: []marketplace.PublisherKey{{
				KeyID:        "kid-1",
				PublicKeyB64: sign.EncodePublicKey(pub),
			}},
		}, nil
	})

	got := runChecks(t, src, ver, policy, []review.Check{review.SignatureCheck{}})
	if !hasCode(got, "signature.verify_failed") {
		t.Fatalf("expected signature.verify_failed for tampered bundle, got %+v", got)
	}
}

func TestSignatureCheck_UnknownKey(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, true, priv, "kid-claimed-but-not-registered")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	policy := review.PolicyLoaderFunc(func(_ context.Context, _ *marketplace.ExtensionVersion) (*review.PolicyContext, error) {
		return &review.PolicyContext{
			Publisher: &marketplace.Publisher{ID: uuid.New(), Slug: "acme"},
			NonRevokedKeys: []marketplace.PublisherKey{{
				KeyID:        "kid-1",
				PublicKeyB64: sign.EncodePublicKey(otherPub),
			}},
		}, nil
	})

	got := runChecks(t, src, ver, policy, []review.Check{review.SignatureCheck{}})
	if !hasCode(got, "signature.unknown_key") {
		t.Fatalf("expected signature.unknown_key, got %+v", got)
	}
}

// --- Pipeline.Run state transition ------------------------------------

func TestPipeline_StateTransition_NoFindings_Approved(t *testing.T) {
	t.Parallel()
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	// Use only checks that won't fire findings on the good
	// bundle (avoid SignatureCheck so the publisher_unsigned
	// info finding doesn't appear here).
	p := &review.Pipeline{
		Source: src,
		Checks: []review.Check{review.BundleSizeCheck{}, review.PermissionScopeCheck{}, review.KTypeNamespaceCheck{}, review.EndpointSchemeCheck{}, review.UIStaticAnalysisCheck{}},
	}
	res, err := p.Run(context.Background(), ver)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != marketplace.ReviewStatusAutomatedPassed {
		t.Fatalf("expected automated_passed, got %q (findings %+v)", res.Status, res.Findings)
	}
}

func TestPipeline_StateTransition_ErrorRejects(t *testing.T) {
	t.Parallel()
	// Force a hash mismatch (error severity).
	bb := newBundle().
		file("kapp-extension.yaml", []byte(goodManifest())).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	ver.BundleHash = strings.Repeat("d", 64)
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	p := &review.Pipeline{Source: src, Checks: []review.Check{review.BundleSizeCheck{}}}
	res, err := p.Run(context.Background(), ver)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != marketplace.ReviewStatusRejected {
		t.Fatalf("expected rejected, got %q", res.Status)
	}
	if res.WorstSeverity != marketplace.SeverityError {
		t.Fatalf("expected error severity, got %q", res.WorstSeverity)
	}
}

func TestPipeline_StateTransition_WarnRequiresHumanReview(t *testing.T) {
	t.Parallel()
	// Use a manifest with a privileged scope to trigger the
	// warn-severity finding from PermissionScopeCheck.
	m := strings.Replace(goodManifest(),
		"permissions_required: [inventory.read, sales.order.read]",
		"permissions_required: [inventory.read, inventory.admin]",
		1)
	bb := newBundle().
		file("kapp-extension.yaml", []byte(m)).
		file("ktypes/box.json", []byte(goodKTypeSchema()))
	body, ver := loadVersion(t, bb, false, nil, "")
	src := review.NewMemorySource()
	src.Set(ver.ID.String(), body)

	p := &review.Pipeline{Source: src, Checks: []review.Check{review.PermissionScopeCheck{}}}
	res, err := p.Run(context.Background(), ver)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != marketplace.ReviewStatusManualReview {
		t.Fatalf("expected manual_review, got %q", res.Status)
	}
	if res.WorstSeverity != marketplace.SeverityWarn {
		t.Fatalf("expected warn severity, got %q", res.WorstSeverity)
	}
}

func TestPipeline_DefaultChecks_StableSet(t *testing.T) {
	t.Parallel()
	// Lock in the v1 default set so an accidental deletion (e.g.
	// SignatureCheck dropped from the chain) shows up as a failing
	// test rather than a silently bypassed security gate.
	want := []string{
		"bundle.size",
		"manifest.schema",
		"permission.scope",
		"ktype.namespace",
		"endpoint.scheme",
		"icon",
		"ui.static",
		"signature",
	}
	checks := review.DefaultChecks()
	if len(checks) != len(want) {
		t.Fatalf("default checks count drifted: got %d, want %d", len(checks), len(want))
	}
	for i, c := range checks {
		if c.Name() != want[i] {
			t.Fatalf("default checks[%d]: got %q, want %q", i, c.Name(), want[i])
		}
	}
}

// --- Helpers -----------------------------------------------------------

// runChecks builds a Pipeline with the given checks (no FindingSink
// / StateSink wired) and returns the findings the run produced.
func runChecks(t *testing.T, src *review.MemorySource, ver *marketplace.ExtensionVersion, policy review.PolicyLoader, checks []review.Check) []marketplace.ReviewFinding {
	t.Helper()
	p := &review.Pipeline{Source: src, Policy: policy, Checks: checks}
	res, err := p.Run(context.Background(), ver)
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	return res.Findings
}

func hasCode(findings []marketplace.ReviewFinding, code string) bool {
	for i := range findings {
		if findings[i].Code == code {
			return true
		}
	}
	return false
}

// emptyPolicyLoader returns a loader that always reports a publisher
// with no registered keys — used by the unsigned-no-keys signature
// check sub-test.
func emptyPolicyLoader() review.PolicyLoader {
	return review.PolicyLoaderFunc(func(_ context.Context, _ *marketplace.ExtensionVersion) (*review.PolicyContext, error) {
		return &review.PolicyContext{
			Publisher:      &marketplace.Publisher{ID: uuid.New(), Slug: "acme"},
			NonRevokedKeys: nil,
		}, nil
	})
}
