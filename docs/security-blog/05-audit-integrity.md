# Audit & Integrity: An Append-Only, Hash-Chained, Redacted Trail

**Tenant:** Acme Corp
**Persona:** an auditor who must be able to (a) see who changed what, and (b) prove the log itself was not tampered with.

An audit log is only useful if you can trust it. Kapp's `audit_log` is
append-only, **hash-chained per tenant**, and **redacted** so the sensitive
fields encrypted at rest (post 4) never reappear as plaintext in the audit
trail or the event pipeline.

## Append-only and transactional

Every tenant-scoped mutation writes one `audit_log` entry inside the *same
transaction* as the mutation — so the entry is durable if and only if the
mutation commits. There is no window where a row is changed but no audit
record exists. The table is partitioned by `tenant_id` range, like the other
write-heavy tenant tables.

## The hash chain: tamper-evidence, not just append-only

Append-only is not enough. If an attacker compromises the database, they can
`DELETE` or `UPDATE` past audit rows and leave no evidence. Migration
`000016_audit_hash_chain.sql` adds two columns:

```sql
ALTER TABLE audit_log ADD COLUMN prev_hash BYTEA;
ALTER TABLE audit_log ADD COLUMN row_hash  BYTEA;
```

Each row's `row_hash` is `SHA-256(prev_hash || tenant_id || target_id ||
action || before || after || context || created_at)`, and `prev_hash` stores
the previous row's `row_hash` for the same tenant. The first row per tenant
has a NULL `prev_hash`, interpreted as a 32-byte zero seed. The logger
serialises the `(fetchPrevHash, INSERT)` pair with a transaction-scoped
advisory lock keyed on the tenant UUID so concurrent writers cannot fork the
chain.

The demo wrote five Acme audit entries through the real `audit.PGLogger`. Here
is the chain, queried under Acme's tenant context:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000001';
kapp_app=> SELECT id, action, actor_kind, encode(row_hash,'hex') AS row_hash
            FROM audit_log ORDER BY id;
 id |         action         | actor_kind |                             row_hash
----+------------------------+------------+------------------------------------------------------------------
  1 | hr.employee.create     | user       | 77a813bc2cc9dca2e0b5a29881308659adb6593da8476cafa1784dcb6b3cc80b
  3 | hr.employee.create     | user       | 680dedf1482d758ec6b7e841adaeea398d602c6d9ca0c4a09f93cf805f789637
  4 | auth.failure           | user       | a430e5232ce7c61fb624278f0d844e1aff403d8b79a72732bd68f8768e5b563e
  5 | admin.tenant.suspend   | system     | 2da34e7c72ff623437050f4201ee5b9a0f43ef6bbb941b394adfacedd55e21e7
  6 | retention.sweep.delete | system     | b06cffdfbf19a7064a09a80241b71cf8a53951ad02a1c607134835eea5225c20
```

(Note `id` jumps 1 → 3: row `id=2` belongs to Globex and is invisible under
Acme's RLS context — another isolation proof.)

A verifier scans the table in `(tenant_id, id)` order and checks that each
row's `prev_hash` equals the prior row's `row_hash`. Any `UPDATE`/`DELETE` of a
past row breaks the chain at that point. We can check the linkage directly
with a window query:

```
kapp_app=> SELECT id, action,
                  (prev_hash IS NULL OR prev_hash = lag(row_hash) OVER (ORDER BY id)) AS chain_ok
            FROM audit_log ORDER BY id;
 id |         action         | chain_ok
----+------------------------+----------
  1 | hr.employee.create     | t
  3 | hr.employee.create     | t
  4 | auth.failure           | t
  5 | admin.tenant.suspend   | t
  6 | retention.sweep.delete | t
```

Every link verifies. If row 3 were edited, its `row_hash` would change, row 4's
`prev_hash` would no longer match, and `chain_ok` would flip to `f` from row 4
onward — pinpointing the first tampered row.

## Redaction: plaintext never reaches the audit row

Here is the subtle attack the hash chain does *not* stop: an attacker reads
the audit log itself and harvests sensitive fields that were logged in
plaintext. Kapp closes this with a single chokepoint — `record.RedactData` —
that every payload destined for an audit row passes through. Sensitive fields
(schema `encrypted: true` or classified `confidential`/`secret`) are replaced
with `<redacted>`, and a `_redacted` array records *which* fields were
redacted so the trail stays human-readable without carrying the values.

The actual `after` payload of the first employee-create audit row:

```
kapp_app=> SELECT jsonb_pretty(after) AS after_payload FROM audit_log WHERE id = 1;
                       after_payload
----------------------------------------------------------
 {
     "ssn": "<redacted>",
     "salary": "<redacted>",
     "_redacted": [
         "salary",
         "ssn"
     ],
     "full_name": "Priya Shah",
     "department": "Engineering"
 }
```

`salary` and `ssn` are gone; `full_name` and `department` (public fields)
survive. The audit trail tells you *that* a salary was set and *which* fields
were sensitive, without ever containing the number.

## HMAC digests: proving a sensitive field changed

Redaction creates a new problem: if `salary` is always `<redacted>`, how does
an auditor confirm a salary actually *changed* between two versions of a
record? The `DiffSummary` helper computes per-field HMAC-SHA256 digests of the
plaintext value (under the domain-separated `kapp.audit.hmac.v1` key) and
writes them as `salary_hash_before` / `salary_hash_after`. A verifier can
compare the digests to confirm the value changed (or did not) without ever
seeing the value.

## The event outbox is redacted too

Audit is not the only place plaintext could leak. The `events` outbox feeds
SSE streams, webhooks, notifications, and worker logs — all of which would
re-expose sensitive data if the payload carried it. The store's
`eventSummaryPayload` writes only identity, status, and the redacted diff
summary to the outbox. Here is the real payload of the first `krecord.created`
event:

```
kapp_app=> SELECT jsonb_pretty(payload) FROM events ORDER BY created_at LIMIT 1;
                          payload
-----------------------------------------------------------
 {
     "id": "bcbaaf99-a24e-43dc-ac58-4ce67381b544",
     "kind": "user",
     "actor": "00000000-0000-4000-8000-000000000011",
     "ktype": "hr.employee",
     "status": "active",
     "tenant": "00000000-0000-4000-8000-000000000001",
     "version": 1,
     "snapshot": "create",
     "changed_fields": ["department", "full_name", "salary", "ssn"],
     "redacted_fields": ["salary", "ssn"],
     "salary_hash_after": "SXyO9vuHMc+TJtPSLlOw7nejGrj7JWjMceNYi5oefP4=",
     "ssn_hash_after": "1QEzHq1RhJBhAfSvFBcOX4Zcw5XzobwizJf+DXwIt9g="
 }
```

No plaintext salary or SSN anywhere in the event — only the HMAC digests and
the list of fields that changed. A webhook consumer or an SSE client receives
exactly this redacted shape, so the plaintext-bleed surface is closed across
the whole downstream pipeline.

## The audit surface in the UI

The admin audit-log screen is the operator's view of this trail — filtered,
paginated, and rendered from the same redacted rows.

![Admin audit log](../screenshots/13-admin-audit-log.png)

The next post turns from integrity to privacy: GDPR, erasure, retention, and
data residency.
