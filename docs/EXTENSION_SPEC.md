# Kapp Extension Specification

**Status:** v1.0 (Phase 1 / B1)
**Audience:** extension authors, marketplace reviewers, platform engineers
**Related:** [PLUGIN_DEVELOPER_GUIDE.md](./PLUGIN_DEVELOPER_GUIDE.md),
[KTYPE_AUTHORING_GUIDE.md](./KTYPE_AUTHORING_GUIDE.md),
`internal/marketplace/`, `internal/extensions/`

A **Kapp Extension** is a single, signed bundle that adds new
business objects, workflows, agent tools, webhook subscriptions, and
UI surfaces to a tenant — without modifying the Kapp kernel. Every
extension is declarative; *all* executable logic runs out-of-process
behind a webhook (see B4) or, in a future phase, a WASM sandbox.

This document defines the bundle's wire format, the validation
rules the marketplace enforces, and the runtime contract between
the extension and the platform.

---

## 1. Conceptual Model

```
                  ┌────────────────────────────────────┐
                  │  Marketplace (operator-curated)    │
                  │  - extensions table                │
                  │  - extension_versions table        │
                  │  - bundle storage (S3 / R2 / …)    │
                  └───────────────┬────────────────────┘
                                  │  install
                                  ▼
   ┌──────────────────────────────────────────────────────┐
   │  Tenant runtime                                      │
   │  ─ tenant_ktypes (ext.<publisher>.<slug> namespace)  │
   │  ─ webhook_subscriptions (extension-owned)           │
   │  ─ agent tool registry (extension-owned)             │
   │  ─ extension_installs (per-tenant settings)          │
   └──────────────────────────────────────────────────────┘
                                  │  dispatch
                                  ▼
                  ┌────────────────────────────────────┐
                  │  Extension service (your code)     │
                  │  HTTPS webhook endpoint(s)         │
                  │  receives signed payloads          │
                  └────────────────────────────────────┘
```

**Three identities matter:**

| Identity        | Where defined                                      | Purpose |
|-----------------|----------------------------------------------------|---------|
| Publisher       | `extension_publishers` table                       | Owns the API key that uploads versions; gets reviewed once. |
| Extension       | `extensions` table (`name` is publisher-scoped)    | A product (e.g. `acme.shipping`). Stable identity across versions. |
| Extension version | `extension_versions` table                       | An immutable, hashed bundle. Tenants install a specific version. |

**Three boundaries matter:**

1. **No in-kernel code.** Extensions never ship Go (or any) compiled
   logic into the API binary. All custom logic runs in the
   extension's own HTTPS service, invoked via signed webhooks.
2. **Namespaced KTypes.** Extension-authored KTypes live in
   `ext.<publisher>.<slug>` — disjoint from platform KTypes
   (`crm.*`, `finance.*`, …) and tenant custom KTypes (`custom.*`).
   The DB CHECK on `tenant_ktypes` is extended to
   `^(custom|ext)\.[a-z][a-z0-9_.]*$`.
3. **Permission scope is the manifest.** An extension can only do
   what its `permissions_required` block declares; the install
   step refuses if the tenant's plan doesn't grant any of them.
   Audit log tags every action with `extension_id` for forensic
   traceability.

---

## 2. Bundle Layout

An extension bundle is a `.tar.gz` archive of a single directory.
The archive's root MUST contain a `kapp-extension.yaml` manifest.
All other files are referenced *by relative path* from the manifest.

```
acme-shipping-1.0.0.tar.gz
└── acme-shipping/
    ├── kapp-extension.yaml          ← required, schema-validated
    ├── settings.json                 ← JSON Schema for per-tenant config
    ├── ktypes/
    │   ├── shipping_label.json       ← one KType per file
    │   └── carrier_rate.json
    ├── workflows/
    │   └── shipping_workflow.json    ← state-machine definitions
    ├── tools/
    │   └── generate_label.json       ← agent tool descriptors
    ├── ui/
    │   ├── shipping-pane.js          ← ESM module
    │   └── shipping-widget.js
    ├── assets/
    │   ├── icon.png                  ← 256×256 PNG, ≤64 KB
    │   └── screenshots/
    │       └── *.png
    ├── docs/
    │   ├── README.md                 ← shown on the listing page
    │   └── CHANGELOG.md
    └── LICENSE
```

**Hard limits enforced by the marketplace at upload time:**

