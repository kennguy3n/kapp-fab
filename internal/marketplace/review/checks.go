package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/sign"
)

// BundleSizeCheck enforces the marketplace's hard size cap on the
// resolved bundle. The submit endpoint also enforces this (so a
// publisher gets a 413 at upload time), but the review pipeline
// re-enforces here so an admin-side ingest path that bypassed
// the API still produces a structured finding.
type BundleSizeCheck struct{}

// Name implements Check.
func (BundleSizeCheck) Name() string { return "bundle.size" }

// Run implements Check. Accumulates both findings (size + hash
// drift) when both fail; a bundle that is over-cap AND hashes
// differently is a clear "publisher swapped the file post-
// submit" signal and the publisher gets one re-submission
// covering both issues, not two round-trips.
func (BundleSizeCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	var findings []marketplace.ReviewFinding
	size := int64(len(b.RawBytes))
	if size > marketplace.MaxBundleSizeBytes {
		findings = append(findings, marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "bundle.size.exceeds_cap",
			Location: "",
			Message: fmt.Sprintf(
				"bundle is %d bytes; exceeds %d-byte cap",
				size, marketplace.MaxBundleSizeBytes),
		})
	}
	// Hash drift: the catalog row claims a hash; the bytes we
	// fetched compute to a different hash. Either the publisher
	// modified the bundle without re-publishing (most likely) or
	// the CDN is serving a different object than the one at
	// submission time. Either way the version is unreviewable
	// against its declared hash.
	if b.Version != nil && b.Version.BundleHash != "" && b.Version.BundleHash != b.Hash {
		findings = append(findings, marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "bundle.hash.mismatch",
			Location: "",
			Message: fmt.Sprintf(
				"bundle bytes hash to %s but catalog records %s",
				b.Hash, b.Version.BundleHash),
		})
	}
	return findings
}

// ManifestSchemaCheck re-validates the manifest with the strict
// parser. The submit endpoint already parses it through the same
// validator, so this check produces findings only when a manifest
// somehow landed in the catalog without going through ParseManifest
// (e.g. a future admin tool that wrote the manifest directly). In
// the normal submit path the check is a no-op — but the cost is
// near-zero and the defence-in-depth is worth it.
type ManifestSchemaCheck struct{}

// Name implements Check.
func (ManifestSchemaCheck) Name() string { return "manifest.schema" }

// Run implements Check.
func (ManifestSchemaCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	raw, ok := b.Files["kapp-extension.yaml"]
	if !ok {
		// LoadReviewBundle would have failed earlier; defence-
		// in-depth only.
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "manifest.missing",
			Location: "kapp-extension.yaml",
			Message:  "bundle root must contain kapp-extension.yaml",
		}}
	}
	if _, err := marketplace.ParseManifest(raw); err != nil {
		// We've already passed the manifest through ParseManifest
		// in LoadReviewBundle (which is why b.Manifest is non-nil),
		// so this branch is unreachable in practice. If it ever
		// fires, surface as an error finding with the parse output.
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "manifest.parse",
			Location: "kapp-extension.yaml",
			Message:  fmt.Sprintf("manifest parse failed: %v", err),
		}}
	}
	return nil
}

// PermissionScopeCheck cross-references manifest.permissions_required
// against the platform's closed list (ValidPermissionScopes) and
// surfaces high-risk scopes as warn-level findings even when they
// are well-formed.
//
// Two finding shapes:
//   - error severity (code=permission.unknown): the scope is not
//     in the platform's list. The manifest validator already
//     rejects these at upload time; this branch is defence-in-
//     depth for admin-side ingest paths.
//   - warn severity (code=permission.privileged): the scope is
//     in the platform's list but is one of the high-risk grants
//     (admin / delete) — flagged so a human reviewer can confirm
//     the extension genuinely needs the scope.
type PermissionScopeCheck struct{}

// privilegedPermissionScopes is the subset of ValidPermissionScopes
// that warrants a warn-severity finding. The list is intentionally
// conservative: `admin` and `delete` verbs both grant operations
// that a malicious extension could use to destroy tenant data.
var privilegedPermissionScopes = map[string]struct{}{
	"inventory.admin":    {},
	"sales.order.delete": {},
	"crm.contact.delete": {},
}

// Name implements Check.
func (PermissionScopeCheck) Name() string { return "permission.scope" }

