package marketplace

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MaxKTypesPerBundle, MaxWorkflowsPerBundle, MaxAgentToolsPerBundle,
// MaxUIExtensionsPerBundle, MaxWebhooksPerBundle pin the per-bundle
// count caps from EXTENSION_SPEC §2. The DB CHECK constraint on
// marketplace_extension_versions re-asserts the same values so the
// manifest validator and the persistence layer agree on the rejection
// boundary.
const (
	MaxKTypesPerBundle       = 32
	MaxWorkflowsPerBundle    = 16
	MaxAgentToolsPerBundle   = 32
	MaxUIExtensionsPerBundle = 16
	MaxWebhooksPerBundle     = 16
	MaxPostingHooksPerBundle = 16
	MaxSecretsPerBundle      = 16
	MaxFeaturesPerBundle     = 32
	MaxPermissionsPerBundle  = 64
)

// AgentToolTimeoutMax pins the upper bound on
// agent_tools[].timeout. Spec §3 ("default 10s, max 30s"). A handler
// that does not respond within this window has its invocation
// recorded as DeadlineExceeded; longer-running operations belong on
// an async webhook subscription path, not a synchronous tool call.
const (
	AgentToolTimeoutDefault = 10 * time.Second
	AgentToolTimeoutMax     = 30 * time.Second
)

// AgentToolMaxAttemptsCap is the upper bound on retry.max_attempts
// in an agent-tool definition. Spec defaults to 2 (initial + 1
// retry); we cap at 5 to keep tail latency bounded — anything beyond
// 5 attempts indicates a structural failure mode (auth issue, dead
// endpoint) where retrying further is harmful.
const AgentToolMaxAttemptsCap = 5

// ManifestSchemaVersion is the only schema_version this parser
// accepts in v1. Per spec §11 ("Compatibility Promise"), additions
// to the schema are backwards-compatible within a single
// schema_version; tightening the validator across schema_versions
// requires bumping this constant + adding a co-existence window in
// B6.
const ManifestSchemaVersion = 1

// PlaceholderWebhookBase is the only `${...}` placeholder that the
// manifest may reference (spec §3.1). The marketplace install dialog
// substitutes it with the tenant's webhook_base before persisting
// the version's manifest copy.
const PlaceholderWebhookBase = "${EXTENSION_WEBHOOK_BASE}"

// ValidUIExtensionSlots enumerates the slot names from spec §8.
// right_pane and record_list_action both bind to a specific KType,
// so the validator requires target_ktype for those two; dashboard
// widget + settings page are tenant-global and reject a stray
// target_ktype to avoid silently misleading the operator.
//
// RequiresLabel mirrors the same per-slot conditional shape. Only
// record_list_action treats `label` as load-bearing — it renders as
// the button text in the record-list action menu, and an extension
// shipping an empty label would surface a blank button to the
// operator ("What does this do?"). The other three slots derive their
// visible name from a different mechanism: right_pane uses the
// target_ktype's display name as the tab title, dashboard_widget is
// titled by the bundle's own internal header, and settings_page is
// labeled by the marketplace catalog header rather than the manifest.
// Requiring label for those three would create false-positive
// validation errors for manifests that follow the spec's example (see
// EXTENSION_SPEC §8 — only record_list_action carries a label in the
// reference YAML).
var ValidUIExtensionSlots = map[string]struct {
	RequiresTargetKType bool
	RequiresLabel       bool
}{
	"right_pane":         {RequiresTargetKType: true, RequiresLabel: false},
	"dashboard_widget":   {RequiresTargetKType: false, RequiresLabel: false},
	"record_list_action": {RequiresTargetKType: true, RequiresLabel: true},
	"settings_page":      {RequiresTargetKType: false, RequiresLabel: false},
}

// ValidPostingHookWhen enumerates the hook timings supported by B4
// (spec §3 posting_hooks). after_delete fires on the soft-delete
// transition; hard-delete is reserved for the platform GC and not
// surfaced to extensions.
var ValidPostingHookWhen = map[string]struct{}{
	"after_create": {},
	"after_update": {},
	"after_delete": {},
}

// ValidRetryBackoff enumerates the backoff strategies supported by
// the agent-tool dispatcher (B3). linear is min-jittered constant
// backoff; exponential is 2^n with full jitter capped at the timeout.
var ValidRetryBackoff = map[string]struct{}{
	"linear":      {},
	"exponential": {},
}

// ValidAgentToolHandlers enumerates the handler kinds supported in
// Phase 1. wasm is reserved for Phase 2 — present in the schema for
// forward compatibility, but the validator declines wasm handlers
// today (rejecting them gives publishers a clearer signal than
// silently accepting and then refusing at install time).
var ValidAgentToolHandlers = map[string]struct{}{
	"webhook": {},
}

