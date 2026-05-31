package marketplace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// validManifest returns the canonical happy-path manifest used as the
// baseline for the negative-path tests. Each test below mutates one
// field and asserts the validator rejects it with the expected
// message — keeping the baseline valid keeps the negative tests
// hermetic from each other.
func validManifest() string {
	return `schema_version: 1
name: acme.shipping
version: 1.0.0
author: ACME Corp
license: MIT
description: Shipping label generator
homepage: https://acme.example/shipping
support_email: support@acme.example
icon: ./assets/icon.png
min_kapp_version: 1.0.0
max_kapp_version: 2.x
features_required:
  - inventory
  - sales
permissions_required:
  - sales.order.read
  - sales.order.write
ktypes:
  - schema: ./ktypes/shipping_label.json
workflows:
  - definition: ./workflows/print.json
agent_tools:
  - definition: ./tools/print_label.json
    handler: webhook
    endpoint: ${EXTENSION_WEBHOOK_BASE}/print
    timeout: 5s
    retry:
      max_attempts: 2
      backoff: exponential
webhooks_consumed:
  - event: sales.order.created
    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_created
posting_hooks:
  - ktype: sales.order
    when: after_create
    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_hook
ui_extensions:
  - slot: right_pane
    target_ktype: sales.order
    component_url: ./ui/order_pane.js
    label: Shipping Labels
settings_schema: ./settings.json
secrets_required:
  - key: CARRIER_API_KEY
    label: Carrier API Key
    description: API key for the shipping carrier
    sensitive: true
`
}

func TestParseManifestHappyPath(t *testing.T) {
	m, err := ParseManifest([]byte(validManifest()))
	if err != nil {
		t.Fatalf("happy-path parse failed: %v", err)
	}
	if m.Publisher != "acme" || m.Slug != "shipping" {
		t.Errorf("publisher/slug split wrong: got %q.%q", m.Publisher, m.Slug)
	}
	if m.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d want 1", m.SchemaVersion)
	}
	if len(m.KTypes) != 1 || m.KTypes[0].Schema != "./ktypes/shipping_label.json" {
		t.Errorf("ktypes wrong: %+v", m.KTypes)
	}
	if len(m.AgentTools) != 1 || m.AgentTools[0].Timeout != "5s" {
		t.Errorf("agent_tools wrong: %+v", m.AgentTools)
	}
	if len(m.UIExtensions) != 1 || m.UIExtensions[0].Slot != "right_pane" {
		t.Errorf("ui_extensions wrong: %+v", m.UIExtensions)
	}
}

// rejectsOn applies the mutator to validManifest's text and verifies
// the validator returns a ManifestError whose Field/Message both
// match the expectations. Keeps the table-driven tests concise.
func rejectsOn(t *testing.T, mutate func(string) string, expectField, expectFragment string) {
	t.Helper()
	src := mutate(validManifest())
	_, err := ParseManifest([]byte(src))
	if err == nil {
		t.Fatal("expected rejection, got nil")
	}
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("expected ErrInvalidManifest sentinel, got %v", err)
	}
	var merrs *MultiManifestError
	if errors.As(err, &merrs) {
		matched := false
		var fields, msgs []string
		for _, me := range merrs.Errors {
			fields = append(fields, me.Field)
			msgs = append(msgs, me.Message)
			if (expectField == "" || me.Field == expectField) && strings.Contains(me.Message, expectFragment) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("no matching error found.\n  want field=%q fragment=%q\n  got   fields=%v\n        msgs=%v",
				expectField, expectFragment, fields, msgs)
		}
		return
	}
	var single *ManifestError
	if errors.As(err, &single) {
		if (expectField == "" || single.Field == expectField) && strings.Contains(single.Message, expectFragment) {
			return
		}
		t.Errorf("single ManifestError mismatch: field=%q msg=%q want field=%q fragment=%q",
			single.Field, single.Message, expectField, expectFragment)
		return
	}
	t.Errorf("expected *ManifestError(s), got %T: %v", err, err)
}