// Run implements Check.
func (PermissionScopeCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	if b.Manifest == nil {
		return nil
	}
	var findings []marketplace.ReviewFinding
	for i, p := range b.Manifest.PermissionsRequired {
		if _, ok := marketplace.ValidPermissionScopes[p]; !ok {
			findings = append(findings, marketplace.ReviewFinding{
				Severity: marketplace.SeverityError,
				Code:     "permission.unknown",
				Location: fmt.Sprintf("permissions_required[%d]", i),
				Message:  fmt.Sprintf("scope %q is not in the platform's allowed list", p),
			})
			continue
		}
		if _, priv := privilegedPermissionScopes[p]; priv {
			findings = append(findings, marketplace.ReviewFinding{
				Severity: marketplace.SeverityWarn,
				Code:     "permission.privileged",
				Location: fmt.Sprintf("permissions_required[%d]", i),
				Message:  fmt.Sprintf("scope %q grants destructive operations; confirm the extension needs it", p),
			})
		}
	}
	return findings
}

// extKTypeNameRegex mirrors the regex enforced by both the
// marketplace.ParseManifest validator (extKTypeNameRegex in
// manifest.go) and the DB CHECK on marketplace_extension_ktypes
// (marketplace_ext_ktypes_name_chk in migrations/000069). The
// review check repeats it so a bundle that bypassed parser-side
// validation still gets a finding here.
var ktypeNameRegex = regexp.MustCompile(`^ext\.[a-z][a-z0-9_]{2,31}\.[a-z][a-z0-9_]*$`)

// KTypeNamespaceCheck enforces that every published KType lives
// under `ext.<publisher>.<slug>` AND that the `<publisher>` segment
// matches the publisher slug on the extension row. This second
// rule is novel here: a publisher cannot register KTypes under
// another publisher's namespace.
type KTypeNamespaceCheck struct{}

// Name implements Check.
func (KTypeNamespaceCheck) Name() string { return "ktype.namespace" }

// Run implements Check.
func (KTypeNamespaceCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	if b.Manifest == nil {
		return nil
	}
	// The manifest's `name` field is "publisher/extension"; the
	// publisher segment is the canonical name authority. Use the
	// ParseManifest-derived Publisher field (set after the strict
	// validator runs) rather than re-splitting `name` here so
	// the check sees the same publisher value the catalog row
	// will be tagged with.
	publisher := b.Manifest.Publisher

	var findings []marketplace.ReviewFinding
	for i, k := range b.Manifest.KTypes {
		// Per spec §3.1 the KType's canonical name lives inside
		// its schema JSON's `name` field (not the manifest path).
		// Pull it out of the bundle file the manifest points at.
		ktypeName, location, sub := resolveKTypeName(b, i, k.Schema)
		if sub != nil {
			findings = append(findings, *sub)
			continue
		}
		if !ktypeNameRegex.MatchString(ktypeName) {
			findings = append(findings, marketplace.ReviewFinding{
				Severity: marketplace.SeverityError,
				Code:     "ktype.namespace.format",
				Location: location,
				Message:  fmt.Sprintf("ktype %q must match ^ext\\.[a-z][a-z0-9_]{2,31}\\.[a-z][a-z0-9_]*$", ktypeName),
			})
			continue
		}
		// ext.<publisher>.<slug>
		segs := strings.Split(ktypeName, ".")
		if len(segs) < 3 {
			// Regex above guarantees 3 segments; defence-in-depth.
			continue
		}
		ktypePublisher := segs[1]
		if publisher != "" && ktypePublisher != publisher {
			findings = append(findings, marketplace.ReviewFinding{
				Severity: marketplace.SeverityError,
				Code:     "ktype.namespace.mismatch",
				Location: location,
				Message: fmt.Sprintf("ktype %q claims publisher %q but extension is published by %q",
					ktypeName, ktypePublisher, publisher),
			})
		}
	}
	return findings
}

