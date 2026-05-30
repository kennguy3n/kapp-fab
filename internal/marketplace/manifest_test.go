package marketplace

import (
	"bytes"
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
	var merrs *ManifestErrors
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

func TestManifestErrorsUnwrapErrInvalidManifest(t *testing.T) {
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
