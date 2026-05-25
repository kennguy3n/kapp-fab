# Infrastructure and Networking

Reference topology, network policies, TLS configuration, and DNS
strategy for a Kapp production deployment. Provider-specific details
are abstracted; per-cell provisioning automation lives in
[MULTI_CELL_OPERATIONS.md §6.2](./MULTI_CELL_OPERATIONS.md#62-cell-provisioning-automation).

---

## 15.1 Network Topology

```
                          ┌────────────────┐
                          │  Internet      │
                          └────────┬───────┘
                                   │ HTTPS (443) + WSS
                                   ▼
                       ┌─────────────────────┐
                       │ CDN  (CloudFront /  │   ← static assets, API caching for
                       │       Cloudflare)   │     GET endpoints with `Cache-Control`
                       └────────┬────────────┘
                                │
                                ▼
                       ┌─────────────────────┐
                       │ L7 Load Balancer    │   ← terminates TLS, routes by host
                       │  (ALB / Ingress)    │
                       └─────┬──────┬────────┘
                             │      │
              ┌──────────────┘      └──────────────┐
              ▼                                    ▼
   ┌──────────────────┐                  ┌──────────────────┐
   │ API pods         │                  │ SSE pods         │  (optional split listener,
   │ (api.*)          │                  │ (sse.*)          │   KAPP_SSE_ADDR)
   └────────┬─────────┘                  └────────┬─────────┘
            │                                     │
            │   ┌─────────────────────────────────┘
            ▼   ▼
   ┌──────────────────┐                  ┌──────────────────┐
   │ Worker pods      │                  │ KChat-Bridge pods │
   │ (background)     │                  │ (KChat I/O)       │
   └────────┬─────────┘                  └────────┬─────────┘
            │                                     │
   ┌────────┴─────────────────────────────────────┴──────────┐
   │                                                          │
   ▼                                                          ▼
┌────────────────┐    ┌──────────────────┐    ┌─────────────────────┐
│ PgBouncer      │    │ Redis cluster    │    │ NATS cluster        │
│ (transaction)  │    │ (cache + buckets)│    │ (events + JetStream)│
└──────┬─────────┘    └──────────────────┘    └─────────────────────┘
       │
       ▼
┌────────────────┐    ┌──────────────────┐
│ Postgres primary│   │ Postgres replicas │
│ (HA, multi-AZ) │   │ (1..N for reads)  │
└────────────────┘   └──────────────────┘

                              │
                              ▼
                 ┌────────────────────────┐
                 │ Object storage         │
                 │ (ZK Fabric / S3 / GCS) │
                 └────────────────────────┘
```

Per-cell isolation:

- Each cell lives in its **own Kubernetes namespace** (`kapp-cell-<id>`).
- Each cell has its **own Postgres cluster** — no cross-cell DB
  queries.
- Per-cell DNS suffixes:
  `api.<cell-id>.<root>`, `sse.<cell-id>.<root>`, `portal.<cell-id>.<root>`.

---

## 15.2 Bandwidth Budget

| Link                                       | Sustained (per 1k tenants) | Notes                                                         |
| ------------------------------------------ | -------------------------- | ------------------------------------------------------------- |
| CDN ⇄ Internet                              | 50 Mbps                    | Mostly cached GETs for static assets.                          |
| LB ⇄ API pods                               | 100 Mbps                   | Compressed JSON; gzip middleware on the API.                   |
| API ⇄ PgBouncer                            | 200 Mbps                   | Compressed wire protocol; partition pruning shrinks payloads.  |
| API ⇄ NATS                                 | 50 Mbps                    | Outbox publish + SSE fan-out.                                   |
| Worker ⇄ Postgres                          | 100 Mbps                   | Outbox drain + scheduled-action queries.                        |
| Postgres primary ⇄ replicas (WAL)          | 50 Mbps                    | Provision 3× sustained; 10× burst headroom for bulk loads.     |

Verify ingress bytes hourly:

```bash
kubectl -n kapp top pod -l app=api --containers --use-protocol-buffers
```

---

## 15.3 Network Policies

Kubernetes `NetworkPolicy` resources enforce default-deny inside the
namespace. Ship the following baseline (`deploy/k8s/networkpolicy.yaml`):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: api-ingress
  namespace: kapp
spec:
  podSelector:
    matchLabels: { app: api }
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels: { app: ingress-nginx }
      ports:
        - port: 8080
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: api-egress
  namespace: kapp
spec:
  podSelector:
    matchLabels: { app: api }
  policyTypes: [Egress]
  egress:
    - to:
        - podSelector:
            matchLabels: { app: pgbouncer }
      ports: [{port: 6432}]
    - to:
        - podSelector:
            matchLabels: { app: nats }
      ports: [{port: 4222}]
    - to:
        - podSelector:
            matchLabels: { app: redis }
      ports: [{port: 6379}]
    # KChat egress
    - to:
        - namespaceSelector: { matchLabels: { name: kchat } }
      ports: [{port: 443}]
    # DNS
    - to:
        - namespaceSelector: { matchLabels: { name: kube-system } }
          podSelector: { matchLabels: { k8s-app: kube-dns } }
      ports: [{port: 53, protocol: UDP}]
```

Mirror the same shape for `worker`, `kchat-bridge`, `pgbouncer`,
`nats`, and `redis`. The deny-by-default posture forces explicit
opt-in for every new pod-to-pod link.

`pgbouncer` is the only pod with egress to Postgres (port 5432). API
and worker can never reach Postgres directly.

---

## 15.4 TLS Configuration

**External:**

- TLS 1.3 minimum, TLS 1.2 only with HIGH cipher suites
  (`ECDHE-ECDSA-AES256-GCM-SHA384`, `ECDHE-RSA-AES256-GCM-SHA384`).
- HSTS: `max-age=31536000; includeSubDomains; preload`.
- Certificate auto-renewal via cert-manager. Alert at < 7 days
  remaining (`KappCertExpiringSoon`).

```yaml
# deploy/k8s/cert.yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: kapp-api-tls
  namespace: kapp
spec:
  secretName: kapp-api-tls
  issuerRef: { name: letsencrypt, kind: ClusterIssuer }
  dnsNames:
    - api.kapp.example.com
    - sse.kapp.example.com
    - portal.kapp.example.com
  duration:    2160h    # 90 days
  renewBefore: 720h     # 30 days
```

**Internal:**

- mTLS service-to-service via Istio / Linkerd **(planned)**;
  baseline is TLS without client certs.
- Pod-to-pod traffic stays inside the namespace; the deny-by-default
  network policy prevents accidental cross-namespace traffic.

**Database SSL:**

- Application → PgBouncer: TLS 1.3, server cert validation
  (`sslmode=verify-full`).
- PgBouncer → Postgres: TLS 1.3, client certs for PgBouncer; the API
  itself is authenticated by Postgres role and trusted by PgBouncer.
- Set `DB_URL` with `sslmode=verify-full&sslrootcert=...` in every
  production deployment. The Makefile DSN uses `sslmode=disable` —
  do not copy it to production.

Verification:

```bash
# Verify TLS version + cipher on the API:
openssl s_client -connect api.kapp.example.com:443 -tls1_3 < /dev/null 2>/dev/null \
  | grep -E 'Protocol|Cipher'

# Verify Postgres requires SSL:
psql "host=postgres user=kapp dbname=kapp sslmode=require" -c "SHOW ssl;"
```

---

## 15.5 DNS and Service Discovery

**External DNS** (per cell):

| Hostname                              | Use case                                    |
| ------------------------------------- | ------------------------------------------- |
| `api.kapp.example.com`                | Default cell's API endpoint                  |
| `api.<cell-id>.kapp.example.com`      | Per-cell pinned API endpoint                 |
| `sse.<cell-id>.kapp.example.com`      | Server-Sent-Events listener (separate timeouts) |
| `portal.<cell-id>.kapp.example.com`    | Customer-facing portal                       |
| `s3.<cell-id>.kapp.example.com`       | ZK Fabric S3-compatible endpoint per cell    |
| `app.kapp.example.com`                | Web client (apps/web build)                  |

**Internal DNS** (Kubernetes CoreDNS):

| Service                                          | Use case                                       |
| ------------------------------------------------ | ---------------------------------------------- |
| `api.kapp-cell-a.svc.cluster.local:8080`         | API service inside cell-a                       |
| `nats.kapp-cell-a.svc.cluster.local:4222`        | NATS client connections                         |
| `pgbouncer.kapp-cell-a.svc.cluster.local:6432`   | Postgres entrypoint                             |
| `redis.kapp-cell-a.svc.cluster.local:6379`       | Redis primary; sentinels at 26379              |

**Multi-cell DNS routing:**

The platform router resolves tenants to cells via the `cells` and
`tenants.cell_id` columns at startup, refreshed every 60 s. Requests
without a cell-pinned host fall through to the default cell, which
issues a 308 redirect to the correct cell URL when the tenant is on
another cell.

---

## 15.6 Load Balancer Configuration

Recommended Nginx ingress annotations:

```yaml
metadata:
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout:    "60"   # CRUD
    nginx.ingress.kubernetes.io/proxy-send-timeout:    "60"
    nginx.ingress.kubernetes.io/proxy-body-size:       "32m"  # file uploads
    nginx.ingress.kubernetes.io/limit-rps:              "100"  # per-IP burst protection
    nginx.ingress.kubernetes.io/limit-burst-multiplier: "5"
```

For the SSE listener (`sse.<cell-id>...`), use a separate ingress
with `proxy-read-timeout: 0` and `proxy-buffering: "off"`. The
service binds to `KAPP_SSE_ADDR` and disables write timeouts (see
`services/api/sse_routes.go`).

WebSocket / SSE upgrades:

```yaml
nginx.ingress.kubernetes.io/configuration-snippet: |
  proxy_set_header Upgrade $http_upgrade;
  proxy_set_header Connection "upgrade";
```

---

## 15.7 Egress Allowlist

The default-deny egress policy permits only:

| Target                                   | Purpose                                         |
| ---------------------------------------- | ----------------------------------------------- |
| `*.kchat.example.com`                    | KChat API + SSO                                  |
| `api.stripe.com`, `api.adyen.com`        | Payment gateways                                 |
| `api.plaid.com`, `api.codat.io`           | Bank connections                                |
| Object-storage endpoint for the cell     | File store                                       |
| `*.googleapis.com`, `*.amazonaws.com`    | KMS / Secrets Manager / S3 / object storage     |
| OTLP endpoint (Tempo / OpenTelemetry)    | Trace export                                     |
| SMTP relay endpoint                       | Outbound email                                  |

New egress targets MUST be added to the policy explicitly; CI gate
`supply-chain.yml` flags unknown egress destinations.