// ValidPermissionScopes is the closed list of permissions the
// platform recognises in v1. The validator rejects unknown scopes
// with ErrPermissionScopeUnknown so a publisher's typo (e.g.
// `sales.order.writes` ← extra `s`) is caught at upload, not at
// install when the platform's role-grant check silently does nothing.
//
// Each scope is `<domain>.<resource>.<verb>` where verb ∈
// {read, write, delete, admin}. The list mirrors the platform's
// own resource taxonomy; adding a new scope requires bumping the
// schema_version (so this list participates in the compatibility
// promise from spec §11).
var ValidPermissionScopes = map[string]struct{}{
	"inventory.read":        {},
	"inventory.write":       {},
	"inventory.admin":       {},
	"sales.order.read":      {},
	"sales.order.write":     {},
	"sales.order.delete":    {},
	"finance.invoice.read":  {},
	"finance.invoice.write": {},
	"finance.payment.read":  {},
	"finance.payment.write": {},
	"crm.contact.read":      {},
	"crm.contact.write":     {},
	"crm.contact.delete":    {},
	"workflow.read":         {},
	"workflow.execute":      {},
	"events.subscribe":      {},
	"agent.tools.invoke":    {},
	"settings.read":         {},
	"settings.write":        {},
	"reports.read":          {},
	"audit.read":            {},
}

// ValidFeatures is the closed list of tenant-plan features an
// extension can require. Same rejection-on-unknown rule as
// ValidPermissionScopes — typos surface at upload, not at install.
var ValidFeatures = map[string]struct{}{
	"inventory":  {},
	"sales":      {},
	"finance":    {},
	"crm":        {},
	"workflow":   {},
	"agents":     {},
	"reports":    {},
	"events":     {},
	"webhooks":   {},
	"ui_extends": {},
	"multi_user": {},
	"multi_curr": {},
}

// SPDXLicenseAllowlist is the small allowed set of SPDX identifiers
// for the v1 schema. "Proprietary" is the escape hatch for closed
// extensions. The list is short because spec §11 says additions are
// backwards-compatible within schema_version 1 — we can extend this
// in a follow-up PR without breaking publishers.
var SPDXLicenseAllowlist = map[string]struct{}{
	"MIT":          {},
	"Apache-2.0":   {},
	"BSD-2-Clause": {},
	"BSD-3-Clause": {},
	"GPL-2.0":      {},
	"GPL-3.0":      {},
	"LGPL-2.1":     {},
	"LGPL-3.0":     {},
	"MPL-2.0":      {},
	"ISC":          {},
	"Unlicense":    {},
	"CC0-1.0":      {},
	"Proprietary":  {},
}

// Manifest is the typed view of kapp-extension.yaml. Each exported
// field maps to a manifest key; the parser returns this struct
// fully populated and validated on success. Callers MUST NOT mutate
// the struct after validation — the validator checks invariants
// (e.g. the publisher prefix of each KType reference matches the
// manifest's publisher) and the persistence layer relies on those
// invariants when computing the denormalised count columns.
//
// Each field carries BOTH yaml and json tags because the YAML form
// is the publisher-authored source of truth and the JSON form is
// what PublishVersion serialises to the manifest JSONB column (see
// store.go). The catalog UI then reads the JSON back through this
// same struct, so the two encodings MUST agree on key names — if
// the json tags were absent, encoding/json would emit Go-style
// PascalCase (`SchemaVersion`, `MinKappVersion`, ...) and a UI
// unmarshalling into a struct with the snake_case yaml-tagged
// fields would silently produce zero values.
type Manifest struct {
	SchemaVersion       int              `yaml:"schema_version"       json:"schema_version"`
	Name                string           `yaml:"name"                 json:"name"`
	Version             string           `yaml:"version"              json:"version"`
	Author              string           `yaml:"author"               json:"author"`
	License             string           `yaml:"license"              json:"license"`
	Description         string           `yaml:"description"          json:"description"`
	Homepage            string           `yaml:"homepage"             json:"homepage,omitempty"`
	SupportEmail        string           `yaml:"support_email"        json:"support_email,omitempty"`
	Icon                string           `yaml:"icon"                 json:"icon,omitempty"`
	MinKappVersion      string           `yaml:"min_kapp_version"     json:"min_kapp_version"`
	MaxKappVersion      string           `yaml:"max_kapp_version"     json:"max_kapp_version,omitempty"`
	FeaturesRequired    []string         `yaml:"features_required"    json:"features_required"`
	PermissionsRequired []string         `yaml:"permissions_required" json:"permissions_required"`
	KTypes              []KTypeRef       `yaml:"ktypes"               json:"ktypes,omitempty"`
	Workflows           []WorkflowRef    `yaml:"workflows"            json:"workflows,omitempty"`
	AgentTools          []AgentToolRef   `yaml:"agent_tools"          json:"agent_tools,omitempty"`
	WebhooksConsumed    []WebhookRef     `yaml:"webhooks_consumed"    json:"webhooks_consumed,omitempty"`
	PostingHooks        []PostingHookRef `yaml:"posting_hooks"        json:"posting_hooks,omitempty"`
	UIExtensions        []UIExtensionRef `yaml:"ui_extensions"        json:"ui_extensions,omitempty"`
	SettingsSchema      string           `yaml:"settings_schema"      json:"settings_schema,omitempty"`
	SecretsRequired     []SecretRef      `yaml:"secrets_required"     json:"secrets_required,omitempty"`

	// Derived fields populated by ParseManifest after validation —
	// not present in the YAML source. Marked yaml:"-" so a hand-
	// crafted manifest containing them is rejected as a "field not
	// recognised" issue rather than silently overriding parser
	// computation. They ARE emitted in the JSON form because the
	// catalog UI needs (publisher, slug) to render extension cards
	// without re-parsing `name` on every read.
	Publisher string `yaml:"-" json:"publisher"`
	Slug      string `yaml:"-" json:"slug"`
}