// resolveKTypeName extracts the canonical KType name from the
// schema file the manifest's ktypes[i].schema path points at. The
// rules mirror bundle/resolver.go's resolveJSONFile so a manifest
// path that would be rejected at install time is also flagged here.
//
// Returns (name, location, finding). On success, finding is nil
// and name is the canonical "ext.<publisher>.<slug>" value. On
// failure, finding is a ready-to-emit error-severity finding and
// the caller should append it and skip to the next ktype.
func resolveKTypeName(b *Bundle, i int, schemaPath string) (name, location string, finding *marketplace.ReviewFinding) {
	location = fmt.Sprintf("ktypes[%d].schema", i)
	cleanPath, ok := iconRelPath(schemaPath) // same ./relative validation as the icon path
	if !ok {
		return "", location, &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "ktype.path.malformed",
			Location: location,
			Message:  fmt.Sprintf("ktypes[%d].schema %q must be a bundle-relative ./ path", i, schemaPath),
		}
	}
	body, ok := b.Files[cleanPath]
	if !ok {
		return "", location, &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "ktype.schema.missing",
			Location: location,
			Message:  fmt.Sprintf("ktypes[%d].schema %q is not present in the bundle", i, schemaPath),
		}
	}
	var named struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &named); err != nil {
		return "", location, &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "ktype.schema.invalid_json",
			Location: location,
			Message:  fmt.Sprintf("ktypes[%d].schema %q does not parse as JSON: %v", i, schemaPath, err),
		}
	}
	if named.Name == "" {
		return "", location, &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "ktype.schema.name_missing",
			Location: location,
			Message:  fmt.Sprintf("ktypes[%d].schema %q has no top-level \"name\" field", i, schemaPath),
		}
	}
	return named.Name, location, nil
}

// EndpointSchemeCheck re-runs the HTTPS-or-placeholder check against
// every endpoint URL in the manifest (agent tools, webhooks
// consumed, posting hooks). Mirrors validateEndpoint in manifest.go
// — the strict validator already runs this at parse time, so the
// pipeline produces a finding only when an admin-side ingest path
// bypassed parser validation. Each finding cites the exact slice
// index for precise developer remediation.
type EndpointSchemeCheck struct{}

// Name implements Check.
func (EndpointSchemeCheck) Name() string { return "endpoint.scheme" }

// Run implements Check.
func (EndpointSchemeCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	if b.Manifest == nil {
		return nil
	}
	var findings []marketplace.ReviewFinding
	for i, at := range b.Manifest.AgentTools {
		if f := checkEndpoint(at.Endpoint, fmt.Sprintf("agent_tools[%d].endpoint", i)); f != nil {
			findings = append(findings, *f)
		}
	}
	for i, w := range b.Manifest.WebhooksConsumed {
		if f := checkEndpoint(w.Endpoint, fmt.Sprintf("webhooks_consumed[%d].endpoint", i)); f != nil {
			findings = append(findings, *f)
		}
	}
	for i, ph := range b.Manifest.PostingHooks {
		if f := checkEndpoint(ph.Endpoint, fmt.Sprintf("posting_hooks[%d].endpoint", i)); f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

func checkEndpoint(endpoint, location string) *marketplace.ReviewFinding {
	// Placeholder-prefix is allowed.
	if strings.HasPrefix(endpoint, marketplace.PlaceholderWebhookBase) {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u == nil {
		return &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "endpoint.malformed",
			Location: location,
			Message:  fmt.Sprintf("endpoint %q is not a parseable URL", endpoint),
		}
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "endpoint.scheme.not_https",
			Location: location,
			Message:  fmt.Sprintf("endpoint %q uses scheme %q; only https is permitted", endpoint, u.Scheme),
		}
	}
	if u.Host == "" {
		return &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "endpoint.host.missing",
			Location: location,
			Message:  fmt.Sprintf("endpoint %q has no host", endpoint),
		}
	}
	if u.User != nil {
		return &marketplace.ReviewFinding{
			Severity: marketplace.SeverityError,
			Code:     "endpoint.userinfo.forbidden",
			Location: location,
			Message:  fmt.Sprintf("endpoint %q embeds userinfo (username/password); forbidden", endpoint),
		}
	}
	return nil
}

// IconCheck enforces the icon's presence, format, and size budget.
// The icon is optional in v1 (Manifest.Icon may be ""), but when
// declared MUST resolve to a file inside the archive, MUST be a
// PNG or SVG, and MUST be <= 64 KiB so catalog listings stay
// snappy.
type IconCheck struct{}

// MaxIconBytes is the per-icon size budget. Conservative — 64 KiB
// is plenty for a 256x256 PNG with reasonable compression.
const MaxIconBytes = 64 * 1024