func TestParseManifestRejections(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(string) string
		field    string
		fragment string
	}{
		{
			name:     "schema_version 2 rejected",
			mutate:   replaceLine("schema_version: 1", "schema_version: 2"),
			field:    "schema_version",
			fragment: "must be 1",
		},
		{
			name:     "name without publisher dot rejected",
			mutate:   replaceLine("name: acme.shipping", "name: justshipping"),
			field:    "name",
			fragment: "must match",
		},
		{
			name:     "name with capital letter rejected",
			mutate:   replaceLine("name: acme.shipping", "name: Acme.shipping"),
			field:    "name",
			fragment: "must match",
		},
		{
			name:     "version non-semver rejected",
			mutate:   replaceLine("version: 1.0.0", "version: 1.0"),
			field:    "version",
			fragment: "SemVer",
		},
		{
			name:     "missing author rejected",
			mutate:   replaceLine("author: ACME Corp", "author: \"\""),
			field:    "author",
			fragment: "required",
		},
		{
			name:     "unknown license rejected",
			mutate:   replaceLine("license: MIT", "license: HocusPocus-1.0"),
			field:    "license",
			fragment: "not a recognised SPDX",
		},
		{
			name:     "homepage http:// rejected",
			mutate:   replaceLine("homepage: https://acme.example/shipping", "homepage: http://acme.example/shipping"),
			field:    "homepage",
			fragment: "https required",
		},
		{
			name:     "homepage with userinfo rejected",
			mutate:   replaceLine("homepage: https://acme.example/shipping", "homepage: https://user:pass@acme.example/shipping"),
			field:    "homepage",
			fragment: "userinfo",
		},
		{
			name:     "support_email malformed rejected",
			mutate:   replaceLine("support_email: support@acme.example", "support_email: not-an-email"),
			field:    "support_email",
			fragment: "invalid RFC 5322",
		},
		{
			name:     "icon outside bundle path rejected",
			mutate:   replaceLine("icon: ./assets/icon.png", "icon: /etc/passwd"),
			field:    "icon",
			fragment: "bundle-relative",
		},
		{
			name:     "min_kapp_version non-semver rejected",
			mutate:   replaceLine("min_kapp_version: 1.0.0", "min_kapp_version: 1.0"),
			field:    "min_kapp_version",
			fragment: "SemVer",
		},
		{
			name:     "max_kapp_version garbage rejected",
			mutate:   replaceLine("max_kapp_version: 2.x", "max_kapp_version: lemons"),
			field:    "max_kapp_version",
			fragment: "SemVer 2.0.0 or a major/minor wildcard",
		},
		{
			name:     "unknown feature rejected",
			mutate:   replaceLine("  - inventory", "  - inventory_legacy"),
			field:    "features_required[0]",
			fragment: "not recognised",
		},
		{
			name:     "duplicate feature rejected",
			mutate:   replaceLine("  - sales", "  - inventory"),
			field:    "features_required",
			fragment: "duplicate",
		},
		{
			name:     "unknown permission rejected",
			mutate:   replaceLine("  - sales.order.write", "  - sales.order.writes"),
			field:    "permissions_required[1]",
			fragment: "not recognised",
		},
		{
			name:     "ktype path outside ./ktypes rejected",
			mutate:   replaceLine("  - schema: ./ktypes/shipping_label.json", "  - schema: ./schemas/shipping_label.json"),
			field:    "ktypes[0].schema",
			fragment: "./ktypes/",
		},
		{
			name:     "workflow path outside ./workflows rejected",
			mutate:   replaceLine("  - definition: ./workflows/print.json", "  - definition: ./flows/print.json"),
			field:    "workflows[0].definition",
			fragment: "./workflows/",
		},
		{
			name:     "agent_tool wasm handler rejected",
			mutate:   replaceLine("    handler: webhook", "    handler: wasm"),
			field:    "agent_tools[0].handler",
			fragment: "not supported in v1",
		},
		{
			name:     "agent_tool plaintext endpoint rejected",
			mutate:   replaceLine("    endpoint: ${EXTENSION_WEBHOOK_BASE}/print", "    endpoint: http://insecure.example/print"),
			field:    "agent_tools[0].endpoint",
			fragment: "https required",
		},
		{
			name:     "agent_tool unknown placeholder rejected",
			mutate:   replaceLine("    endpoint: ${EXTENSION_WEBHOOK_BASE}/print", "    endpoint: ${ATTACKER_BASE}/print"),
			field:    "agent_tools[0].endpoint",
			fragment: "EXTENSION_WEBHOOK_BASE",
		},
		{
			name:     "agent_tool timeout > 30s rejected",
			mutate:   replaceLine("    timeout: 5s", "    timeout: 60s"),
			field:    "agent_tools[0].timeout",
			fragment: "ceiling",
		},
		{
			name:     "agent_tool max_attempts > cap rejected",
			mutate:   replaceLine("      max_attempts: 2", "      max_attempts: 99"),
			field:    "agent_tools[0].retry.max_attempts",
			fragment: "ceiling",
		},
		{
			name:     "webhook plaintext endpoint rejected",
			mutate:   replaceLine("    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_created", "    endpoint: http://insecure.example/order"),
			field:    "webhooks_consumed[0].endpoint",
			fragment: "https required",
		},
		{
			name:     "posting_hook bad when rejected",
			mutate:   replaceLine("    when: after_create", "    when: before_create"),
			field:    "posting_hooks[0].when",
			fragment: "must be one of",
		},
		{
			name:     "ui_extension dashboard_widget with target_ktype rejected",
			mutate:   replaceLine("  - slot: right_pane", "  - slot: dashboard_widget"),
			field:    "ui_extensions[0].target_ktype",
			fragment: "must be omitted",
		},
		{
			name:     "ui_extension component_url outside ./ui rejected",
			mutate:   replaceLine("    component_url: ./ui/order_pane.js", "    component_url: ./components/order_pane.js"),
			field:    "ui_extensions[0].component_url",
			fragment: "./ui/",
		},
		{
			name:     "ui_extension non-.js component rejected",
			mutate:   replaceLine("    component_url: ./ui/order_pane.js", "    component_url: ./ui/order_pane.html"),
			field:    "ui_extensions[0].component_url",
			fragment: ".js or .mjs",
		},
		{
			// record_list_action renders as a button in the
			// record-list action menu — the label is the button's
			// visible text, so a publisher shipping an empty label
			// would produce a blank button. Per ValidUIExtensionSlots
			// RequiresLabel=true for this slot only; the other three
			// slots derive their name from a different mechanism and
			// MUST NOT require label (right_pane uses the
			// target_ktype's display name as the tab title,
			// dashboard_widget owns its own bundle-internal header,
			// and settings_page is labeled by the marketplace catalog
			// header rather than the manifest).
			name: "ui_extension record_list_action without label rejected",
			mutate: func(in string) string {
				return strings.Replace(in,
					"  - slot: right_pane\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n    label: Shipping Labels\n",
					"  - slot: record_list_action\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n",
					1)
			},
			field:    "ui_extensions[0].label",
			fragment: "required for slot",
		},
		{
			// Whitespace-only label is rejected via the
			// strings.TrimSpace check, defending against a publisher
			// putting "   " in the label field to bypass the
			// non-empty validator.
			name: "ui_extension record_list_action whitespace-only label rejected",
			mutate: func(in string) string {
				return strings.Replace(in,
					"  - slot: right_pane\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n    label: Shipping Labels\n",
					"  - slot: record_list_action\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n    label: \"   \"\n",
					1)
			},
			field:    "ui_extensions[0].label",
			fragment: "required for slot",
		},
		{
			name:     "secret key lowercase rejected",
			mutate:   replaceLine("  - key: CARRIER_API_KEY", "  - key: carrier_api_key"),
			field:    "secrets_required[0].key",
			fragment: "upper-snake",
		},
		{
			name:     "settings_schema absolute path rejected",
			mutate:   replaceLine("settings_schema: ./settings.json", "settings_schema: /etc/passwd"),
			field:    "settings_schema",
			fragment: "bundle-relative",
		},
		{
			name:     "missing schema_version rejected",
			mutate:   removeLine("schema_version: 1"),
			field:    "schema_version",
			fragment: "must be 1",
		},
		{
			// Two agent_tools entries with the same definition path
			// would resolve to the same tool name at extract time
			// and crash on the install-time UNIQUE(tenant_id,
			// installation_id, tool_name) constraint. Reject at
			// upload time with a clearer error message.
			name: "duplicate agent_tools definition path rejected",
			mutate: func(in string) string {
				return strings.Replace(in,
					"agent_tools:\n  - definition: ./tools/print_label.json\n    handler: webhook\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/print\n    timeout: 5s\n    retry:\n      max_attempts: 2\n      backoff: exponential\n",
					"agent_tools:\n  - definition: ./tools/print_label.json\n    handler: webhook\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/print\n    timeout: 5s\n  - definition: ./tools/print_label.json\n    handler: webhook\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/print2\n    timeout: 5s\n",
					1)
			},
			field:    "agent_tools[1].definition",
			fragment: "duplicate path",
		},
		{
			// Two webhooks_consumed entries with identical (event,
			// endpoint) tuple would deliver the same payload to the
			// same URL twice. Duplicate tuples must be rejected;
			// different endpoints for the same event are legitimate
			// (extension wants both POSTs).
			name: "duplicate webhooks_consumed (event,endpoint) rejected",
			mutate: func(in string) string {
				return strings.Replace(in,
					"webhooks_consumed:\n  - event: sales.order.created\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_created\n",
					"webhooks_consumed:\n  - event: sales.order.created\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_created\n  - event: sales.order.created\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_created\n",
					1)
			},
			field:    "webhooks_consumed[1]",
			fragment: "duplicate",
		},
		{
			// Two posting_hooks entries with identical (ktype, when,
			// endpoint) triple would fire the same callback twice
			// per record event. Reject the literal triple-duplicate.
			name: "duplicate posting_hooks triple rejected",
			mutate: func(in string) string {
				return strings.Replace(in,
					"posting_hooks:\n  - ktype: sales.order\n    when: after_create\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_hook\n",
					"posting_hooks:\n  - ktype: sales.order\n    when: after_create\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_hook\n  - ktype: sales.order\n    when: after_create\n    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_hook\n",
					1)
			},
			field:    "posting_hooks[1]",
			fragment: "duplicate",
		},
		{
			// Two ui_extensions entries with identical
			// (slot, target_ktype, label, component_url) tuple would
			// render two identical buttons firing the same component
			// — unambiguously a manifest bug.
			name: "duplicate ui_extensions tuple rejected",
			mutate: func(in string) string {
				return strings.Replace(in,
					"ui_extensions:\n  - slot: right_pane\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n    label: Shipping Labels\n",
					"ui_extensions:\n  - slot: right_pane\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n    label: Shipping Labels\n  - slot: right_pane\n    target_ktype: sales.order\n    component_url: ./ui/order_pane.js\n    label: Shipping Labels\n",
					1)
			},
			field:    "ui_extensions[1]",
			fragment: "duplicate",
		},
		{
			// description error message must say "bytes" — the cap
			// is len()-based (byte count), not rune-based; saying
			// "chars" would be misleading for multi-byte UTF-8
			// descriptions where byte count > visible char count.
			name: "oversize description rejected with byte-count message",
			mutate: func(in string) string {
				huge := strings.Repeat("a", 4097)
				return strings.Replace(in,
					"description: Shipping label generator",
					"description: "+huge,
					1)
			},
			field:    "description",
			fragment: "bytes exceeds 4096-byte",
		},
		{
			// An HTTPS URL that embeds the EXTENSION_WEBHOOK_BASE
			// placeholder somewhere other than the prefix is a
			// manifest bug — after substitution it would yield a
			// malformed URL like `https://publisher.com/
			// https://tenant.example/hooks/...`. Reject at upload
			// time with the explicit "prefix not embedded" error.
			name: "agent_tool endpoint with embedded placeholder rejected",
			mutate: replaceLine("    endpoint: ${EXTENSION_WEBHOOK_BASE}/print",
				"    endpoint: https://publisher.example/${EXTENSION_WEBHOOK_BASE}/print"),
			field:    "agent_tools[0].endpoint",
			fragment: "must be the URL prefix",
		},
		{
			// Same shape on a webhook subscription — the dispatcher
			// would post the literal placeholder text or, after
			// substitution, a doubly-prefixed broken URL.
			name: "webhook endpoint with embedded placeholder rejected",
			mutate: replaceLine("    endpoint: ${EXTENSION_WEBHOOK_BASE}/order_created",
				"    endpoint: https://publisher.example/${EXTENSION_WEBHOOK_BASE}/order_created"),
			field:    "webhooks_consumed[0].endpoint",
			fragment: "must be the URL prefix",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rejectsOn(t, tc.mutate, tc.field, tc.fragment)
		})
	}
}