// KTypeRef is one element of manifest.ktypes[]. spec §3 / §4.
type KTypeRef struct {
	Schema string `yaml:"schema" json:"schema"`
}

// WorkflowRef is one element of manifest.workflows[].
type WorkflowRef struct {
	Definition string `yaml:"definition" json:"definition"`
}

// AgentToolRef is one element of manifest.agent_tools[]. spec §6.
type AgentToolRef struct {
	Definition string     `yaml:"definition" json:"definition"`
	Handler    string     `yaml:"handler"    json:"handler"`
	Endpoint   string     `yaml:"endpoint"   json:"endpoint"`
	Timeout    string     `yaml:"timeout"    json:"timeout"`
	Retry      *RetryRule `yaml:"retry"      json:"retry,omitempty"`
}

// RetryRule is the retry block on agent_tools[] / posting_hooks[].
type RetryRule struct {
	MaxAttempts int    `yaml:"max_attempts" json:"max_attempts"`
	Backoff     string `yaml:"backoff"      json:"backoff"`
}

// WebhookRef is one element of manifest.webhooks_consumed[]. spec §7.
type WebhookRef struct {
	Event    string            `yaml:"event"    json:"event"`
	Filter   map[string]string `yaml:"filter"   json:"filter,omitempty"`
	Endpoint string            `yaml:"endpoint" json:"endpoint"`
}

// PostingHookRef is one element of manifest.posting_hooks[]. B4.
type PostingHookRef struct {
	KType    string `yaml:"ktype"    json:"ktype"`
	When     string `yaml:"when"     json:"when"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`
}

// UIExtensionRef is one element of manifest.ui_extensions[]. spec §8.
type UIExtensionRef struct {
	Slot         string `yaml:"slot"          json:"slot"`
	TargetKType  string `yaml:"target_ktype"  json:"target_ktype"`
	Label        string `yaml:"label"         json:"label"`
	ComponentURL string `yaml:"component_url" json:"component_url"`
}

// SecretRef is one element of manifest.secrets_required[]. spec §9.
type SecretRef struct {
	Key         string `yaml:"key"         json:"key"`
	Label       string `yaml:"label"       json:"label"`
	Description string `yaml:"description" json:"description,omitempty"`
	Sensitive   bool   `yaml:"sensitive"   json:"sensitive"`
}

// ManifestError carries the per-field detail for a manifest rejection.
// The B6 API translates this into a 400 response with the per-field
// detail rendered into the response body so the publisher's UI (or
// the kapp-ext CLI) can highlight the offending field.
type ManifestError struct {
	Field   string
	Message string
}

func (e *ManifestError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", ErrInvalidManifest, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", ErrInvalidManifest, e.Field, e.Message)
}

// Unwrap exposes ErrInvalidManifest so callers can errors.Is for
// the sentinel without losing the per-field detail.
func (e *ManifestError) Unwrap() error { return ErrInvalidManifest }

// ManifestErrors aggregates multiple ManifestError values. The
// validator collects every problem in a single pass rather than
// short-circuiting on the first failure so publishers see all
// issues at once instead of fixing one at a time.
type ManifestErrors struct {
	Errors []*ManifestError
}

