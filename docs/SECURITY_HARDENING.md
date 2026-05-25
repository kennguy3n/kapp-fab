# Security Hardening Guide

Pre-deploy checklist and continuous-hardening practices for a Kapp
production deployment. Encryption design lives in
[SECURITY_REVIEW.md](./SECURITY_REVIEW.md); compliance mapping in
[COMPLIANCE.md](./COMPLIANCE.md); auth conventions in
[API_REFERENCE.md §12.5](./API_REFERENCE.md#125-authentication).

---

## 17.1 Pre-Deployment Checklist

Run through the list before promoting a build to production. Tick
each box only after verification — not because it "should" be true.

### Secrets

- [ ] `KAPP_JWT_SECRET` rotated within the last 90 days.
- [ ] `KAPP_MASTER_KEY` stored in KMS (AWS / GCP / Vault), not in
      `.env` or Kubernetes secrets.
- [ ] `DB_URL`, `ADMIN_DB_URL`, S3 keys, NATS credentials all sourced
      from the secret manager — verify no hard-coded values in
      `deploy/helm/` or `deploy/k8s/`.
- [ ] No `.env`, `.env.local`, or credential JSON committed to git
      (CI gate via `.gitleaks.toml`).
- [ ] Container images don't bake secrets:
      ```bash
      docker history ghcr.io/kennguy3n/kapp-fab/api:v0.1.1 \
        | grep -iE 'jwt|secret|key|password'
      ```

### Database roles

- [ ] `kapp_app` is the application role — NO BYPASSRLS.
      Verify:
      ```sql
      SELECT rolname, rolbypassrls FROM pg_roles
       WHERE rolname IN ('kapp_app', 'kapp_admin');
      -- kapp_app  → false
      -- kapp_admin → true
      ```
- [ ] `kapp_admin` is used only by control-plane endpoints (cells
      registry, tenant lifecycle, RLS-bypassed forensic reads).
- [ ] `kapp_tier_admin` is granted EXECUTE on
      `promote_tenant_to_schema` only.

### Authentication

- [ ] `KAPP_JWT_ISSUER` and `KAPP_JWT_AUDIENCE` set to production
      values (not `kapp-dev`).
- [ ] Access TTL ≤ 15 min; refresh TTL ≤ 7 days.
- [ ] `KAPP_ALLOW_DEV_JWT_SECRET=false` (or unset).
- [ ] `KAPP_AUTHZ_ENFORCE=true` — fail-closed when the policy engine
      can't decide.
- [ ] `KAPP_REQUIRE_JWT=true` for all production paths.

### RLS

- [ ] Every tenant-scoped table has a `tenant_isolation` policy.
      `.github/workflows/migration-rls-check.yml` enforces this in
      CI; verify in production:
      ```sql
      SELECT relname,
             (SELECT count(*) FROM pg_policies
              WHERE tablename = relname AND policyname = 'tenant_isolation')
             AS policies
      FROM pg_class
      WHERE relkind = 'r' AND relname IN (
        'krecords','events','audit_log','journal_lines','inventory_moves',
        'leave_ledger','workflows','workflow_runs','approvals',
        'accounts','journal_entries','files','sessions','user_tenants'
      );
      -- Every row's policies count must be >= 1.
      ```
- [ ] Application connects as `kapp_app` (RLS enforced).

### Container images

- [ ] All container images use a non-root user
      (`USER 65534:65534` in Dockerfile).
- [ ] Read-only root filesystem
      (`securityContext.readOnlyRootFilesystem: true`).
- [ ] Dropped Linux capabilities
      (`securityContext.capabilities.drop: [ALL]`).
- [ ] Tagged with the immutable digest, not a moving tag like
      `latest`.
- [ ] Signed via cosign — verify on pull:
      ```bash
      cosign verify ghcr.io/kennguy3n/kapp-fab/api:v0.1.1 \
        --certificate-identity-regexp 'https://github.com/kennguy3n/kapp-fab' \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com
      ```

### Network

- [ ] Kubernetes `NetworkPolicy` default-deny in the namespace (see
      [INFRASTRUCTURE.md §15.3](./INFRASTRUCTURE.md#153-network-policies)).
- [ ] Database port 5432 is **only** reachable from PgBouncer.
- [ ] Egress allowlist enforced
      ([INFRASTRUCTURE.md §15.7](./INFRASTRUCTURE.md#157-egress-allowlist)).
- [ ] TLS 1.3 minimum on every external endpoint (`openssl s_client`
      check).

---

## 17.2 Runtime Security Monitoring

### Failed authentication detection

```sql
-- Failed logins spike for a single account → password spray
SELECT actor_email,
       count(*),
       min(created_at) AS first_attempt,
       max(created_at) AS last_attempt
FROM audit_log
WHERE action     = 'auth.failure'
  AND created_at > now() - interval '15 minutes'
GROUP BY actor_email
HAVING count(*) > 20
ORDER BY count DESC;
```

Auto-lock the user after 20 failures / 15 minutes:

```sql
UPDATE users
SET locked_at      = now(),
    locked_reason  = 'auto:password-spray'
WHERE id IN (
  SELECT u.id FROM users u
  JOIN audit_log a ON a.actor_id = u.id
  WHERE a.action = 'auth.failure'
    AND a.created_at > now() - interval '15 minutes'
  GROUP BY u.id HAVING count(*) > 20
);
```

### RLS bypass detection

Any non-empty result is a SEV-1:

```sql
-- Audit-log entries where tenant_id != the actor's tenant
SELECT a.id, a.created_at, a.tenant_id, a.actor_id, ut.tenant_id AS actor_tenant
FROM audit_log a
JOIN user_tenants ut ON ut.user_id = a.actor_id
WHERE a.created_at > now() - interval '1 hour'
  AND a.tenant_id != ut.tenant_id;
```

### Rate-limit hits

```promql
# Top-N tenants by 429 rate (last 5m)
topk(10,
  sum by (tenant_id) (
    rate(kapp_request_total{status="429"}[5m])
  )
)
```

Investigate any tenant sustaining > 50 RPS of 429s — either the
tenant is being abused or their integration is misbehaving.

### Admin access patterns

```sql
-- Every action taken against admin endpoints in the last 24h
SELECT a.created_at, a.actor_id, a.action, a.target_type, a.target_id,
       a.payload->>'reason' AS reason
FROM audit_log a
WHERE a.action LIKE 'admin.%'
  AND a.created_at > now() - interval '24 hours'
ORDER BY a.created_at DESC;
```

Action items: any `admin.tenant.destroy`, `admin.user.impersonate`,
or `admin.session.terminate` should map back to a Jira / Linear
ticket. Otherwise it's anomalous.

---

## 17.3 Dependency Security

### Go modules

```bash
# Known vulnerabilities — run in CI and locally before release:
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...
# Exit non-zero blocks the deploy.
```

### Node modules

```bash
# In apps/web:
npm audit --omit=dev --audit-level=moderate
# Exit non-zero blocks the deploy.
```

### Container scanning

Trivy is the default scanner in `.github/workflows/supply-chain.yml`:

```bash
trivy image --severity HIGH,CRITICAL --exit-code 1 \
  ghcr.io/kennguy3n/kapp-fab/api:v0.1.1
```

### SBOM

Every release artefact emits a CycloneDX SBOM and an SLSA Level 3
provenance attestation. Verify on consumption:

```bash
cosign verify-attestation \
  --type cyclonedx \
  --certificate-identity-regexp 'https://github.com/kennguy3n/kapp-fab' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/kennguy3n/kapp-fab/api:v0.1.1
```

---

## 17.4 Secrets Rotation Schedule

| Secret                         | Frequency         | Procedure                                                                                          |
| ------------------------------ | ----------------- | -------------------------------------------------------------------------------------------------- |
| `KAPP_JWT_SECRET`              | 90 days           | Rotate via JWT keyring: set `KAPP_JWT_PRIMARY_REF=<new>`, keep `<old>` in `KAPP_JWT_VERIFY_REFS` for 1 token TTL window; remove after grace period. Rolling restart picks up the change. |
| `KAPP_MASTER_KEY`              | Annual            | Per [DR_RUNBOOK.md §4](./DR_RUNBOOK.md): set `KAPP_MASTER_KEY_PREV=<old>`, set `KAPP_MASTER_KEY=<new>`, deploy. Worker decrypts with PREV, re-encrypts with new on next write. Verify with a sweep job. |
| Database `kapp_app` password    | 90 days           | `ALTER ROLE kapp_app PASSWORD '...'` on primary; update K8s secret; rolling restart. |
| Database `kapp_admin` password  | 90 days           | Same as above for `kapp_admin`.                                                                     |
| Per-tenant ZK Fabric keys      | On compromise     | Operator regenerates via the ZK Fabric console; updates the tenant row.                             |
| TLS certificates               | 90 days (auto)    | cert-manager + Let's Encrypt. Alert at < 7 days remaining.                                          |
| KChat API key                   | 6 months          | Re-issue in KChat, update `KCHAT_API_KEY`, rolling restart bridge.                                  |
| S3 access keys                  | 90 days           | Create new IAM key pair → update K8s secret → restart pods → deactivate old key after one cycle.   |
| Webhook signing secrets        | 6 months          | Per-tenant; rotated via `POST /api/v1/webhooks/{id}/rotate-secret`.                                |
| NATS credentials                | 6 months          | Re-issue NKey pair; redeploy NATS server config; rolling restart clients.                          |

---

## 17.5 Pod Security Standards

Enforce the `restricted` Pod Security Standard:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: kapp
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit:   restricted
    pod-security.kubernetes.io/warn:    restricted
```

Pod manifest baseline:

```yaml
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot:    true
    runAsUser:       65534
    runAsGroup:      65534
    fsGroup:         65534
    seccompProfile:  { type: RuntimeDefault }
  containers:
    - name: api
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem:   true
        capabilities: { drop: [ALL] }
      resources:
        limits:    { cpu: "1", memory: "1Gi" }
        requests:  { cpu: "250m", memory: "256Mi" }
```

---

## 17.6 Audit and Threat-Hunting Queries

```sql
-- Users with sessions in the last 24h from > 5 distinct IPs
SELECT user_id, count(DISTINCT ip_address) AS distinct_ips
FROM sessions
WHERE created_at > now() - interval '24 hours'
GROUP BY user_id HAVING count(DISTINCT ip_address) > 5
ORDER BY distinct_ips DESC;

-- KType registrations / updates in the last 7 days (should match the
-- KType registration audit trail)
SELECT created_at, payload->>'name', payload->>'version', payload->>'actor_id'
FROM audit_log
WHERE action = 'ktype.register'
  AND created_at > now() - interval '7 days'
ORDER BY created_at DESC;

-- Workflows that have been moved straight from "draft" to "completed"
-- without an intermediate state (potential approval bypass)
SELECT id, workflow_name, state, history->-1, history->-2
FROM workflow_runs
WHERE state = 'completed'
  AND (history->-2)->>'state' = 'draft';
```

Investigate any unexpected result.

---

## 17.7 Incident Pre-Wiring

Pre-wire the following so an incident doesn't waste time on plumbing:

- A `security-readonly` Postgres role with `BYPASSRLS` for emergency
  forensic queries (kept in a sealed-vault credential, not in the
  app cluster).
- A standing PagerDuty service `kapp-security` with a published
  runbook URL pointing at [INCIDENT_RESPONSE.md §3.5](./INCIDENT_RESPONSE.md#35-evidence-preservation-security).
- Object-lock-enabled S3 bucket `s3://kapp-evidence` with COMPLIANCE
  mode and 7-year retention.
- A backup of `KAPP_MASTER_KEY` in an offline custodian-managed
  vault (e.g. AWS KMS multi-region with manual-only deletion).