func TestParseManifestRejectsUnknownField(t *testing.T) {
	src := validManifest() + "unknown_field: 42\n"
	_, err := ParseManifest([]byte(src))
	if err == nil {
		t.Fatal("expected rejection for unknown field")
	}
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("expected ErrInvalidManifest, got %v", err)
	}
	if !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("error should name the unknown field; got %v", err)
	}
}

func TestParseManifestRejectsOversizedManifest(t *testing.T) {
	// Pad the description out to push us past the 64 KiB cap.
	pad := strings.Repeat("a", int(MaxManifestSizeBytes)+1)
	src := []byte(pad)
	_, err := ParseManifest(src)
	if err == nil {
		t.Fatal("expected size rejection")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestParseManifestRejectsEmpty(t *testing.T) {
	_, err := ParseManifest(nil)
	if err == nil {
		t.Fatal("expected rejection for empty input")
	}
}

func TestParseManifestRejectsTwoYAMLDocuments(t *testing.T) {
	src := validManifest() + "---\nschema_version: 1\n"
	_, err := ParseManifest([]byte(src))
	if err == nil {
		t.Fatal("expected rejection for multi-document yaml")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("expected multi-doc error, got %v", err)
	}
}

func TestParseManifestCountLimits(t *testing.T) {
	cases := []struct {
		name     string
		field    string
		listKey  string
		render   func(int) string
		over     int
		fragment string
	}{
		{
			name:    "ktypes over cap",
			field:   "ktypes",
			listKey: "ktypes:",
			render: func(i int) string {
				return fmt.Sprintf("  - schema: ./ktypes/k%d.json", i)
			},
			over:     MaxKTypesPerBundle + 1,
			fragment: fmt.Sprintf("exceeds %d limit", MaxKTypesPerBundle),
		},
		{
			name:    "workflows over cap",
			field:   "workflows",
			listKey: "workflows:",
			render: func(i int) string {
				return fmt.Sprintf("  - definition: ./workflows/w%d.json", i)
			},
			over:     MaxWorkflowsPerBundle + 1,
			fragment: fmt.Sprintf("exceeds %d limit", MaxWorkflowsPerBundle),
		},
		{
			name:    "webhooks over cap",
			field:   "webhooks_consumed",
			listKey: "webhooks_consumed:",
			render: func(i int) string {
				return fmt.Sprintf("  - event: sales.order.evt_%d\n    endpoint: https://example.com/h%d", i, i)
			},
			over:     MaxWebhooksPerBundle + 1,
			fragment: fmt.Sprintf("exceeds %d limit", MaxWebhooksPerBundle),
		},
		{
			name:    "ui_extensions over cap",
			field:   "ui_extensions",
			listKey: "ui_extensions:",
			render: func(i int) string {
				return fmt.Sprintf("  - slot: dashboard_widget\n    component_url: ./ui/w%d.js", i)
			},
			over:     MaxUIExtensionsPerBundle + 1,
			fragment: fmt.Sprintf("exceeds %d limit", MaxUIExtensionsPerBundle),
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			buf.WriteString(tc.listKey)
			buf.WriteString("\n")
			for i := 0; i < tc.over; i++ {
				buf.WriteString(tc.render(i))
				buf.WriteString("\n")
			}
			src := replaceSection(validManifest(), tc.listKey, buf.String())
			_, err := ParseManifest([]byte(src))
			if err == nil {
				t.Fatalf("expected count-cap rejection for %s", tc.field)
			}
			if !strings.Contains(err.Error(), tc.fragment) {
				t.Fatalf("expected fragment %q in error, got %v", tc.fragment, err)
			}
		})
	}
}

func TestValidateKTypeName(t *testing.T) {
	if err := ValidateKTypeName("ext.acme.shipping_label", "acme"); err != nil {
		t.Errorf("happy-path rejected: %v", err)
	}
	if err := ValidateKTypeName("ext.evil.shipping_label", "acme"); err == nil {
		t.Error("expected publisher-mismatch rejection")
	}
	if err := ValidateKTypeName("shipping_label", "acme"); err == nil {
		t.Error("expected unqualified-name rejection")
	}
	if err := ValidateKTypeName("ext.acme.UPPERCASE", "acme"); err == nil {
		t.Error("expected uppercase rejection")
	}
}

// TestParseManifestAgentToolTimeoutDefaultPersisted regression-tests
// the bug where the validator wrote the spec §6 "default 10s" timeout
// to the loop-variable copy of AgentToolRef rather than the underlying
// slice element, so the returned *Manifest carried an empty timeout
// and PublishVersion serialised the JSONB column with no default.
// After the fix, m.AgentTools[i].Timeout MUST equal "10s" when the
// YAML omits the field, AND json.Marshal MUST emit the default in the
// stored manifest JSONB (which is what the catalog UI reads).
func TestParseManifestAgentToolTimeoutDefaultPersisted(t *testing.T) {
	src := removeLine("    timeout: 5s")(validManifest())
	m, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("parse rejected after timeout removal: %v", err)
	}
	if len(m.AgentTools) != 1 {
		t.Fatalf("want 1 agent tool, got %d", len(m.AgentTools))
	}
	if got := m.AgentTools[0].Timeout; got != "10s" {
		t.Errorf("manifest.AgentTools[0].Timeout = %q, want \"10s\" (spec §6 default)", got)
	}
	// Catalog UI reads back through json.Unmarshal — the JSON form
	// MUST also carry the default, otherwise a re-parse on the read
	// side would zero it out.
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(out), `"timeout":"10s"`) {
		t.Errorf("json output missing defaulted timeout=10s; got: %s", string(out))
	}
}

