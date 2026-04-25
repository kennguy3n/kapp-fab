# KType Authoring Guide

A KType is the schema-as-data definition that powers Kapp. Every
business object — leads, invoices, tickets, lessons, certificates
— is a KType. This guide walks through authoring a new KType end
to end, using a hypothetical `garage.work_order` as the worked
example.

## Anatomy of a KType

A KType is a JSON document with five top-level keys:

```json
{
  "name": "garage.work_order",
  "version": 1,
  "fields":      [ … ],
  "views":       { "list": …, "form": …, "kanban": … },
  "cards":       { "summary": "…" },
  "permissions": { "read": [ … ], "write": [ … ] },
  "workflow":    { … }
}
```

* `name` — globally unique. Convention is `<domain>.<noun>`.
* `version` — bumped only for breaking schema changes. Old records
  keep their `ktype_version` so migrations are explicit.
* `fields` — typed schema. Validation is enforced by
  `internal/ktype/validator.go` on every Create / Update.
* `views` — UI hints the React frontend uses to render list,
  form, and kanban screens.
* `cards.summary` — Mustache-style template used for KChat cards
  and the global search result list.
* `permissions.read` / `.write` — role names. The session must
  have at least one matching role for the operation.
* `workflow` (optional) — registers a state-machine workflow if
  the record has a lifecycle.

## Step 1: declare the KType

Pick a package — for the worked example we'd create
`internal/garage/garage.go`:

```go
package garage

import "github.com/kennguy3n/kapp-fab/internal/ktype"

const KTypeWorkOrder = "garage.work_order"

var workOrderSchema = []byte(`{
  "name": "garage.work_order",
  "version": 1,
  "fields": [
    {"name": "vehicle_id", "type": "ref", "ktype": "garage.vehicle", "required": true},
    {"name": "customer_id", "type": "ref", "ktype": "crm.customer", "required": true},
    {"name": "complaint", "type": "text", "max_length": 2000},
    {"name": "status", "type": "enum",
      "values": ["draft", "in_progress", "ready", "delivered"], "default": "draft"},
    {"name": "estimated_total", "type": "money"},
    {"name": "opened_at", "type": "datetime"}
  ],
  "views": {
    "list":   {"columns": ["status", "vehicle_id", "customer_id", "estimated_total"]},
    "form":   {"sections": [
      {"title": "Vehicle",   "fields": ["vehicle_id", "complaint"]},
      {"title": "Customer",  "fields": ["customer_id"]},
      {"title": "Estimate",  "fields": ["estimated_total"]},
      {"title": "Lifecycle", "fields": ["status", "opened_at"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "vehicle_id", "card_subtitle": "customer_id"}
  },
  "cards": {"summary": "{{vehicle_id}} — {{status}} (${{estimated_total}})"},
  "permissions": {
    "read":  ["tenant.member"],
    "write": ["garage.tech", "tenant.admin"]
  },
  "workflow": {
    "name": "garage.work_order.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "in_progress", "ready", "delivered"],
    "transitions": [
      {"from": ["draft"],         "to": "in_progress", "action": "start"},
      {"from": ["in_progress"],   "to": "ready",       "action": "mark_ready"},
      {"from": ["ready"],         "to": "delivered",   "action": "deliver"}
    ]
  }
}`)

func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeWorkOrder, Version: 1, Schema: workOrderSchema},
	}
}
```

## Step 2: register against every tenant

Call `RegisterKTypes` from the wizard's KType seeder so every
tenant gets the KType the moment it's provisioned. New tenants
created via `tenant.WizardStore.Create` pick it up automatically;
existing tenants get it via the wizard's idempotent re-run.

```go
// internal/tenant/wizard.go (excerpt)
for _, kt := range garage.All() {
    if err := registry.Register(ctx, kt); err != nil {
        return err
    }
}
```

## Step 3: SQL migration

If your KType needs custom indexes (e.g. uniqueness on a
JSONB-derived expression), drop a migration:

```sql
-- migrations/000099_garage_work_orders.sql
CREATE INDEX IF NOT EXISTS garage_work_order_status_idx
    ON krecords (tenant_id, (data->>'status'))
    WHERE ktype = 'garage.work_order';
```

The `krecords` table itself does NOT need a per-KType DDL; the
generic store handles inserts.

## Step 4: HTTP exposure

Generic CRUD is already available:

* `POST /api/v1/krecords` with `{"ktype": "garage.work_order", "data": {…}}`
* `GET  /api/v1/krecords?ktype=garage.work_order`
* `GET  /api/v1/krecords/{id}`
* `PATCH /api/v1/krecords/{id}` with `{"data": {…}}`
* `POST /api/v1/krecords/{id}/transition` with `{"action": "start"}`

If you need typed endpoints (custom validation, computed fields,
side-effects), drop a `services/api/garage_handlers.go` and wire
it into `services/api/main.go` under
`r.Route("/api/v1/garage", …)` with the standard middleware
stack.

## Step 5: agent tool

If the KType participates in agent flows, add a tool in
`internal/agents/garage_tools.go`:

```go
type startWorkOrderTool struct{ records *record.PGStore }

func (t *startWorkOrderTool) Name() string               { return "garage.start_work_order" }
func (t *startWorkOrderTool) RequiresConfirmation() bool { return true }
func (t *startWorkOrderTool) Invoke(ctx context.Context, inv Invocation) (*Result, error) {
    var in struct{ ID uuid.UUID `json:"id"` }
    if err := decodeInputs(inv, &in); err != nil { return nil, err }
    if inv.Mode == ModeDryRun {
        body, _ := json.Marshal(in)
        return &Result{Summary: "Would start work order " + in.ID.String(), Preview: body}, nil
    }
    // … perform the transition via t.records …
}
```

Register from the agents wiring (`services/api/main.go::registerAgentTools`).

## Step 6: KChat command (optional)

Add a `case "work-order"` arm in
`services/kchat-bridge/commands.go::Dispatch` that calls
`d.createRecord(ctx, req, garage.KTypeWorkOrder, …)` so users can
file work orders directly from chat.

## Step 7: frontend

* Add a list page in `apps/web/src/pages/garage/WorkOrdersPage.tsx`
  using the `<RecordListPage ktype="garage.work_order" />` shell.
* Add a detail page that wraps `<RecordDetailPage />`.
* Register both in `App.tsx`.
* Add a nav entry in `Layout.tsx` if appropriate.

The `<RecordListPage />` and `<RecordDetailPage />` shells read
the `views` block from your KType and render automatically — most
new KTypes need zero custom React code.

## Step 8: tests

* Unit-test any custom store / agent logic alongside its package
  (`garage/garage_test.go`).
* Add an integration test in
  `internal/integrationtest/phase_<latest>_test.go` covering:
  * Create / list / transition through the workflow;
  * RLS isolation (tenant A cannot see tenant B's rows);
  * Schema validation rejects bad inputs.

## Common pitfalls

* **Forgetting RLS on a custom table** — every tenant-scoped table
  needs `ENABLE ROW LEVEL SECURITY`, the `tenant_isolation` policy,
  and `GRANT SELECT, INSERT, UPDATE, DELETE … TO kapp_app`.
* **Bypassing `dbutil.WithTenantTx`** — direct `pool.Query` calls
  do not set `app.tenant_id` and will return zero rows under RLS.
* **Bumping `version` for non-breaking changes** — only bump when
  the field set or types change in a way that breaks existing
  records. Adding a new optional field is NOT a version bump.
* **Hardcoding tenant IDs** — always read from
  `platform.TenantFromContext(ctx)`.
* **Skipping `ktype.Registry.Get`** — the generic record store
  validates writes against the cached schema; bypassing it means
  you also bypass field-level validation and encryption.