| Constraint                                      | Value     |
|-------------------------------------------------|-----------|
| Total bundle size (post-extract)                | 10 MiB    |
| Number of KType files                           | 32        |
| Number of workflow files                        | 16        |
| Number of agent tool files                      | 32        |
| Number of webhook subscriptions in manifest     | 16        |
| Number of UI extension slots                    | 16        |
| Manifest YAML size                              | 64 KiB    |
| Any single file inside the bundle               | 2 MiB     |
| Icon dimensions                                 | 256×256 px |

Anything exceeding these is rejected before the bundle is hashed,
so the marketplace never stores rejected bundles.

---

## 3. Manifest (`kapp-extension.yaml`)

The manifest is YAML 1.2. Everything below is normative — the
field-by-field schema lives at `internal/marketplace/manifest_schema.json`
(B2) and is validated server-side at upload time AND client-side by
`kapp-ext validate` (B8).

```yaml
# kapp-extension.yaml — extension manifest, schema v1
schema_version: 1                # MUST be 1 for this spec

# --- Identity --------------------------------------------------------
name: "acme.shipping"            # publisher_slug "." extension_slug
                                 # ^[a-z][a-z0-9_-]{2,31}\.[a-z][a-z0-9_-]{2,31}$
version: "1.0.0"                 # SemVer 2.0.0
author: "Acme Corp"
license: "MIT"                   # SPDX identifier or "Proprietary"
description: |
  Shipping label generation and carrier rate lookup for Kapp's
  sales-order workflow.
homepage: "https://acme.example/kapp/shipping"
support_email: "support@acme.example"
icon: "./assets/icon.png"        # relative to bundle root

# --- Platform compatibility -----------------------------------------
min_kapp_version: "1.0.0"        # SemVer; install refuses below
max_kapp_version: "1.x"          # optional; default ">=min"

# --- What the extension needs from the host tenant ------------------
features_required:               # tenant plan MUST grant all of these
  - "inventory"
  - "sales"
permissions_required:            # role-grant matrix MUST allow all
  - "inventory.read"
  - "sales.order.read"
  - "sales.order.write"

# --- What the extension provides ------------------------------------

ktypes:                          # KType schemas (B3 §3.1)
  - schema: ./ktypes/shipping_label.json
  - schema: ./ktypes/carrier_rate.json

workflows:                       # state-machine definitions
  - definition: ./workflows/shipping_workflow.json

agent_tools:                     # callable tools for the agent platform
  - definition: ./tools/generate_label.json
    handler: webhook             # webhook | wasm (wasm in Phase 2)
    endpoint: "${EXTENSION_WEBHOOK_BASE}/generate-label"
    timeout: "10s"               # default 10s, max 30s
    retry:
      max_attempts: 2            # initial try + retries; 1 means no retry
      backoff: "exponential"     # linear | exponential

webhooks_consumed:               # platform events the extension wants
  - event: "sales.order.status_changed"
    filter:                      # JSONPath-style equality filter
      status: "confirmed"
    endpoint: "${EXTENSION_WEBHOOK_BASE}/on-order-confirmed"

posting_hooks:                   # B4 — declarative, fired post-commit
  - ktype: "ext.acme.shipping_label"
    when: "after_create"         # after_create | after_update | after_delete
    endpoint: "${EXTENSION_WEBHOOK_BASE}/hooks/label-created"

ui_extensions:                   # B5 — UI surface slots
  - slot: "right_pane"
    target_ktype: "sales.order"
    component_url: "./ui/shipping-pane.js"
  - slot: "dashboard_widget"
    component_url: "./ui/shipping-widget.js"
  - slot: "record_list_action"
    target_ktype: "sales.order"
    label: "Generate label"
    component_url: "./ui/shipping-pane.js#action"

# --- Per-tenant config -----------------------------------------------
settings_schema: ./settings.json  # JSON Schema draft 2020-12

# --- Secrets the extension expects to be wired at install time -------
# These are operator-supplied per-install (UI prompts the installer
# for each one). They are written into extension_installs.settings
# under settings.secrets.<key> and surfaced to the extension's
# webhook via the X-Kapp-Extension-Secrets header (B4).
secrets_required:
  - key: "EASYPOST_API_KEY"
    label: "EasyPost API key"
    description: "Used for rate lookup and label generation."
    sensitive: true
```

### 3.1 Resolution of `${...}` placeholders

