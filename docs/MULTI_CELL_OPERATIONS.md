# Multi-Cell Operations Guide

A "cell" is one self-contained Kapp deployment: a PostgreSQL cluster,
a Kubernetes namespace running `api` / `worker` / `kchat-bridge`, a
NATS cluster, a Redis cluster, an object-storage tier, and optional
per-cell observability. Cells share **no operational state** — each
cell's database is the source of truth for the tenants assigned to it.

Cells exist for two reasons:

1. **Horizontal scaling.** Beyond ~5,000 tenants / cell the marginal
   cost of more compute grows faster than the marginal capacity
   gained (see [CAPACITY_PLANNING.md §1.1](./CAPACITY_PLANNING.md#1-resource-sizing-matrix)).
2. **Data residency.** Some tenants require their data to live in a
   specific geographic region (GDPR Art. 44–50, US-region-only).

---

## 6.1 Cell Architecture Overview

```
                       Internet
                          │
                          ▼
                  ┌────────────────┐
                  │ L7 LB / TLS    │
                  └───────┬────────┘
                          │  (tenant → cell routing)
       ┌──────────────────┼──────────────────┐
       ▼                  ▼                  ▼
  ┌──────────┐       ┌──────────┐       ┌──────────┐
  │ cell-a   │       │ cell-b   │       │ cell-c   │
  │ ──────── │       │ ──────── │       │ ──────── │
  │ API pods │       │ API pods │       │ API pods │
  │ Worker   │       │ Worker   │       │ Worker   │
  │ Bridge   │       │ Bridge   │       │ Bridge   │
  │ Redis    │       │ Redis    │       │ Redis    │
  │ NATS     │       │ NATS     │       │ NATS     │
  │ Postgres │       │ Postgres │       │ Postgres │
  │ ZK fab.  │       │ ZK fab.  │       │ ZK fab.  │
  └──────────┘       └──────────┘       └──────────┘

  ┌────────────────────────────────────────────────┐
  │ Control plane (cells registry, tenant lookup)  │
  └────────────────────────────────────────────────┘
```

Each cell is independent: no cross-cell DB queries, no shared
locks, no shared caches. The control plane (`public.cells`,
`public.tenants.cell_id` — see `migrations/000041_cell_capacity.sql`)
is the only globally visible state. The L7 load balancer reads it on
startup and refreshes on a TTL; in-flight requests use the cached
mapping.

The cell-id column on `tenants` is `NULL`-able. A `NULL` cell_id
maps to the seeded `'default'` cell — newly created tenants land
there until the placement policy places them elsewhere.

---

## 6.2 Cell Provisioning Automation

Reference Terraform module layout (the repo ships a starter at
`deploy/terraform/cell/`; non-shipped resources are placeholders to be
implemented per-cloud):

```
deploy/terraform/cell/
├── main.tf            # composes the modules below
├── variables.tf       # cell-id, region, tenant-count target
├── outputs.tf         # DSN, NATS URL, S3 endpoint
├── postgres/          # primary + 2 replicas, parameter group
├── kubernetes/        # namespace, service accounts, RBAC
├── nats/              # 3- or 5-node cluster (gated on var.tenant_target)
├── redis/             # 3-node + sentinel
├── zk_fabric/         # per-cell ZK Fabric or fallback MinIO
├── dns/               # api.<cell-id>.<root>, portal.<cell-id>.<root>
└── monitoring/        # Prometheus targets, Grafana datasource
```

Expected provisioning time: **~30 minutes** automated (per-cloud
provider). The longest leg is PostgreSQL multi-AZ replica
provisioning (~15 min on RDS / Cloud SQL).

End-to-end provisioning (assuming Terraform is the IaC):

```bash
cd deploy/terraform/cell
terraform init
terraform apply \
  -var "cell_id=cell-b" \
  -var "region=us-east-1" \
  -var "tenant_target=5000"
# Outputs:
#   db_url       = postgres://kapp:****@cell-b-primary:5432/kapp?sslmode=verify-full
#   nats_url     = nats://nats.cell-b.svc.cluster.local:4222
#   s3_endpoint  = https://s3.cell-b.kapp.example.com

# Apply schema:
DB_URL=$(terraform output -raw db_url) make migrate
DB_URL=$(terraform output -raw db_url) make migrate-version    # confirm tip

# Register in control plane:
psql "$KAPP_ADMIN_DB_URL" <<SQL
INSERT INTO cells (id, region, max_tenants)
VALUES ('cell-b', 'us-east-1', 5000)
ON CONFLICT (id) DO NOTHING;
SQL

# Deploy applications (Helm chart at deploy/helm/kapp):
helm upgrade --install kapp-cell-b deploy/helm/kapp \
  -n kapp-cell-b --create-namespace \
  --values  deploy/helm/values-cell-b.yaml \
  --set     image.tag=v0.1.1
```

---

## 6.3 Cross-Cell Operations

Some routes operate across cells. Their lifecycle:

| Route                                            | Behaviour                                                                                            |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------------------- |
| `GET /api/v1/admin/tenants`                      | Reads from the central `tenants` registry (control plane). Each row's `cell_id` is returned to the caller. |
| `POST /api/v1/admin/tenants/{id}/migrate`        | Drives the cell migration procedure in [SCALING_RUNBOOK.md §2.5](./SCALING_RUNBOOK.md#25-tenant-migration-between-cells). |
| `GET /api/v1/admin/cells`                        | Returns the `cells` table state plus aggregate load (CPU, mem, conn-saturation) and tenant counts.    |
| Cross-cell analytics dashboards                  | Each cell exposes its own metrics; the central Prometheus federates by `cell` label.                 |

Cross-cell event forwarding (e.g. platform admin notifications,
billing roll-ups) is routed via a separate NATS super-cluster
configured by the platform team — application code does not depend on
cross-cell NATS subjects.

Consolidated reporting:

- Per-cell Prometheus → central Mimir/Thanos via remote-write.
- Per-cell Loki/CloudWatch logs → central Loki via Promtail or
  CloudWatch cross-account export.
- Grafana with one datasource per cell *plus* one federated
  datasource for the cross-cell dashboards
  (see [OBSERVABILITY_GUIDE.md §8.2](./OBSERVABILITY_GUIDE.md#82-dashboard-catalog)).

---

## 6.4 Cell Health Dashboard

Per-cell metrics that every operator dashboard must surface (Grafana
JSON at `deploy/grafana/dashboards/kapp-cell-comparison.json`,
planned):

- **Tenants**: active (`status='active'`) vs. total.
- **Database**: CPU %, memory %, connections (cl_active /
  max_client_conn), replication lag (per replica), bloat ratio.
- **API**: pod count, CPU/memory utilization, request rate (rps),
  error rate, p99 latency.
- **Worker**: outbox depth (`SELECT count(*) FROM events WHERE
  delivered_at IS NULL`), drain rate
  (`rate(kapp_outbox_events_total[5m])`), batch size distribution,
  DLQ depth.
- **Object storage**: total bytes, requests/sec, GET / PUT split,
  4xx / 5xx rate.
- **Top tenants** (sortable):
  - by API calls: `topk(10, sum by (tenant_id) (rate(kapp_request_total[5m])))`
  - by storage:   `topk(10, sum by (tenant_id) (kapp_tenant_storage_bytes))` (planned)
  - by record count:
    ```sql
    SELECT tenant_id, count(*) FROM krecords
    GROUP BY tenant_id ORDER BY count DESC LIMIT 10;
    ```

---

## 6.5 Cell Decommissioning

Decommissioning is the inverse of provisioning. Always migrate
tenants away first — never destroy data still owned by an active
tenant.

```bash
TARGET=cell-b
SAFE_CELLS="cell-a,cell-c"

# 1. Freeze new tenant assignments:
psql "$KAPP_ADMIN_DB_URL" <<SQL
UPDATE cells SET max_tenants = 0 WHERE id = '$TARGET';
SQL

# 2. Migrate tenants away, max 10 concurrent (rate-limit risk to the
#    destination cells). For each tenant follow SCALING_RUNBOOK.md §2.5.
psql "$KAPP_ADMIN_DB_URL" -At -c "SELECT id FROM tenants WHERE cell_id='$TARGET' AND status='active'" \
  | xargs -n1 -P10 -I{} ./scripts/migrate_tenant.sh {} $TARGET cell-a

# 3. Verify zero active tenants:
psql "$KAPP_ADMIN_DB_URL" -At -c "SELECT count(*) FROM tenants WHERE cell_id='$TARGET' AND status='active'"
# Must return 0 before proceeding.

# 4. Drain pending outbox and scheduled actions on $TARGET:
psql "$TARGET_DB_URL" <<SQL
SELECT count(*) FROM events WHERE delivered_at IS NULL;      -- must be 0
SELECT count(*) FROM scheduled_actions WHERE state='claimed';-- must be 0
SQL

# 5. Final backup:
go run ./services/kapp-backup snapshot \
  --db "$TARGET_DB_URL" \
  --out s3://kapp-backups/$TARGET/final/$(date -u +%Y-%m-%dT%H-%M-%S).tar.gz

# 6. Tear down infrastructure:
helm -n kapp-$TARGET uninstall kapp-$TARGET
kubectl delete namespace kapp-$TARGET
cd deploy/terraform/cell && terraform destroy -var "cell_id=$TARGET"

# 7. Remove from registry:
psql "$KAPP_ADMIN_DB_URL" <<SQL
DELETE FROM cells WHERE id = '$TARGET';
SQL
```

Audit trail of the decommissioning event is captured in
`platform_scale_events` automatically by the autoscaler
(`internal/platform/autoscaler.go`).

---

## 6.6 Placement Policy

Where new tenants land is governed by:

1. **Explicit assignment** (control-plane API):
   `POST /api/v1/admin/tenants` with `{"cell_id": "cell-b"}`.
2. **Region preference** (tenant config): match
   `cells.region` to the tenant's required residency.
3. **Capacity scoring**:
   ```
   score(cell) = max_tenants(cell) - current_tenants(cell)
              - 50 * (cpu_pct(cell) > 60)
              - 50 * (conn_saturation_pct(cell) > 70)
   ```
   Highest score wins.
4. **Fallback** to the seeded `default` cell if no candidates qualify;
   a fallback fires `KappPlatformOverloaded` (planned) on every
   placement.

Verify a placement decision via the audit log:

```sql
SELECT * FROM platform_scale_events
WHERE event_type IN ('scale_up','hold')
  AND created_at > now() - interval '24 hours'
ORDER BY created_at DESC;
```