// Name implements Check.
func (IconCheck) Name() string { return "icon" }

// Run implements Check.
func (IconCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	if b.Manifest == nil {
		return nil
	}
	if b.Manifest.Icon == "" {
		// Icon optional; absence is an info-level finding rather
		// than a warning so the publisher sees the suggestion but
		// it doesn't block.
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityInfo,
			Code:     "icon.absent",
			Location: "icon",
			Message:  "no icon declared; the catalog UI will fall back to the publisher's default icon",
		}}
	}
	rel, ok := iconRelPath(b.Manifest.Icon)
	if !ok {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "icon.path",
			Location: "icon",
			Message:  fmt.Sprintf("icon path %q must be a bundle-relative path starting with ./", b.Manifest.Icon),
		}}
	}
	body, ok := b.Files[rel]
	if !ok {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "icon.missing",
			Location: "icon",
			Message:  fmt.Sprintf("icon path %q is declared but the file is not present in the bundle", b.Manifest.Icon),
		}}
	}
	if len(body) == 0 {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "icon.empty",
			Location: "icon",
			Message:  fmt.Sprintf("icon path %q is empty", b.Manifest.Icon),
		}}
	}
	if len(body) > MaxIconBytes {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "icon.too_large",
			Location: "icon",
			Message:  fmt.Sprintf("icon is %d bytes; exceeds %d-byte cap", len(body), MaxIconBytes),
		}}
	}
	// Sniff the first bytes for a PNG signature or an SVG tag.
	if !looksLikePNG(body) && !looksLikeSVG(body) {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "icon.format",
			Location: "icon",
			Message:  "icon must be PNG (8-byte signature 89 50 4E 47 0D 0A 1A 0A) or SVG",
		}}
	}
	// Filename hint: extension should match content (case-
	// insensitive). A .png extension with SVG body is suspicious
	// (catalog UIs sometimes dispatch on extension, not sniff).
	ext := strings.ToLower(path.Ext(rel))
	if ext == ".png" && looksLikeSVG(body) {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityWarn,
			Code:     "icon.extension_mismatch",
			Location: "icon",
			Message:  "icon filename has .png extension but body is SVG; rename to .svg",
		}}
	}
	if ext == ".svg" && looksLikePNG(body) {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityWarn,
			Code:     "icon.extension_mismatch",
			Location: "icon",
			Message:  "icon filename has .svg extension but body is PNG; rename to .png",
		}}
	}
	return nil
}

func iconRelPath(p string) (string, bool) {
	if !strings.HasPrefix(p, "./") {
		return "", false
	}
	cleaned := path.Clean(p[2:])
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}

func looksLikePNG(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	sig := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	for i, x := range sig {
		if b[i] != x {
			return false
		}
	}
	return true
}

func looksLikeSVG(b []byte) bool {
	// SVG is XML — we accept either a leading XML declaration or
	// a direct <svg ...> tag. Tolerant of leading whitespace and
	// BOM. Case-insensitive on the tag itself.
	if len(b) == 0 {
		return false
	}
	// Strip UTF-8 BOM.
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	// Look at the first 256 bytes max — an SVG must declare its
	// root element early.
	head := b
	if len(head) > 256 {
		head = head[:256]
	}
	lower := strings.ToLower(string(head))
	return strings.Contains(lower, "<svg")
}

// UIStaticAnalysisCheck inspects UI extension files for patterns
// that would be unsafe in a Wasm-sandboxed runtime. The Wasm runtime
// itself enforces the sandbox at execution time, but the static
// check gives publishers an upload-time signal — and protects the
// platform from a UI bundle that would be silently no-op'd by the
// sandbox (e.g. a publisher who used `fetch()` and was confused
// why their data didn't load).
//
// Findings:
//   - warn (code=ui.unsafe_global.<slug>): the body references a
//     browser-only global (window, document, navigator, location,
//     XMLHttpRequest, fetch, eval, Function, importScripts, alert,
//     localStorage, sessionStorage, indexedDB) — the Wasm sandbox
//     does not expose these. The bundle will load but the
//     specific feature won't work. The <slug> in the code is the
//     lowercased global name (e.g. xmlhttprequest, localstorage)
//     so it satisfies the marketplace_review_findings_code_format
//     CHECK in migrations/000073 (lowercase identifiers only);
//     the un-cased name is included in the human-readable Message.
//   - info (code=ui.console_log): the body uses `console.log` —
//     not an issue in itself, but the sandbox writes console
//     output to the extension log, not the browser console,
//     so a publisher relying on the browser DevTools console
//     should know.
type UIStaticAnalysisCheck struct{}