- `${EXTENSION_WEBHOOK_BASE}` — set at install time by the operator;
  the marketplace UI's install dialog prompts for it. The placeholder
  is interpolated into the stored manifest copy in
  `extension_versions.manifest` so the runtime resolver never has to
  re-fetch the bundle.
- No other placeholders are supported in v1. Manifest interpolation
  is intentionally minimal so a malicious extension can't, e.g.,
  exfiltrate the operator's region by templating `${AWS_REGION}` into
  its endpoint.

### 3.2 Versioning rules

- `version` MUST be SemVer 2.0.0. Pre-release tags (`-alpha.1`) are
  allowed but flagged on the listing page.
- A given `(name, version)` is **immutable**. Re-uploading the same
  version fails with `409 Conflict` (the bundle hash already exists).
- Bumping the **major** version triggers a separate review queue
  entry — operators frequently want to re-audit majors for
  permission-scope creep.
- The marketplace serves the **highest non-pre-release version** as
  "latest" unless the installer pins a specific version.

---

## 4. KType Schemas (extension-authored)

Extension KTypes are normal KTypes (see
[KTYPE_AUTHORING_GUIDE.md](./KTYPE_AUTHORING_GUIDE.md)) with three
extra constraints:

1. **Name** MUST match `^ext\.<publisher_slug>\.[a-z][a-z0-9_]*$`,
   where `<publisher_slug>` equals the first segment of the manifest
   `name`. The marketplace validator checks this against the manifest
   before persisting — a shipping-label KType named
   `ext.acme.shipping_label` is fine; `ext.evil.shipping_label`
   inside an `acme.*` extension is a rejection.
2. **Field types** are restricted to the *safe subset* enforced by
   `internal/ktype/validate_custom.go` (extended for the `ext.*`
   namespace). The dangerous types (`raw_sql`, `pg_function`) are
   rejected.
3. **Indexes are advisory.** Extensions cannot declare arbitrary
   SQL indexes; they can mark fields as `"indexed": true` and the
   platform creates the JSONB expression index automatically (see
   A3). Index creation runs in a backoff-scheduled background task,
   never inside the install transaction.

Example (`ktypes/shipping_label.json`):

```json
{
  "name": "ext.acme.shipping_label",
  "version": 1,
  "fields": [
    {"name": "sales_order_id", "type": "ref", "ktype": "sales.order", "required": true, "indexed": true},
    {"name": "carrier",        "type": "enum", "values": ["ups","fedex","usps"], "required": true},
    {"name": "tracking_number","type": "text", "max_length": 64, "indexed": true},
    {"name": "label_pdf_url",  "type": "url"},
    {"name": "cost",           "type": "money"},
    {"name": "status",         "type": "enum",
       "values": ["draft","purchased","void"], "default": "draft"}
  ],
  "views":       {"list": {...}, "form": {...}},
  "cards":       {"summary": "{{carrier}} {{tracking_number}}"},
  "permissions": {"read": ["sales.user"], "write": ["sales.user"]}
}
```

---

## 5. Workflows (extension-authored)

Same JSON shape as the platform's built-in workflows
(`internal/workflow/engine.go`). The only restriction is that
`actions` listed in a transition MUST resolve to either:

- a built-in workflow action (`emit_event`, `set_field`,
  `notify_user`, …), OR
- one of the extension's own `agent_tools[]` (referenced by `name`).

The marketplace validator walks every transition and rejects
references to unknown actions before approving the bundle.

---

## 6. Agent Tools

Each `tools/*.json` follows the existing agent-tool descriptor schema
(see `internal/agents/registry.go`). The manifest's `agent_tools[]`
entry wires the descriptor to a handler:

```yaml
agent_tools:
  - definition: ./tools/generate_label.json
    handler: webhook
    endpoint: "${EXTENSION_WEBHOOK_BASE}/generate-label"
    timeout: "10s"
    retry: {max_attempts: 2, backoff: "exponential"}
```

At runtime, the extension runtime engine (B3) translates an invocation
into a signed HTTPS POST (see B4 §4 for the payload shape) and maps
the response back to an `AgentToolResponse`. The webhook MUST return
within `timeout`; otherwise the call fails with `DeadlineExceeded`
and the agent platform records the failure in the audit log tagged
with `extension_id`.

---

## 7. Webhook Subscriptions

