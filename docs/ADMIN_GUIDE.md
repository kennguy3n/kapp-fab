# End-User Administration Guide

For **tenant administrators** (not platform operators). Walks through
day-to-day configuration of a Kapp tenant. Operator procedures live in
[OPERATOR_GUIDE.md](./OPERATOR_GUIDE.md) and the runbooks in this
directory.

Cross-references:

- Developer concepts: [DEVELOPER_GUIDE.md](./DEVELOPER_GUIDE.md)
- KType authoring: [KTYPE_AUTHORING_GUIDE.md](./KTYPE_AUTHORING_GUIDE.md)
- API reference: [API_REFERENCE.md](./API_REFERENCE.md)

Screenshots referenced below live in `docs/screenshots/` and are kept
in sync by the Playwright capture spec
`scripts/capture-screenshots.spec.ts`.

---

## 11.1 Tenant Setup

The setup wizard runs the first time a tenant admin logs in via KChat
SSO. Each step is idempotent — re-running the wizard updates rather
than re-creates.

1. **Company profile** — legal name, country, time zone, locale.
   Country pre-fills the chart of accounts and tax pack (see
   [TAX_PACK_MAINTENANCE.md](./TAX_PACK_MAINTENANCE.md)).
2. **Plan selection** — the wizard surfaces the plans defined in
   `plan_definitions`. Per-plan feature flags are seeded into
   `tenant_features` (`migrations/000021_tenant_features.sql`).
3. **User invitation** — invite by KChat user ID or email. Invited
   users land in `user_tenants` with `status='invited'` until they
   accept.
4. **Role assignment** — pick from built-in roles (Owner, Admin,
   Manager, Member) or copy one as a starting point for a custom role
   (§11.2).
5. **Module activation** — enable the modules the plan includes:
   Finance, CRM, Inventory, HR, Helpdesk, Manufacturing.

The wizard writes one audit-log entry per step
(`action='wizard.step.complete'`) so the platform admin can replay it.

---

## 11.2 User Management

**Create a user**: invite via the Setup Wizard ("Users") or directly:

```http
POST /api/v1/users
Authorization: Bearer <admin-token>
Content-Type: application/json

{
  "email": "alice@tenant.example",
  "kchat_user_id": "<KChat ID>",
  "display_name": "Alice",
  "role": "manager"
}
```

**Role hierarchy** (see `migrations/000050_role_hierarchy.sql`):

```
Owner       ▶ full read/write on every record + admin endpoints
  └─ Admin  ▶ full read/write on every record (no platform-admin actions)
       └─ Manager  ▶ read all records in their team, write their own
             └─ Member ▶ read/write their own records only
```

Permissions are checked at the API layer
(`internal/authz/policies.go`) and the DB layer (RLS) — both must
allow the operation.

**Custom role creation**:

```http
POST /api/v1/roles
{
  "name": "finance-only",
  "permissions": [
    "finance.journal.read",
    "finance.journal.write",
    "finance.account.read"
  ],
  "parent": "member"
}
```

**Field-level permissions**: configured via KType schema
(`docs/KTYPE_AUTHORING_GUIDE.md` §`field_acls`). Per-field reads and
writes are enforced by the record store
(`internal/record/store.go`).

**Session management**:

```http
# List active sessions for a user
GET /api/v1/admin/users/{user_id}/sessions

# Force logout (single session)
DELETE /api/v1/admin/sessions/{session_id}

# Force logout (all sessions for a user)
DELETE /api/v1/admin/users/{user_id}/sessions
```

---

## 11.3 Module Configuration

Each module ships an admin UI under `apps/web/src/pages/admin/<module>`.
Configuration is stored under `tenants.config` JSONB and module-specific
tables.

### Finance

- **Chart of accounts** — pre-seeded by country tax pack; customize
  via `POST /api/v1/finance/accounts`.
- **Fiscal periods** — `migrations/000004_finance_extensions.sql`
  defines `fiscal_periods`. Lock a period to freeze postings:
  `POST /api/v1/finance/periods/{id}/lock`.
- **Tax codes** — `tax_codes` table; seeded by country tax pack.
- **Bank accounts** — `bank_accounts` table; link via the bank-connect
  wizard (Plaid / Codat for North America, EBICS for EU).
- **Cost centers** — `cost_centers` table (Phase I).

### CRM

- **Pipeline configuration** — pipelines per KType (`crm.deal`,
  `crm.lead`), stages stored in the KType definition.
- **Deal stages** — see `docs/KTYPE_AUTHORING_GUIDE.md` for the
  workflow schema.