// Name implements Check.
func (UIStaticAnalysisCheck) Name() string { return "ui.static" }

// uiUnsafeGlobal pairs the case-sensitive identifier the static
// scan greps for with the lowercase slug used inside the finding
// Code (which the marketplace_review_findings_code_format CHECK
// in migrations/000073 enforces as lowercase-only). Keeping the
// two as separate fields avoids losing case information in the
// Message that the publisher reads.
type uiUnsafeGlobal struct {
	Name string // case-sensitive identifier as it appears in JS/TS
	Slug string // lowercase identifier safe to embed in a finding code
}

// uiUnsafeGlobals is the closed list of browser globals the
// Wasm sandbox does NOT expose. The Wasm host imports a small
// `kapp.*` namespace and nothing else; references to anything in
// this list will silently no-op at runtime.
//
// `Function` is the global JS constructor `new Function("...")`
// which lets a publisher build and invoke a function from a
// string (effectively `eval`). The Wasm sandbox does not expose
// it; flagged alongside `eval` so a publisher who routed around
// a flagged `eval(...)` by switching to `new Function(...)` still
// gets the same warning.
var uiUnsafeGlobals = []uiUnsafeGlobal{
	{Name: "window", Slug: "window"},
	{Name: "document", Slug: "document"},
	{Name: "navigator", Slug: "navigator"},
	{Name: "location", Slug: "location"},
	{Name: "XMLHttpRequest", Slug: "xmlhttprequest"},
	{Name: "fetch", Slug: "fetch"},
	{Name: "eval", Slug: "eval"},
	{Name: "Function", Slug: "function"},
	{Name: "importScripts", Slug: "importscripts"},
	{Name: "alert", Slug: "alert"},
	{Name: "localStorage", Slug: "localstorage"},
	{Name: "sessionStorage", Slug: "sessionstorage"},
	{Name: "indexedDB", Slug: "indexeddb"},
}

// uiFileExtensions are the file extensions the static check
// inspects. JS / TS source (.js, .ts, .jsx, .tsx) and Wasm WAT
// text (.wat). Wasm binary (.wasm) is opaque and not text-grepped.
var uiFileExtensions = map[string]struct{}{
	".js":  {},
	".ts":  {},
	".jsx": {},
	".tsx": {},
	".wat": {},
}

// Run implements Check.
func (UIStaticAnalysisCheck) Run(_ context.Context, b *Bundle) []marketplace.ReviewFinding {
	if b.Manifest == nil {
		return nil
	}
	// Per-manifest UI extensions can declare their entry path;
	// the static check runs over every JS/TS/WAT file inside the
	// archive so a publisher who happens to ship a helper file
	// alongside their declared entry still gets flagged.
	var findings []marketplace.ReviewFinding
	// Sort keys for deterministic finding order across runs.
	keys := make([]string, 0, len(b.Files))
	for k := range b.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		ext := strings.ToLower(path.Ext(p))
		if _, ok := uiFileExtensions[ext]; !ok {
			continue
		}
		body := b.Files[p]
		// Cheap word-boundary scan — false positives on a string
		// literal containing the global name are acceptable for
		// a static check, and the publisher can suppress the
		// finding by adjusting their source.
		for _, g := range uiUnsafeGlobals {
			if containsIdent(body, g.Name) {
				findings = append(findings, marketplace.ReviewFinding{
					Severity: marketplace.SeverityWarn,
					Code:     "ui.unsafe_global." + g.Slug,
					Location: p,
					Message: fmt.Sprintf(
						"%s references browser global %q which the Wasm sandbox does not expose",
						p, g.Name),
				})
			}
		}
		if containsIdent(body, "console") {
			findings = append(findings, marketplace.ReviewFinding{
				Severity: marketplace.SeverityInfo,
				Code:     "ui.console",
				Location: p,
				Message: fmt.Sprintf(
					"%s uses console.* — output is routed to the extension log, not the browser DevTools console",
					p),
			})
		}
	}
	return findings
}