func (e *ManifestErrors) Error() string {
	if len(e.Errors) == 0 {
		return ErrInvalidManifest.Error()
	}
	parts := make([]string, 0, len(e.Errors))
	for _, err := range e.Errors {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

// Unwrap routes errors.Is(err, ErrInvalidManifest) and
// errors.Is(err, ErrPermissionScopeUnknown) through the aggregated
// set so callers don't have to walk the slice themselves.
func (e *ManifestErrors) Unwrap() []error {
	out := make([]error, 0, len(e.Errors)+1)
	out = append(out, ErrInvalidManifest)
	for _, me := range e.Errors {
		out = append(out, me)
	}
	return out
}

var (
	// Compile once at init — the manifest validator is on the hot
	// upload path and recompiling regexes per call would dominate
	// validator latency for small manifests.
	// nameRegex / publisherSlugRegex MUST match the DB CHECK
	// constraints in migrations/000068_marketplace.sql:99-103
	// exactly (`^[a-z][a-z0-9_]*$`). The earlier draft of these
	// regexes also allowed hyphens, which made the Go validator
	// strictly looser than the DB: a manifest like `my-pub.my-ext`
	// passed ParseManifest, then CreateExtension surfaced an opaque
	// CHECK constraint violation at INSERT time. Keep these two
	// boundaries in lock-step. extKTypeNameRegex below has the same
	// constraint for the same reason (it references the publisher
	// namespace).
	nameRegex            = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}\.[a-z][a-z0-9_]{2,31}$`)
	publisherSlugRegex   = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
	semverRegex          = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.\-]+)?(\+[0-9A-Za-z.\-]+)?$`)
	kappVersionRangeRe   = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.\-]+)?(\+[0-9A-Za-z.\-]+)?|[0-9]+\.x|[0-9]+\.[0-9]+\.x)$`)
	bundleRelPathRegex   = regexp.MustCompile(`^\./[A-Za-z0-9._\-/]+$`)
	endpointPlaceholder  = regexp.MustCompile(`^\$\{EXTENSION_WEBHOOK_BASE\}(/[A-Za-z0-9._\-/#?=&%]*)?$`)
	componentURLRegex    = regexp.MustCompile(`^\./[A-Za-z0-9._\-/]+(\.js|\.mjs)(#[A-Za-z0-9._\-]+)?$`)
	secretKeyRegex       = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	extKTypeNameRegex    = regexp.MustCompile(`^ext\.[a-z][a-z0-9_]{2,31}\.[a-z][a-z0-9_]*$`)
	eventNameRegex       = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){1,3}$`)
	platformKTypeRegex   = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	unknownPlaceholderRe = regexp.MustCompile(`\$\{[^}]+\}`)
)

// ParseManifest parses raw kapp-extension.yaml bytes into a typed
// Manifest with every spec §2/§3/§4/§6/§7/§8/§9 invariant checked.
// Returns *ManifestErrors (wrapping ErrInvalidManifest) with the full
// list of issues on rejection, or the validated Manifest on success.
//
// The parser is strict — yaml.Decoder.KnownFields(true) is enabled so
// a typo in a field name (e.g. `permissons_required`) surfaces as a
// validation error instead of silently being dropped on the floor.
//
// The validator is purely structural — it does NOT walk the bundle's
// actual files (that is B6 / the upload pipeline). It checks that
// every referenced relative path is in the bundle-relative form
// (./foo/bar) so the extractor knows where to look; whether the file
// exists is verified at extract time.
func ParseManifest(data []byte) (*Manifest, error) {
	if int64(len(data)) > MaxManifestSizeBytes {
		return nil, &ManifestError{Field: "", Message: fmt.Sprintf("manifest size %d bytes exceeds %d byte limit", len(data), MaxManifestSizeBytes)}
	}
	if len(data) == 0 {
		return nil, &ManifestError{Field: "", Message: "manifest is empty"}
	}

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		// Both yaml.TypeError (mismatched node kind) and other
		// parser-level failures (syntax error, unknown field with
		// KnownFields(true)) get wrapped identically — callers only
		// need ErrInvalidManifest + the field-less message. An
		// earlier draft had a `errors.Is(err, &yaml.TypeError{})`
		// branch but yaml.TypeError is a value type and the &v{}
		// target can never match by identity, so the branch was
		// always dead; removed to make the actual single-path
		// behaviour obvious.
		return nil, &ManifestError{Field: "", Message: fmt.Sprintf("yaml decode: %v", err)}
	}
	// Second decode call to confirm there is no extra document — a
	// manifest with two YAML documents in a single file is rejected
	// because the loader API picks the first only and silently
	// ignores the rest.
	var extra interface{}
	if err := dec.Decode(&extra); err == nil {
		return nil, &ManifestError{Field: "", Message: "manifest contains multiple YAML documents; only one is permitted"}
	}

	agg := &ManifestErrors{}
	validateManifest(&m, agg)
	if len(agg.Errors) > 0 {
		return nil, agg
	}
	return &m, nil
}

func validateManifest(m *Manifest, agg *ManifestErrors) {
	// --- schema_version ---
	if m.SchemaVersion != ManifestSchemaVersion {
		agg.add("schema_version", fmt.Sprintf("must be %d (got %d)", ManifestSchemaVersion, m.SchemaVersion))
	}

	// --- identity ---
	if m.Name == "" {
		agg.add("name", "required")
	} else if !nameRegex.MatchString(m.Name) {
		agg.add("name", "must match ^[a-z][a-z0-9_]{2,31}\\.[a-z][a-z0-9_]{2,31}$")
	} else {
		parts := strings.SplitN(m.Name, ".", 2)
		m.Publisher = parts[0]
		m.Slug = parts[1]
		if !publisherSlugRegex.MatchString(m.Publisher) {
			agg.add("name", "publisher segment is malformed")
		}
		if !publisherSlugRegex.MatchString(m.Slug) {
			agg.add("name", "slug segment is malformed")
		}
	}

	if m.Version == "" {
		agg.add("version", "required")
	} else if !semverRegex.MatchString(m.Version) {
		agg.add("version", "must be SemVer 2.0.0")
	}

	if strings.TrimSpace(m.Author) == "" {
		agg.add("author", "required")
	}

	if m.License == "" {
		agg.add("license", "required (SPDX identifier or \"Proprietary\")")
	} else if _, ok := SPDXLicenseAllowlist[m.License]; !ok {
		agg.add("license", fmt.Sprintf("%q is not a recognised SPDX identifier in the v1 allowlist", m.License))
	}

	if strings.TrimSpace(m.Description) == "" {
		agg.add("description", "required")
	} else if len(m.Description) > 4096 {
		// 4 KiB description cap — prevents a manifest abusing the
		// description field to push close to MaxManifestSizeBytes.
		agg.add("description", fmt.Sprintf("description %d chars exceeds 4096-char limit", len(m.Description)))
	}

	if m.Homepage != "" {
		if err := validateHTTPSURL(m.Homepage); err != nil {
			agg.add("homepage", err.Error())
		}
	}

	if m.SupportEmail != "" {
		if _, err := mail.ParseAddress(m.SupportEmail); err != nil {
			agg.add("support_email", fmt.Sprintf("invalid RFC 5322 address: %v", err))
		}
	}

	if m.Icon != "" && !bundleRelPathRegex.MatchString(m.Icon) {
		agg.add("icon", "must be a bundle-relative path of the form ./assets/icon.png")
	}

	// --- platform compatibility ---
	if m.MinKappVersion == "" {
		agg.add("min_kapp_version", "required")
	} else if !semverRegex.MatchString(m.MinKappVersion) {
		agg.add("min_kapp_version", "must be SemVer 2.0.0")
	}
	if m.MaxKappVersion != "" && !kappVersionRangeRe.MatchString(m.MaxKappVersion) {
		agg.add("max_kapp_version", "must be SemVer 2.0.0 or a major/minor wildcard (e.g. 1.x, 1.2.x)")
	}

	// --- features / permissions ---
	if len(m.FeaturesRequired) > MaxFeaturesPerBundle {
		agg.add("features_required", fmt.Sprintf("count %d exceeds %d limit", len(m.FeaturesRequired), MaxFeaturesPerBundle))
	}
	for i, f := range m.FeaturesRequired {
		if _, ok := ValidFeatures[f]; !ok {
			agg.add(fmt.Sprintf("features_required[%d]", i), fmt.Sprintf("feature %q not recognised", f))
		}
	}
	if hasDuplicateStrings(m.FeaturesRequired) {
		agg.add("features_required", "duplicate entries detected")
	}

	if len(m.PermissionsRequired) > MaxPermissionsPerBundle {
		agg.add("permissions_required", fmt.Sprintf("count %d exceeds %d limit", len(m.PermissionsRequired), MaxPermissionsPerBundle))
	}
	for i, p := range m.PermissionsRequired {
		if _, ok := ValidPermissionScopes[p]; !ok {
			agg.addRaw(&ManifestError{Field: fmt.Sprintf("permissions_required[%d]", i), Message: fmt.Sprintf("scope %q not recognised", p)})
		}
	}
	if hasDuplicateStrings(m.PermissionsRequired) {
		agg.add("permissions_required", "duplicate entries detected")
	}

	// --- KTypes ---
	if len(m.KTypes) > MaxKTypesPerBundle {
		agg.add("ktypes", fmt.Sprintf("count %d exceeds %d limit", len(m.KTypes), MaxKTypesPerBundle))
	}
	seenKTypePaths := make(map[string]struct{}, len(m.KTypes))
	for i, kt := range m.KTypes {
		if kt.Schema == "" {
			agg.add(fmt.Sprintf("ktypes[%d].schema", i), "required")
			continue
		}
		if !bundleRelPathRegex.MatchString(kt.Schema) {
			agg.add(fmt.Sprintf("ktypes[%d].schema", i), "must be a bundle-relative path of the form ./ktypes/foo.json")
		}
		if _, dup := seenKTypePaths[kt.Schema]; dup {
			agg.add(fmt.Sprintf("ktypes[%d].schema", i), fmt.Sprintf("duplicate path %q (already referenced earlier)", kt.Schema))
		}
		seenKTypePaths[kt.Schema] = struct{}{}
	}

	// --- Workflows ---
	if len(m.Workflows) > MaxWorkflowsPerBundle {
		agg.add("workflows", fmt.Sprintf("count %d exceeds %d limit", len(m.Workflows), MaxWorkflowsPerBundle))
	}
	seenWfPaths := make(map[string]struct{}, len(m.Workflows))
	for i, wf := range m.Workflows {
		if wf.Definition == "" {
			agg.add(fmt.Sprintf("workflows[%d].definition", i), "required")
			continue
		}
		if !bundleRelPathRegex.MatchString(wf.Definition) {
			agg.add(fmt.Sprintf("workflows[%d].definition", i), "must be a bundle-relative path of the form ./workflows/foo.json")
		}
		if _, dup := seenWfPaths[wf.Definition]; dup {
			agg.add(fmt.Sprintf("workflows[%d].definition", i), fmt.Sprintf("duplicate path %q", wf.Definition))
		}
		seenWfPaths[wf.Definition] = struct{}{}
	}

	// --- Agent tools ---
	if len(m.AgentTools) > MaxAgentToolsPerBundle {
		agg.add("agent_tools", fmt.Sprintf("count %d exceeds %d limit", len(m.AgentTools), MaxAgentToolsPerBundle))
	}
	for i, at := range m.AgentTools {
		field := fmt.Sprintf("agent_tools[%d]", i)
		if at.Definition == "" {
			agg.add(field+".definition", "required")
		} else if !bundleRelPathRegex.MatchString(at.Definition) {
			agg.add(field+".definition", "must be a bundle-relative path of the form ./tools/foo.json")
		}
		if at.Handler == "" {
			agg.add(field+".handler", "required (one of: webhook)")
		} else if _, ok := ValidAgentToolHandlers[at.Handler]; !ok {
			agg.add(field+".handler", fmt.Sprintf("handler %q not supported in v1 (only `webhook` is enabled; `wasm` is reserved for Phase 2)", at.Handler))
		}
		if err := validateEndpoint(at.Endpoint); err != nil {
			agg.add(field+".endpoint", err.Error())
		}
		if at.Timeout == "" {
			// Persist the spec §6 default ("timeout default 10s,
			// max 30s") to BOTH the loop copy (so the validation
			// below uses the defaulted value) AND the underlying
			// slice element (so the returned *Manifest carries the
			// default through json.Marshal in PublishVersion, and
			// the manifest JSONB the catalog UI reads back has the
			// explicit "10s" rather than an empty timeout that the
			// runtime engine would have to re-default at dispatch).
			m.AgentTools[i].Timeout = "10s"
			at.Timeout = "10s"
		}
		if d, err := time.ParseDuration(at.Timeout); err != nil {
			agg.add(field+".timeout", fmt.Sprintf("invalid duration %q: %v", at.Timeout, err))
		} else if d <= 0 {
			agg.add(field+".timeout", "must be positive")
		} else if d > AgentToolTimeoutMax {
			agg.add(field+".timeout", fmt.Sprintf("%s exceeds %s ceiling", d, AgentToolTimeoutMax))
		}
		if at.Retry != nil {
			if at.Retry.MaxAttempts < 1 {
				agg.add(field+".retry.max_attempts", "must be >= 1")
			} else if at.Retry.MaxAttempts > AgentToolMaxAttemptsCap {
				agg.add(field+".retry.max_attempts", fmt.Sprintf("%d exceeds %d ceiling", at.Retry.MaxAttempts, AgentToolMaxAttemptsCap))
			}
			if at.Retry.Backoff == "" {
				// at.Retry is a *RetryRule so this write reaches the
				// shared struct through the pointer — m.AgentTools[i]
				// .Retry.Backoff sees the same value. The .Timeout
				// default above needs the explicit slice-index write
				// because AgentToolRef itself is a value type.
				at.Retry.Backoff = "exponential"
			} else if _, ok := ValidRetryBackoff[at.Retry.Backoff]; !ok {
				agg.add(field+".retry.backoff", fmt.Sprintf("backoff %q not recognised (linear or exponential)", at.Retry.Backoff))
			}
		}
	}

	// --- Webhook subscriptions ---
	if len(m.WebhooksConsumed) > MaxWebhooksPerBundle {
		agg.add("webhooks_consumed", fmt.Sprintf("count %d exceeds %d limit", len(m.WebhooksConsumed), MaxWebhooksPerBundle))
	}
	for i, w := range m.WebhooksConsumed {
		field := fmt.Sprintf("webhooks_consumed[%d]", i)
		if w.Event == "" {
			agg.add(field+".event", "required (e.g. sales.order.status_changed)")
		} else if !eventNameRegex.MatchString(w.Event) {
			agg.add(field+".event", fmt.Sprintf("event %q must match <domain>.<entity>[.<verb>[.<qualifier>]] lower-snake form", w.Event))
		}
		if err := validateEndpoint(w.Endpoint); err != nil {
			agg.add(field+".endpoint", err.Error())
		}
		// filter is optional; if present, every value must be
		// non-empty (an empty value is almost always a YAML typo
		// like `status: ` which parses as null and would silently
		// match every payload). We coerce filter keys to a
		// deterministic order so the persisted manifest hash is
		// stable across re-uploads of the same source manifest.
		if len(w.Filter) > 0 {
			for _, k := range sortedMapKeys(w.Filter) {
				v := w.Filter[k]
				if strings.TrimSpace(v) == "" {
					agg.add(fmt.Sprintf("%s.filter.%s", field, k), "filter values must be non-empty")
				}
			}
		}
	}

	// --- Posting hooks ---
	if len(m.PostingHooks) > MaxPostingHooksPerBundle {
		agg.add("posting_hooks", fmt.Sprintf("count %d exceeds %d limit", len(m.PostingHooks), MaxPostingHooksPerBundle))
	}
	for i, ph := range m.PostingHooks {
		field := fmt.Sprintf("posting_hooks[%d]", i)
		if ph.KType == "" {
			agg.add(field+".ktype", "required")
		} else if !platformKTypeRegex.MatchString(ph.KType) && !extKTypeNameRegex.MatchString(ph.KType) {
			agg.add(field+".ktype", fmt.Sprintf("ktype %q must be of the form <domain>.<entity> or ext.<publisher>.<entity>", ph.KType))
		}
		if _, ok := ValidPostingHookWhen[ph.When]; !ok {
			agg.add(field+".when", fmt.Sprintf("must be one of after_create, after_update, after_delete (got %q)", ph.When))
		}
		if err := validateEndpoint(ph.Endpoint); err != nil {
			agg.add(field+".endpoint", err.Error())
		}
	}

	// --- UI extensions ---
	if len(m.UIExtensions) > MaxUIExtensionsPerBundle {
		agg.add("ui_extensions", fmt.Sprintf("count %d exceeds %d limit", len(m.UIExtensions), MaxUIExtensionsPerBundle))
	}
	for i, ui := range m.UIExtensions {
		field := fmt.Sprintf("ui_extensions[%d]", i)
		slotDef, ok := ValidUIExtensionSlots[ui.Slot]
		if !ok {
			agg.add(field+".slot", fmt.Sprintf("slot %q not recognised (must be one of right_pane, dashboard_widget, record_list_action, settings_page)", ui.Slot))
		} else {
			if slotDef.RequiresTargetKType && ui.TargetKType == "" {
				agg.add(field+".target_ktype", fmt.Sprintf("required for slot %q", ui.Slot))
			}
			if !slotDef.RequiresTargetKType && ui.TargetKType != "" {
				agg.add(field+".target_ktype", fmt.Sprintf("must be omitted for slot %q (tenant-global slot)", ui.Slot))
			}
			if slotDef.RequiresLabel && strings.TrimSpace(ui.Label) == "" {
				agg.add(field+".label", fmt.Sprintf("required for slot %q (renders as the action button text)", ui.Slot))
			}
		}
		if ui.TargetKType != "" {
			if !platformKTypeRegex.MatchString(ui.TargetKType) && !extKTypeNameRegex.MatchString(ui.TargetKType) {
				agg.add(field+".target_ktype", "must be of the form <domain>.<entity> or ext.<publisher>.<entity>")
			}
		}
		if ui.ComponentURL == "" {
			agg.add(field+".component_url", "required")
		} else if !componentURLRegex.MatchString(ui.ComponentURL) {
			agg.add(field+".component_url", "must be a bundle-relative .js or .mjs path (e.g. ./ui/foo.js, optionally with #anchor)")
		}
	}

	// --- Settings schema ---
	if m.SettingsSchema != "" && !bundleRelPathRegex.MatchString(m.SettingsSchema) {
		agg.add("settings_schema", "must be a bundle-relative path of the form ./settings.json")
	}

	// --- Secrets ---
	if len(m.SecretsRequired) > MaxSecretsPerBundle {
		agg.add("secrets_required", fmt.Sprintf("count %d exceeds %d limit", len(m.SecretsRequired), MaxSecretsPerBundle))
	}
	seenSecretKeys := make(map[string]struct{}, len(m.SecretsRequired))
	for i, s := range m.SecretsRequired {
		field := fmt.Sprintf("secrets_required[%d]", i)
		if s.Key == "" {
			agg.add(field+".key", "required")
		} else if !secretKeyRegex.MatchString(s.Key) {
			agg.add(field+".key", "must match ^[A-Z][A-Z0-9_]{1,63}$ (upper-snake, 2-64 chars)")
		}
		if strings.TrimSpace(s.Label) == "" {
			agg.add(field+".label", "required")
		}
		if _, dup := seenSecretKeys[s.Key]; dup {
			agg.add(field+".key", fmt.Sprintf("duplicate key %q", s.Key))
		}
		seenSecretKeys[s.Key] = struct{}{}
	}

	// --- Cross-field invariants ---

	// (1) Every KType file path must live under ./ktypes/ — the
	//     extractor uses the path as the schema name fallback, so
	//     keeping the layout consistent simplifies B6.
	for i, kt := range m.KTypes {
		if kt.Schema != "" && !strings.HasPrefix(kt.Schema, "./ktypes/") {
			agg.add(fmt.Sprintf("ktypes[%d].schema", i), "must live under ./ktypes/")
		}
	}
	for i, wf := range m.Workflows {
		if wf.Definition != "" && !strings.HasPrefix(wf.Definition, "./workflows/") {
			agg.add(fmt.Sprintf("workflows[%d].definition", i), "must live under ./workflows/")
		}
	}
	for i, at := range m.AgentTools {
		if at.Definition != "" && !strings.HasPrefix(at.Definition, "./tools/") {
			agg.add(fmt.Sprintf("agent_tools[%d].definition", i), "must live under ./tools/")
		}
	}
	for i, ui := range m.UIExtensions {
		if ui.ComponentURL != "" && !strings.HasPrefix(ui.ComponentURL, "./ui/") {
			agg.add(fmt.Sprintf("ui_extensions[%d].component_url", i), "must live under ./ui/")
		}
	}
}

func (e *ManifestErrors) add(field, msg string) {
	e.Errors = append(e.Errors, &ManifestError{Field: field, Message: msg})
}

func (e *ManifestErrors) addRaw(err *ManifestError) {
	e.Errors = append(e.Errors, err)
}

// validateEndpoint enforces that the endpoint is either the literal
// `${EXTENSION_WEBHOOK_BASE}` placeholder with an optional /-rooted
// path component, or a fully-qualified HTTPS URL. Plain HTTP, other
// schemes, and unknown placeholders are rejected. This is the
// load-bearing check that keeps an extension from pointing webhook
// dispatch at attacker-controlled domains or downgrading transport
// to plaintext.
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("required")
	}
	if endpointPlaceholder.MatchString(endpoint) {
		return nil
	}
	// Any other `${...}` token is rejected — spec §3.1 says
	// EXTENSION_WEBHOOK_BASE is the only allowed placeholder.
	if locs := unknownPlaceholderRe.FindAllString(endpoint, -1); len(locs) > 0 {
		for _, p := range locs {
			if p != PlaceholderWebhookBase {
				return fmt.Errorf("only ${EXTENSION_WEBHOOK_BASE} placeholder is supported (got %s)", p)
			}
		}
	}
	return validateHTTPSURL(endpoint)
}

// validateHTTPSURL parses url and rejects anything that isn't a
// fully-qualified https:// URL with a non-empty host. Mirrors the
// DB CHECK on marketplace_extension_installations.webhook_base
// (^https://) so the validator and the storage layer agree on the
// rejection boundary.
func validateHTTPSURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("scheme %q rejected; https required", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("host is required")
	}
	if u.User != nil {
		return errors.New("URL must not contain userinfo (username/password segment)")
	}
	return nil
}

// hasDuplicateStrings reports true if any element of v appears more
// than once. The validator uses this to flag duplicate
// features_required / permissions_required entries — a duplicate is
// almost always a copy-paste error and the platform's role-grant
// check treats duplicates as a single grant, so the silent dedup
// would hide intent.
func hasDuplicateStrings(v []string) bool {
	if len(v) < 2 {
		return false
	}
	seen := make(map[string]struct{}, len(v))
	for _, s := range v {
		if _, ok := seen[s]; ok {
			return true
		}
		seen[s] = struct{}{}
	}
	return false
}

// sortedMapKeys returns the keys of m in lexicographic order. Used
// to give the validator a deterministic iteration order so error
// messages and the persisted manifest hash are stable across runs.
func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidateKTypeName checks that an extension-authored KType name is
// in the `ext.<publisher>.<slug>` form AND that <publisher> matches
// the manifest's own publisher segment. Called by B6's bundle
// extractor for every ktypes/*.json the validator finds inside the
// archive — keeps a publisher named `acme` from shipping a KType
// named `ext.evil.shipping_label` (spec §4 rejection 1).
func ValidateKTypeName(ktypeName, manifestPublisher string) error {
	if !extKTypeNameRegex.MatchString(ktypeName) {
		return &ManifestError{
			Field:   "ktype.name",
			Message: fmt.Sprintf("KType %q must match ^ext\\.<publisher>\\.<slug>$", ktypeName),
		}
	}
	// Second segment is the publisher.
	parts := strings.SplitN(ktypeName, ".", 3)
	if len(parts) < 3 || parts[1] != manifestPublisher {
		return &ManifestError{
			Field:   "ktype.name",
			Message: fmt.Sprintf("KType publisher segment %q does not match manifest publisher %q", parts[1], manifestPublisher),
		}
	}
	return nil
}
