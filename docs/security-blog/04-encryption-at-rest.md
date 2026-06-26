# Encryption: Per-Tenant Keys from One Master Key

**Tenant:** Acme Corp + Globex GmbH
**Persona:** a buyer asking "if your database backup leaks, is my salary data exposed?"

RLS (post 1) stops one tenant from reading another's rows through the
application. It does **not** protect a database backup, a disk snapshot, or a
replica: all of those contain plaintext. Kapp therefore encrypts sensitive
fields *before* they hit the database, with a key derived per tenant, so a
stolen backup is ciphertext without the per-tenant keys.

## One master key, derived per tenant

The platform holds one `KAPP_MASTER_KEY` (32+ bytes, stored in KMS in
production — never in `.env`). For each tenant, a 32-byte AES-256 key is
derived with HKDF-SHA256, using the tenant UUID as the salt and a fixed
context label:

```go
var infoLabel = []byte("kapp.krecord.field.v1")

func DeriveKey(masterKey []byte, tenantID uuid.UUID) ([]byte, error) {
    salt := tenantID[:]
    r := hkdf.New(sha256.New, masterKey, salt, infoLabel)
    out := make([]byte, 32) // AES-256
    io.ReadFull(r, out)
    return out, nil
}
```

Derivation is deterministic: the same `(masterKey, tenantID)` always produces
the same key, so the `KeyManager` can re-derive on a cache miss without
coordinating with any other component. The label is domain-separated from the
audit-HMAC and blind-index labels (different HKDF `info`), so a leaked field
key cannot be confused with an audit key or a search-index key.

## AES-256-GCM with an opaque ciphertext prefix

Encryption uses AES-GCM with a fresh random nonce per value. The ciphertext is
base64-encoded and prefixed with `kapp:enc:v1:` so the store can distinguish
ciphertext from legacy plaintext without a schema migration — anything without
the prefix is returned verbatim (the dev fallback when no master key is set).

The demo registers an `hr.employee` KType with a confidential `salary`
(`encrypted: true`) and a secret `ssn` (classified `secret`, which is treated
as sensitive even without the legacy flag). The seeding program writes the
records through the real `record.PGStore.WithEncryptor(km)` path. Here is what
actually landed in `krecords.data` under Acme's tenant context:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000001';
kapp_app=> SELECT jsonb_build_object(
              'full_name', data->'full_name',
              'department', data->'department',
              'salary_ciphertext', data->'salary',
              'ssn_ciphertext', data->'ssn') AS data
            FROM krecords ORDER BY created_at LIMIT 1;
                                  data
----------------------------------------------------------------------
 {"full_name": "Priya Shah",
  "department": "Engineering",
  "ssn_ciphertext": "kapp:enc:v1:9kBg3XuDcVHAyAyNODx+6nkxyjTpjogp8fSurrcKubbnl8RnpOXy",
  "salary_ciphertext": "kapp:enc:v1:O51XISeD08ZWp/4DY9xRARYpC4eyiVqQiWh3g8su6OdK"}
```

`full_name` and `department` are stored as plaintext (they are public-class
fields). `salary` and `ssn` are stored as `kapp:enc:v1:` ciphertext. The API
decrypts transparently on read for authorised roles, so application code never
sees the prefix.

## Per-tenant key isolation, proven

Because the key is derived from the tenant UUID, Acme and Globex get different
keys. The same plaintext salary produces different ciphertext under each
tenant (and different ciphertext on every write, thanks to the random nonce).
Globex's one employee, queried under Globex's context:

```
kapp_app=> SET LOCAL app.tenant_id = '00000000-0000-4000-8000-000000000002';
kapp_app=> SELECT jsonb_build_object('full_name', data->'full_name,
                                     'salary_ciphertext', data->'salary') AS data
            FROM krecords;
                                  data
----------------------------------------------------------------------
 {"full_name": "Lena Fischer",
  "salary_ciphertext": "kapp:enc:v1:YGlPPD+g2fQgYl9qiO797zX9lpuPyf6GdhebGlc+3IGM"}
```

A stolen backup containing both tenants' rows gives the attacker two different
ciphertext blobs; without Globex's derived key, Globex's salary is
unrecoverable even if Acme's key somehow leaks.

## Blind indexes: searching encrypted fields

Encrypting a field makes it unsearchable — you cannot `WHERE salary = 92500`
against ciphertext. For fields marked `indexed: true`, the store writes a
blind index alongside the record: `HMAC(tenant_search_key, canonical(value))`.
The index is a deterministic digest that lets `ListByField` match on the field
without decrypting. The search key is derived under yet another HKDF label
(`kapp.blind.index.v1`), domain-separated from the field-encryption key.

## Fail-closed on a missing master key

In production (`KAPP_ENV=production`), a missing or too-short `KAPP_MASTER_KEY`
is a **fatal boot error** — `tenant.FailClosedOnMissingMasterKey` returns an
error that stops the service. The rationale: a production deployment without a
master key cannot decrypt tenant credentials or encrypted fields, so booting
would silently degrade into a broken state. Outside production the helper
returns nil so dev keeps working without secrets plumbing.

## Files: the ZK Object Fabric

Attachments get a stronger isolation story than the database. The ZK Object
Fabric is a multi-tenant zero-knowledge S3-compatible gateway: each tenant is
provisioned its own HMAC credentials and bucket, and the gateway encrypts each
object under a per-tenant DEK. The platform operator (or anyone with raw
access to the storage backend) cannot read tenant objects without the tenant's
credentials. The setup wizard provisions these credentials during tenant
onboarding.

![Setup wizard](../screenshots/00-setup-wizard.png)

## In transit

External endpoints require TLS 1.3 minimum (cert-manager + ACME). Database
connections use `sslmode=verify-full` with CA validation in production (the dev
compose stack uses `sslmode=disable` — never in prod). NATS uses TLS with
client certificates. Internal service-to-service mTLS via a service mesh is on
the roadmap (post 8).

## What encryption does not change

The API must decrypt to serve an authorised request, so the default SME tier
cannot truthfully promise "no one at the vendor can ever read your data." The
honest version — and the higher tiers that *do* change this — is in post 8.
Encryption here is about **at-rest confidentiality against backup/disk
exfiltration and per-tenant key isolation**, not about making the application
blind to its own data.

The next post covers what happens to the plaintext the API *does* see when it
writes an audit entry: it gets redacted and hash-chained.