// containsIdent reports whether b contains the identifier ident
// surrounded by non-identifier characters. Not a real parser — a
// boundary heuristic that is good enough to catch the unsafe
// globals without false-positive-spamming on substrings like
// "myfetcher".
func containsIdent(b []byte, ident string) bool {
	s := string(b)
	idx := 0
	for {
		j := strings.Index(s[idx:], ident)
		if j < 0 {
			return false
		}
		start := idx + j
		end := start + len(ident)
		if (start == 0 || !isIdentChar(s[start-1])) &&
			(end == len(s) || !isIdentChar(s[end])) {
			return true
		}
		idx = end
	}
}

func isIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_' || c == '$'
}

// SignatureCheck enforces the publisher's signing policy. The
// policy is publisher-scoped (see B7 design note in package doc):
// if the publisher has registered any non-revoked ed25519 keys,
// every version MUST be signed and the signature MUST verify
// against ONE of the registered keys. If no keys are registered,
// unsigned uploads are accepted with an info-level finding to
// nudge the publisher toward signing.
type SignatureCheck struct{}

// Name implements Check.
func (SignatureCheck) Name() string { return "signature" }

// Run implements Check.
func (SignatureCheck) Run(ctx context.Context, b *Bundle) []marketplace.ReviewFinding {
	policy, ok := PolicyFromContext(ctx)
	if !ok || policy == nil {
		// No policy attached — we cannot know whether the publisher
		// has registered keys. Surface as a warn so a human reviewer
		// notices that the pipeline ran without policy context
		// (most likely a wiring bug in the worker).
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityWarn,
			Code:     "signature.policy.missing",
			Location: "",
			Message:  "signature policy not loaded; cannot evaluate publisher signing requirement",
		}}
	}
	sigPresent := b.Version != nil &&
		b.Version.BundleSignature != "" &&
		b.Version.BundleSignatureKeyID != ""

	if len(policy.NonRevokedKeys) == 0 {
		if sigPresent {
			// Publisher signed even though they haven't registered
			// a key — we can't verify the sig (we don't know which
			// key it claims). Warn so the publisher registers the
			// public half.
			return []marketplace.ReviewFinding{{
				Severity: marketplace.SeverityWarn,
				Code:     "signature.unregistered_key",
				Location: "bundle_signature_key_id",
				Message: fmt.Sprintf(
					"signature references key %q but publisher has no registered keys; register the public half before signing",
					b.Version.BundleSignatureKeyID),
			}}
		}
		// Unsigned + no keys: info-level nudge.
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityInfo,
			Code:     "signature.publisher_unsigned",
			Location: "",
			Message:  "publisher has not registered any signing keys; this version is unsigned (allowed in v1 but signature is recommended)",
		}}
	}

	// Publisher has at least one non-revoked key registered →
	// signature is required.
	if !sigPresent {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "signature.required",
			Location: "bundle_signature",
			Message:  "publisher has registered keys; every version must be signed but bundle_signature is empty",
		}}
	}
	var matchedKey *marketplace.PublisherKey
	for i := range policy.NonRevokedKeys {
		if policy.NonRevokedKeys[i].KeyID == b.Version.BundleSignatureKeyID {
			matchedKey = &policy.NonRevokedKeys[i]
			break
		}
	}
	if matchedKey == nil {
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     "signature.unknown_key",
			Location: "bundle_signature_key_id",
			Message: fmt.Sprintf(
				"signature references key %q which is not in publisher's registered (non-revoked) key set",
				b.Version.BundleSignatureKeyID),
		}}
	}
	if err := sign.Verify(b.RawBytes, b.Version.BundleSignature, matchedKey.PublicKeyB64); err != nil {
		code := "signature.verify_failed"
		if errors.Is(err, sign.ErrInvalidPublicKey) {
			code = "signature.invalid_key"
		} else if errors.Is(err, sign.ErrInvalidSignatureEncoding) {
			code = "signature.invalid_encoding"
		}
		return []marketplace.ReviewFinding{{
			Severity: marketplace.SeverityError,
			Code:     code,
			Location: "bundle_signature",
			Message:  fmt.Sprintf("signature verification failed: %v", err),
		}}
	}
	return nil
}