// TestParseManifestRejectsHyphensInName regression-tests the bug
// where the Go validator's nameRegex / publisherSlugRegex allowed `-`
// in publisher / slug segments but the DB CHECK constraints in
// migrations/000068_marketplace.sql:99-103 rejected hyphens with the
// pattern `^[a-z][a-z0-9_]*$`. The validator MUST reject hyphens
// up-front so the publisher receives a clear field-level error
// instead of an opaque CHECK violation at INSERT time.
func TestParseManifestRejectsHyphensInName(t *testing.T) {
	cases := []struct {
		name       string
		manifestIn string
		field      string
	}{
		{
			name:       "hyphen in publisher segment",
			manifestIn: replaceLine("name: acme.shipping", "name: my-pub.shipping")(validManifest()),
			field:      "name",
		},
		{
			name:       "hyphen in slug segment",
			manifestIn: replaceLine("name: acme.shipping", "name: acme.my-ext")(validManifest()),
			field:      "name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.manifestIn))
			if err == nil {
				t.Fatal("expected rejection but parse succeeded")
			}
			// Must be a ManifestError or MultiManifestError that surfaces
			// the field name. Using errors.Is for ErrInvalidManifest
			// is the contract callers rely on.
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("errors.Is(err, ErrInvalidManifest) = false; err=%v", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error message %q does not mention field %q", err.Error(), tc.field)
			}
		})
	}
}

