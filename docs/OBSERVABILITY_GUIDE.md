# Observability Guide

Three pillars — metrics, logs, traces — wired across `services/api`,
`services/worker`, `services/kchat-bridge`, `services/importer`, and
`services/agent-tools`. The actual implementation lives in:

- `internal/platform/metrics.go` (Prometheus exposition format,
  zero-dep)
- `internal/platform/tracing.go` (OpenTelemetry OTLP/gRPC)
- `internal/platform/log.go` (slog JSON handler, request_id +
  tenant_id correlation)

Cross-references:

- Alert catalogue: [ONCALL_PLAYBOOK.md](./ONCALL_PLAYBOOK.md)
- SLOs: [INCIDENT_RESPONSE.md §3.1](./INCIDENT_RESPONSE.md#31-severity-definitions)
- Load testing: [LOAD_TESTING.md](./LOAD_TESTING.md)

---

## 8.1 Three Pillars Setup

### Metrics (Prometheus)

Scrape configuration:

```yaml
# prometheus.yaml (or via Prometheus Operator ServiceMonitor)
scrape_configs:
  - job_name: kapp-api
    metrics_path: /metrics
    static_configs:
      - targets: ['api.kapp:9090']
    relabel_configs:
      - source_labels: [__address__]
        target_label: cell
        replacement: cell-a

  - job_name: kapp-worker
    metrics_path: /metrics
    static_configs:
      - targets: ['worker.kapp:9091']

  - job_name: kapp-kchat-bridge
    metrics_path: /metrics
    static_configs:
      - targets: ['kchat-bridge.kapp:9092']
```

Production deployments should set `KAPP_METRICS_ADDR=:9090` to expose
`/metrics` on a dedicated port (off the user-facing auth chain — see
the rationale in `.env.example` lines around `KAPP_METRICS_ADDR`).

Retention:

- Raw: 30 days
- Downsampled (5m resolution): 1 year via Thanos / Mimir / Cortex
- Recording rules for hot queries:
  ```yaml
  # deploy/prometheus/recording-rules.yaml
  groups:
    - name: kapp-recording
      interval: 30s
      rules:
        - record: kapp:request_rate_5m
          expr: sum by (cell, path) (rate(kapp_request_total[5m]))
        - record: kapp:error_rate_5m
          expr: |
            sum by (cell) (rate(kapp_request_total{status=~"5.."}[5m]))
            / sum by (cell) (rate(kapp_request_total[5m]))
        - record: kapp:p99_latency_5m
          expr: |
            histogram_quantile(
              0.99,
              sum by (cell, path, le) (rate(kapp_request_duration_seconds_bucket[5m]))
            )
        - record: kapp:outbox_depth
          expr: kapp_outbox_drain_duration_seconds
  ```

### Logs (structured JSON)

Every service emits one-JSON-object-per-line via Go's `slog`
(`internal/platform/log.go`). Required fields on every line:

| Field         | Source                                                |
| ------------- | ----------------------------------------------------- |
| `service`     | Static, set at boot ("api" / "worker" / "kchat-bridge") |
| `tenant_id`   | Request context (`SET LOCAL app.tenant_id`)            |
| `request_id`  | Chi `middleware.RequestID`                            |
| `trace_id`    | OpenTelemetry propagation                              |
| `level`       | slog level (DEBUG / INFO / WARN / ERROR)               |
| `msg`         | Human-readable message                                 |
| `time`        | ISO-8601 UTC                                           |

Ship via Fluent Bit sidecar (default), Vector, or Loki Promtail.
Retention: 30 days hot, 90 days cold (S3 with Object Lock for audit
logs).

Log query cookbook (LogQL — Loki):

```logql
# All errors for tenant X in the last hour
{service="api"} |= `"tenant_id":"<UUID>"` |= `"level":"ERROR"` | json

# Slow requests (> 2 s)
{service="api"} | json | duration > 2.0

# Recent panics across services
{service=~"api|worker|kchat-bridge"} |= "panic" | json

# All audit-log writes for a tenant
{service="api"} |= `"event":"audit.write"` |= `"tenant_id":"<UUID>"`
```

For CloudWatch / Elasticsearch, the same field names apply; adapt the
query syntax.

### Traces (OpenTelemetry → Tempo / Jaeger)

Configuration (lines in `.env.example`):

```bash
KAPP_OTEL_ENDPOINT=tempo.observability:4317
KAPP_OTEL_INSECURE=0                # production MUST use TLS
KAPP_OTEL_SAMPLE_RATIO=0.10         # 10% head-based sampling
KAPP_OTEL_SERVICE_VERSION=v0.1.1    # filled by CI/CD
```

Sampling strategy:

- **Production**: 10 % head-based; always sample on error
  (tail-based once Tempo's `tail_sampling` processor is enabled).
- **Staging**: 100 %.

Trace propagation flows: API (`otelhttp.NewMiddleware`) → DB
(`WithTenantTx` wraps the transaction in a span) → NATS publish
(span context as message header) → Worker consumer.

Key spans to instrument (already in place unless marked **(planned)**):

| Span                                       | Owner                                |
| ------------------------------------------ | ------------------------------------ |
| HTTP handler (`http.server.request`)       | `otelhttp` auto-instrumentation      |
| DB transaction (`db.tx`)                   | `internal/platform/db.go::WithTenantTx` |
| NATS publish / subscribe                   | Worker (`services/worker`)            |
| Outbound HTTP (integrations, KChat API)    | `otelhttp.NewTransport`              |
| File upload / download (ZK Fabric)          | `internal/files/store.go` **(planned)** |
| KType validation                           | `internal/ktype/validate.go` **(planned)** |

---

## 8.2 Dashboard Catalog

Grafana JSON ships at `deploy/grafana/dashboards/` (planned). One JSON
per dashboard so operators can hand-edit without breaking the
provisioning sidecar.

| File                                                | Purpose                                                                 |
| --------------------------------------------------- | ----------------------------------------------------------------------- |
| `kapp-overview.json`                                | Top-line: request rate, error rate, p50 / p95 / p99 latency, active tenants. |
| `kapp-database.json`                                | Connections, transactions/sec, tuple activity, WAL rate, replication lag, vacuum stats. |
| `kapp-worker.json`                                  | Outbox depth, drain rate, batch sizes, scheduled-actions due / executed, DLQ depth. |
| `kapp-tenant-health.json`                           | Per-tenant API calls, storage, record counts, quota utilization (top-N by metric).      |
| `kapp-security.json`                                | Auth failures, RLS violations (target: 0), rate-limit hits, blocked requests.           |
| `kapp-integrations.json`                            | Per-integration success/failure rates, latency, webhook delivery stats.                |
| `kapp-cell-comparison.json`                         | Side-by-side cell metrics for multi-cell deployments.                                  |

Each dashboard exposes:

- Cell filter (single or "All")
- Tenant filter (regex against `tenant_id` label; cardinality-safe
  because the metric labels are already low-cardinality after the
  `path` is normalized to a chi route pattern in
  `internal/platform/metrics.go`)
- Time range (default 1 h, 6 h, 24 h, 7 d)

---

## 8.3 Alert Routing

| Severity | Route                                                        | Action                                       |
| -------- | ------------------------------------------------------------ | -------------------------------------------- |
| SEV-1    | PagerDuty / Opsgenie → phone escalation in 5 min if no ack    | Wakes up on-call regardless of hour.         |
| SEV-2    | PagerDuty / Opsgenie → Slack/KChat fallback                   | Pages during business hours; Slack out of hours. |
| SEV-3    | Slack/KChat channel notification only                         | No page.                                     |
| SEV-4    | Jira / Linear ticket auto-creation                            | No page; backlog item.                       |

Silencing during maintenance windows is registered ahead of time via
Alertmanager `silences` API:

```bash
amtool --alertmanager.url=http://alertmanager:9093 silence add \
  alertname=~Kapp.* cell=cell-a -c "Maintenance window: v0.2.0 upgrade" \
  -d 60m -u devin
```

---

## 8.4 SLI / SLO Implementation

**Service Level Indicators** (per service):

| SLI                                                         | Definition                                                                                  |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------- |
| Availability                                                | `1 - (5xx rate)` over a 30-day window                                                       |
| Latency (CRUD)                                              | `p99 < 500 ms` for paths under `/api/v1/{records,ktypes,roles,users,plans}`                  |
| Latency (reports / insights)                                | `p99 < 5 s` for paths under `/api/v1/reports/`, `/api/v1/insights/`                          |
| Outbox freshness                                            | `kapp_outbox_drain_duration_seconds < 5` p99                                                |
| Audit chain integrity                                       | `kapp_audit_chain_broken_total == 0` (planned)                                              |

**Service Level Objectives:**

| Surface                  | Target                |
| ------------------------ | --------------------- |
| Availability             | 99.9 % monthly        |
| Latency (CRUD)            | p99 < 500 ms          |
| Latency (reports)         | p99 < 5 s             |
| Outbox freshness          | p99 < 5 s             |
| Audit chain integrity     | 100 % (no breaks)     |

Error budget: at 99.9 % availability, the monthly budget is
**43.2 minutes** of downtime equivalent.

**Burn-rate alerts** (multi-window, multi-burn-rate):

```promql
# 14.4× burn over 1h → 2% of monthly budget consumed → page
sum(rate(kapp_request_total{status=~"5.."}[1h]))
  / sum(rate(kapp_request_total[1h])) > 0.0144

# 6× burn over 6h → 5% of monthly budget consumed → page
sum(rate(kapp_request_total{status=~"5.."}[6h]))
  / sum(rate(kapp_request_total[6h])) > 0.006

# 1× burn over 3d → 10% of monthly budget consumed → ticket
sum(rate(kapp_request_total{status=~"5.."}[3d]))
  / sum(rate(kapp_request_total[3d])) > 0.001
```

Implementation gauge (planned in v0.2):
`kapp_slo_error_budget_remaining` updated by a recording rule every
1m so dashboards can show the remaining budget without re-computing
on every render.