### Inventory

- **Warehouses** — `inventory_warehouses`.
- **Item categories** — KType `inventory.item` `category` field.
- **Reorder points** — per item, stored in `inventory_items.reorder_point`.
- **Batches** — `inventory_batches` (`migrations/000040_batch_tracking.sql`).

### HR

- **Departments / positions** — KTypes `hr.department`, `hr.position`.
- **Leave policies** — `leave_ledger` (`migrations/000006_hr.sql`) is
  the append-only ledger; policy parameters live in the tenant config.
- **Shift patterns** — KType `hr.shift_pattern` + custom workflow.

### Helpdesk

- **SLA policies** — `sla_policies` (`migrations/000018_helpdesk.sql`).
- **Email accounts** — `helpdesk_mailboxes`
  (`migrations/000058_helpdesk_mailboxes.sql`); add via the
  `/api/v1/helpdesk/mailboxes` endpoint.
- **Knowledge base categories** — KType `helpdesk.article`.

### Manufacturing

- **Workstations / routings** — KTypes `mfg.workstation`,
  `mfg.routing`.
- **BOM templates** — KType `mfg.bom`.

---

## 11.4 Integration Setup

| Integration            | Wizard / endpoint                                        | Notes                                            |
| ---------------------- | -------------------------------------------------------- | ------------------------------------------------ |
| Bank connection        | `/api/v1/integrations/bank-connect`                      | Plaid / Codat / EBICS. Tokens stored encrypted.   |
| Payment gateway         | `/api/v1/integrations/payments`                          | Stripe, Adyen. Webhook URLs auto-provisioned.    |
| Shipping provider       | `/api/v1/integrations/shipping`                          | FedEx, UPS, DHL — credentials encrypted per-tenant. |
| E-invoicing (per country)| `/api/v1/integrations/einvoicing`                       | Country-specific (Italy SDI, France Chorus Pro, etc). |
| Webhooks (outbound)    | `/api/v1/webhooks`                                       | Per-event subscriptions; HMAC signing. See `migrations/000048_webhook_v2.sql`. |

Test a webhook delivery:

```bash
curl -s -XPOST -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"event":"deal.created"}' \
  https://api.kapp.example.com/api/v1/webhooks/{id}/test
```

---

## 11.5 Reporting and Insights

Insights schema in `migrations/000038_insights.sql` and family.

- **Visual query builder** (`apps/web/src/pages/insights/builder`) —
  point-and-click, generates SQL under the hood.
- **Dashboard creation** — drag-drop widgets backed by saved queries
  (`insights_queries`).
- **Scheduled reports** — `report_schedules`
  (`migrations/000033_report_schedules.sql`); cron + recipients.
- **SQL editor** (power users) — gated by feature flag
  `insights.sql_mode` (`migrations/000045_insights_sql_mode.sql`).
  Safety constraints:
  - `SET LOCAL app.tenant_id` is injected automatically.
  - Read-only role (`kapp_insights_ro`).
  - Statement timeout: 30 s.
  - Memory limit per query: 256 MB (`SET LOCAL work_mem`).
- **Embedded dashboards** — `insights_embeds`
  (`migrations/000044_insights_embeds.sql`); generate a signed
  embed URL via `POST /api/v1/insights/embeds`.

---

## 11.6 Backup and Export

**On-demand tenant data export**:

```http
POST /api/v1/admin/exports
{
  "tenant_id": "<UUID>",
  "format":    "jsonl",      // or "csv"
  "scope":     ["krecords", "audit_log", "files"]
}
```

Returns an `export_id`; poll `GET /api/v1/admin/exports/{id}` for
completion. The output lands in the tenant's ZK Fabric bucket or — if
the tenant uses the global MinIO fallback — under
`s3://kapp-exports/<tenant_id>/<export_id>.tar.gz` with a 7-day TTL.

**Scheduled exports**:

```http
POST /api/v1/admin/export-schedules
{
  "tenant_id": "<UUID>",
  "cron":      "0 3 * * 0",            # Sundays at 03:00
  "format":    "jsonl",
  "scope":     ["krecords", "audit_log"],
  "destination": "s3://customer-bucket/kapp-backup/"
}
```

**Data formats**:

- **JSONL** — one record per line; `_table` field identifies the
  source. Matches `services/kapp-backup` output for round-trip
  compatibility.
- **CSV** — one file per table inside a tarball.

Round-trip: an exported JSONL can be restored into a different
tenant_id via `services/kapp-backup restore --remap`.