`webhooks_consumed[]` registers per-extension listeners on the
platform's event bus. At install time the runtime creates one
`webhook_subscriptions` row per entry, owned by the install record
(uninstall cascades). The `filter` block is an equality match on
the event payload — anything more expressive belongs in the
extension's own handler.

---

## 8. UI Extensions

UI bundles are ES modules (`type=module`) loaded from the
marketplace CDN. They run inside a sandboxed `<iframe>` with a
restrictive Content-Security-Policy:

```
default-src 'none';
script-src 'self' https://marketplace.<operator-host>/;
style-src  'self' 'unsafe-inline';
img-src    'self' https://marketplace.<operator-host>/ data:;
connect-src https://marketplace.<operator-host>/ ${EXTENSION_WEBHOOK_BASE};
frame-ancestors 'self';
```

`connect-src` permits two origins exactly: the marketplace CDN
(for bundle/asset fetches) and the extension's own
`EXTENSION_WEBHOOK_BASE` (for direct API calls). Any other
cross-origin call is blocked by the browser, so a malicious
extension cannot exfiltrate session data to an attacker-controlled
domain.

**Slot reference:**

| Slot                  | `target_ktype` required? | Receives                                |
|-----------------------|--------------------------|-----------------------------------------|
| `right_pane`          | yes                      | currently-open record (read-only)       |
| `dashboard_widget`    | no                       | tenant summary (read-only)              |
| `record_list_action`  | yes                      | array of selected record ids            |
| `settings_page`       | no                       | per-tenant settings draft (read+write)  |

The host injects a postMessage-based API client whose surface is
the union of the extension's `permissions_required[]` — there's no
way for a `inventory.read`-only extension to issue a
`finance.payment.write` from its iframe.

---

## 9. Settings Schema

`settings_schema` is a JSON Schema (draft 2020-12) document
describing the per-tenant configuration the extension expects. The
marketplace UI auto-renders a form from the schema; the form's
output is persisted to `extension_installs.settings` as JSONB.

Secrets declared in `secrets_required[]` are stored under
`settings.secrets.<key>` and **never** returned in `GET .../settings`
responses; the UI shows them only as "set" / "not set" toggles. On
webhook dispatch they're surfaced to the extension via the
`X-Kapp-Extension-Secrets` HTTP header as a comma-separated key
list, plus the decrypted values in a per-call short-lived JWT that
the extension validates with its bundled HMAC secret.

---

## 10. Bundle Hashing & Signing

Marketplace upload computes SHA-256 of the raw `.tar.gz` and stores
it as `extension_versions.bundle_hash`. The hash is part of the
install contract — at install time the runtime re-fetches the bundle
and verifies the hash before extracting. Tampering with bundle
storage produces an install-time integrity error, not silent
corruption.

Optional code-signing (Phase 2): publishers may upload a detached
PGP signature; the marketplace verifies it against the publisher's
registered public key and the `verified` badge on the listing
includes a signing chain summary.

---

## 11. Compatibility Promise

The fields in this document are guaranteed stable for the lifetime
of `schema_version: 1`. Additions are backwards-compatible (new
optional fields, new slot names). Breaking changes require
incrementing `schema_version` AND a co-existence window during which
the marketplace serves both schema versions.

The validator's rejection rules are tightened only across schema
versions, never within one — an extension that uploads cleanly
today will keep uploading cleanly until `schema_version: 2` is
declared.

---

## 12. Worked Example

A minimal end-to-end Stripe-Payments-style extension is shipped
under `examples/extensions/hello-stripe/` once B3 lands. The
example covers:

- Manifest with one KType (`ext.kapp.stripe_intent`), one webhook
  subscription (`finance.invoice.posted`), one agent tool, and
  one dashboard widget.
- Settings schema with two secrets (`STRIPE_API_KEY`,
  `STRIPE_WEBHOOK_SIGNING_KEY`).
- A tiny webhook server (Node, ~50 lines) demonstrating how to
  verify the `X-Kapp-Signature` header (B4).

See [PLUGIN_DEVELOPER_GUIDE.md](./PLUGIN_DEVELOPER_GUIDE.md) §13.2
for the existing gRPC plugin model — extensions are the higher-level
packaging that wraps zero or more agent tools, KTypes, workflows,
UI components, and webhook subscriptions into a single installable
unit. Both surfaces coexist: a developer who wants raw gRPC bypasses
the marketplace; a developer who wants the marketplace's review,
distribution, and per-tenant settings ships an extension.
