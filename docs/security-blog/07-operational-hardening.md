# Operational Hardening: Rate Limits, Supply Chain, and Break-Glass

**Tenant:** platform-wide
**Persona:** the SRE on call, and the operator who must justify their access to an auditor.

The previous posts cover the data plane: isolation, authz, encryption, audit,
privacy. This post covers the *operational* controls that surround the data
plane — the things that keep the platform from being abused, keep the software
supply chain trustworthy, and keep operator access scoped and audited.

## Rate limiting with zero idle cost

Every tenant has a per-tenant rate bucket. The in-process limiter
(`platform.RateLimiter`) and the distributed alternative
(`platform.RedisRateLimiter`) both preserve a key invariant from the
architecture doc: **idle tenants consume zero compute and ~zero memory.** The
bucket map and the LRU metadata cache drop a tenant's entry after
`IdleTimeout`. The Redis backend uses an atomic sliding-window Lua script
(loaded once with `SCRIPT LOAD`, invoked via `EVALSHA` with `EVAL` fallback)
and calls `EXPIRE` on every access so idle keys drop after the timeout — the
zero-idle-cost invariant holds across replicas. Redis outages fail *open*
(return `allowed=true`) so a Redis hiccup does not block every request; the
reverse proxy remains the outer ceiling on abusive traffic.

A tenant sustaining > 50 RPS of 429s is a signal — either abuse or a
misbehaving integration — and is investigated via the per-tenant 429 rate
Prometheus query in the hardening guide.

## Runtime security monitoring

The audit log is not just for after-the-fact forensics; queries against it
power runtime detection. A password-spray detector counts `auth.failure`
entries per actor over a 15-minute window and auto-locks the user after 20
failures:

```sql
SELECT actor_email, count(*)
FROM audit_log
WHERE action = 'auth.failure' AND created_at > now() - interval '15 minutes'
GROUP BY actor_email HAVING count(*) > 20;
```

An RLS-bypass detector flags any audit entry whose `tenant_id` does not match
the actor's tenant — a non-empty result is a SEV-1. Admin access patterns are
reviewed daily: any `admin.tenant.destroy`, `admin.user.impersonate`, or
`admin.session.terminate` must map back to a ticket.

## The admin role split: control plane vs. break-glass

The `kapp_admin` role has `BYPASSRLS` and is used **only** for cross-tenant
control-plane reads (the tenant picker, user-tenant lookups). It is not used
for tenant data-plane traffic — that always runs as `kapp_app` with RLS
enforced. The live role configuration:

```
 rolname     | rolbypassrls | rolsuper
 kapp        | t            | t        -- migrations only
 kapp_admin  | t            | f        -- control-plane reads only
 kapp_app    | f            | f        -- the API; RLS enforced
 kapp_tier_admin | f        | f        -- tenant-tier promotion only
```

The hardening plan (post 8) tracks narrowing this further: splitting
`kapp_admin` into a readonly control-plane role, a maintenance role
(`NOSUPERUSER NOBYPASSRLS` for retention/purge jobs), and a time-boxed
`kapp_breakglass` role that requires a reason code and writes to an immutable
`admin_audit_log`. Break-glass access is the rare legitimate cross-tenant
operation, and the design goal is that every such access is audited and
alerted, not silent.

## Supply-chain security

Every release artefact emits a CycloneDX SBOM and an SLSA Level 3 provenance
attestation. Container images are signed with cosign and verified on pull:

```bash
cosign verify ghcr.io/kennguy3n/kapp-fab/api:v0.1.1 \
  --certificate-identity-regexp 'https://github.com/kennguy3n/kapp-fab' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Dependency vulnerabilities are gated in CI:

- **Go:** `govulncheck ./...` — exit non-zero blocks the deploy.
- **Node:** `npm audit --omit=dev --audit-level=moderate`.
- **Containers:** Trivy scans at `HIGH,CRITICAL` severity with `--exit-code 1`.

No secret is baked into an image — the pre-deploy checklist verifies with
`docker history ... | grep -iE 'jwt|secret|key|password'` and a gitleaks CI
gate rejects committed credentials.

## Container and network hardening

The production container posture:

- Non-root user (`USER 65534:65534`).
- Read-only root filesystem (`securityContext.readOnlyRootFilesystem: true`).
- All Linux capabilities dropped.
- Images tagged by immutable digest, not `latest`.

At the network layer: a Kubernetes `NetworkPolicy` default-deny in the
namespace; the database port 5432 reachable only from PgBouncer; an egress
allowlist; TLS 1.3 minimum on every external endpoint. The metrics listener is
split onto its own port (`KAPP_METRICS_ADDR`) so Prometheus scrapes stay off
the auth chain and do not contend with user-facing latency.

## Secrets rotation

| Secret | Frequency |
|---|---|
| `KAPP_JWT_SECRET` | 90 days (keyring rotation, no restart) |
| `KAPP_MASTER_KEY` | annual (dual-key decrypt window) |
| DB `kapp_app` / `kapp_admin` passwords | 90 days |
| TLS certificates | 90 days (cert-manager, auto) |

Master-key rotation uses the `KAPP_MASTER_KEY_PREV` window: the worker
decrypts with the previous key and re-encrypts with the new key on the next
write, so rotation is online.

## The operational surfaces in the UI

Usage metering and webhook delivery are the two operational surfaces a tenant
admin sees. Both are tenant-scoped and RLS-protected; webhook delivery logs
are themselves subject to the retention sweeper.

![Usage metering](../screenshots/13-admin-usage.png)

![Webhooks](../screenshots/13-admin-webhooks.png)

The final post is the one we owe any honest buyer: what is verified, what is
still open, and the promise we will and will not make.