// TestParseManifestRejectsHyphensInExtKTypeName regression-tests the
// same fix on the extKTypeNameRegex used by ValidateKTypeName — the
// publisher segment of `ext.<pub>.<label>` MUST also match the DB
// publisher pattern so an installed extension's KTypes can resolve
// against a real marketplace_extensions row.
func TestParseManifestRejectsHyphensInExtKTypeName(t *testing.T) {
	if err := ValidateKTypeName("ext.my-pub.shipping_label", "my-pub"); err == nil {
		t.Error("expected rejection of hyphenated publisher in ext.<pub>.<label>")
	}
}

// TestManifestSerialisesAsSnakeCase regression-tests the bug where
// the Manifest struct lacked json tags, so json.Marshal emitted
// Go-style PascalCase keys (`SchemaVersion`, `MinKappVersion`, ...).
// PublishVersion stores the marshalled JSON in the manifest JSONB
// column, and the catalog UI reads it back through this same struct;
// a key-case mismatch silently zeroed every field. After the fix,
// every key MUST be the snake_case form the YAML source uses.
func TestManifestSerialisesAsSnakeCase(t *testing.T) {
	m, err := ParseManifest([]byte(validManifest()))
	if err != nil {
		t.Fatalf("happy-path parse failed: %v", err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	wantKeys := []string{
		`"schema_version":`, `"min_kapp_version":`, `"max_kapp_version":`,
		`"features_required":`, `"permissions_required":`,
		`"agent_tools":`, `"webhooks_consumed":`, `"posting_hooks":`,
		`"ui_extensions":`, `"secrets_required":`,
		`"publisher":`, `"slug":`,
	}
	for _, k := range wantKeys {
		if !strings.Contains(string(out), k) {
			t.Errorf("json output missing snake_case key %s; got: %s", k, string(out))
		}
	}
	pascalCaseKeys := []string{
		`"SchemaVersion":`, `"MinKappVersion":`, `"FeaturesRequired":`,
		`"AgentTools":`, `"WebhooksConsumed":`,
	}
	for _, k := range pascalCaseKeys {
		if strings.Contains(string(out), k) {
			t.Errorf("json output still emits Go-style key %s; struct tags regressed", k)
		}
	}
}

// TestInstallStatusTransitionAllowed pins the install lifecycle FSM
// added to close the "UpdateInstallStatus accepts any transition"
// finding. Spec posture: convergent self-loops are allowed (for the
// at-least-once worker), uninstalled is terminal, and unauthorised
// jumps like `uninstalled→active` or `active→pending` are rejected.
func TestInstallStatusTransitionAllowed(t *testing.T) {
	allowed := map[InstallStatus][]InstallStatus{
		InstallStatusPending:     {InstallStatusInstalling, InstallStatusFailed, InstallStatusUninstalled},
		InstallStatusInstalling:  {InstallStatusActive, InstallStatusFailed, InstallStatusUninstalled},
		InstallStatusActive:      {InstallStatusDisabled, InstallStatusFailed, InstallStatusUninstalled},
		InstallStatusDisabled:    {InstallStatusActive, InstallStatusUninstalled},
		InstallStatusFailed:      {InstallStatusInstalling, InstallStatusUninstalled},
		InstallStatusUninstalled: {},
	}
	allStates := []InstallStatus{
		InstallStatusPending, InstallStatusInstalling, InstallStatusActive,
		InstallStatusDisabled, InstallStatusFailed, InstallStatusUninstalled,
	}
	for _, from := range allStates {
		// Self-loop is always allowed for idempotency.
		if !installStatusTransitionAllowed(from, from) {
			t.Errorf("self-loop %s→%s rejected", from, from)
		}
		for _, to := range allStates {
			if from == to {
				continue
			}
			want := false
			for _, ok := range allowed[from] {
				if ok == to {
					want = true
					break
				}
			}
			got := installStatusTransitionAllowed(from, to)
			if got != want {
				t.Errorf("installStatusTransitionAllowed(%s, %s) = %v, want %v",
					from, to, got, want)
			}
		}
	}
}

func TestMultiManifestErrorUnwrapErrInvalidManifest(t *testing.T) {
	src := replaceLine("schema_version: 1", "schema_version: 2")(validManifest())
	_, err := ParseManifest([]byte(src))
	if err == nil {
		t.Fatal("expected rejection")
	}
	if !errors.Is(err, ErrInvalidManifest) {
		t.Errorf("errors.Is(err, ErrInvalidManifest) returned false; err=%v", err)
	}
}

// replaceLine returns a mutator that swaps a whole line (matched by
// prefix) for `replacement`. Used to make the table-driven tests
// terse.
func replaceLine(prefixOrFull, replacement string) func(string) string {
	return func(src string) string {
		out := make([]string, 0, 64)
		replaced := false
		for _, line := range strings.Split(src, "\n") {
			if !replaced && (line == prefixOrFull || strings.TrimRight(line, " ") == prefixOrFull) {
				out = append(out, replacement)
				replaced = true
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
}

func removeLine(prefixOrFull string) func(string) string {
	return func(src string) string {
		out := make([]string, 0, 64)
		removed := false
		for _, line := range strings.Split(src, "\n") {
			if !removed && (line == prefixOrFull || strings.TrimRight(line, " ") == prefixOrFull) {
				removed = true
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
}

// replaceSection swaps the entire `key:`-rooted block (key line + all
// indented child lines) for `replacement`. Used to stuff a list
// section with > max-cap entries.
func replaceSection(src, key, replacement string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		if !skipping && strings.HasPrefix(strings.TrimSpace(line), key) {
			out = append(out, strings.TrimRight(replacement, "\n"))
			skipping = true
			continue
		}
		if skipping {
			if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") || line == "" {
				continue
			}
			skipping = false
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
