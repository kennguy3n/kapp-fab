# Data Retention & Privacy: GDPR, Erasure, Residency, Crypto-Shredding

**Tenant:** Acme Corp (US) + Globex GmbH (EU)
**Persona:** a DPO at Globex asking "can you extract, erase, and keep our data in the EU?"

Encryption and isolation protect data *while it is in use*. Privacy is about
the data's **lifecycle**: how long it is kept, where it is kept, who counts as
the controller vs. the processor, and how a tenant admin exercises a data
subject's rights. Kapp maps these to concrete tables and worker jobs, not
policy prose.

## Controller vs. processor

Kapp's compliance model is explicit about roles:

- The **tenant admin** is the **data controller** — they decide why and how
  their users' data is processed.
- The **operator** (whoever deploys `kapp-fab`) is the **data processor**.
- Sub-processors (cloud provider, object-storage provider, transactional
  email) are enumerated in the DPA template.

This split matters because it determines who is on the hook for a DSAR: the
tenant admin initiates it, the operator provides the extraction tooling.

## Data Subject Access Request (DSAR)

A tenant admin submits a DSAR for one of their users. The operator extracts
via `kapp-backup`:

```bash
go run ./services/kapp-backup extract \
  --db "$KAPP_ADMIN_DB_URL" --tenant "$TENANT_ID" \
  --user "$USER_ID" --format json --out /tmp/dsar_${USER_ID}.json
```

The SLA is 30 days (GDPR Art. 12(3)). The extraction runs under the admin pool
and is scoped to the tenant + user, so the output contains only that subject's
footprint across `krecords`, `audit_log`, `sessions`, and `user_tenants`.

## Right to erasure

Erasure is deliberately *not* a blanket `DELETE`. Regulatory retention applies
to the audit log, so the procedure anonymises rather than destroys audit rows:

```sql
-- Anonymise audit entries (retain timestamps + event types for compliance)
UPDATE audit_log
SET actor_id = NULL,
    payload  = jsonb_set(payload, '{actor_email}', '"ANONYMIZED"')
WHERE actor_id = $1 AND tenant_id = $2;

-- Sessions and memberships are deleted outright
DELETE FROM sessions     WHERE user_id = $1;
DELETE FROM user_tenants WHERE user_id = $1 AND tenant_id = $2;

-- The user row is deleted only if no other tenant membership remains
DELETE FROM users WHERE id = $1
  AND NOT EXISTS (SELECT 1 FROM user_tenants WHERE user_id = $1);
```

Full tenant erasure (service termination) is the
`/api/v1/admin/tenants/{id}/destroy` flow — a separate, audited, hard-deleting
procedure documented in the DR runbook.

## Retention policies: per-tenant, per-category

Migration `000032` introduces `data_retention_policies` — one row per
`(tenant, category)`. The retention sweeper (a background worker) deletes rows
older than `retention_days` for each enabled category and emits an
`audit_log` entry for every sweep:

```sql
CREATE TABLE data_retention_policies (
    tenant_id      UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    category       TEXT NOT NULL,
    retention_days INT  NOT NULL CHECK (retention_days BETWEEN 1 AND 3650),
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (tenant_id, category)
);
```

The demo seeds Acme with a 7-year audit floor and shorter windows for chatty
tables:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000001';
kapp_app=> SELECT category, retention_days, enabled FROM data_retention_policies ORDER BY category;
      category      | retention_days | enabled
--------------------+----------------+---------
 audit_log          |           2555 | t       -- 7 years (regulatory floor)
 events             |             90 | t
 notifications      |           30   | t
 webhook_deliveries |           30   | t
```

The `audit_log` category has a hard floor: it is never configurable below 7
years. Globex, on the business plan, keeps audit for 10 years and events for
180 days — the same table, different policy rows, isolated by RLS.

![Retention policies](../screenshots/13-admin-retention.png)

## Data residency by cell

Each cell carries a `region` column. Tenants requiring EU residency are placed
in EU-region cells via the placement policy, and the setup wizard surfaces
region selection during onboarding. The demo models this directly: Acme sits
in `us-east-1` (US), Globex in `eu-central-1` (EU):

```
kapp_admin=> SELECT slug, name, base_currency, country
             FROM tenants ORDER BY slug;
  slug  |    name     | base_currency | country
--------+-------------+---------------+---------
 acme   | Acme Corp   | USD           | US
 globex | Globex GmbH | EUR           | DE
```

Residency is an *operational* control: the operator commits to provisioning EU
tenants only on EU cells, and the cell's `region` column is the auditable
evidence of that commitment. Cross-cell data movement is governed by the
multi-region guide.

## Crypto-shredding

For object storage, deletion is crypto-shredding rather than overwrite: when a
file passes its retention window, the ZK Object Fabric drops the per-tenant
DEK. The ciphertext blobs become permanently unreadable even if a backup of
the raw object store survives — the key is gone, not the bytes. This is the
file-storage analogue of the database's per-tenant key isolation (post 4):
destroying the key is equivalent to destroying the data.

## What privacy controls do not do

They do not make Kapp a turnkey GDPR product. The operator still owns the DPA,
the sub-processor list, the breach-notification process, and the residency
*enforcement* (Kapp exposes the knobs; the operator configures them correctly).
The compliance doc is operator-facing; the auditor-facing evidence pack
(`kapp-cli compliance pack`) is on the v0.2 roadmap (post 8).

![Tenant management](../screenshots/13-admin-tenants.png)

The next post covers the operational layer that surrounds all of this: rate
limiting, supply-chain security, and break-glass admin access.
