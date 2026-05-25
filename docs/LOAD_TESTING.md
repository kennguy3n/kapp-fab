# Load Testing Guide

How to load-test Kapp against the production SLOs. The in-tree
harness is `internal/integrationtest/loadtest` (Go); the recommended
external driver is **k6** for repeatable end-to-end runs.

Cross-references:

- SLO definitions: [OBSERVABILITY_GUIDE.md §8.4](./OBSERVABILITY_GUIDE.md#84-sli-slo-implementation)
- Capacity planning: [CAPACITY_PLANNING.md](./CAPACITY_PLANNING.md)
- Performance tuning: [PERFORMANCE_TUNING.md](./PERFORMANCE_TUNING.md)

---

## 16.1 Test Scenarios

| Scenario           | Purpose                                                                    | Duration       | Virtual users (VUs)        |
| ------------------ | -------------------------------------------------------------------------- | -------------- | -------------------------- |
| Smoke              | Sanity check the system under minimal load                                  | 1 minute       | 1                          |
| Load               | Sustained traffic at production peak; validate SLOs                         | 30 minutes     | 100                        |
| Stress             | Find the breaking point and the recovery curve                              | 30 minutes     | Ramp 100 → 1000 over 20 min |
| Soak               | Detect memory leaks and connection-pool drift                                | 4 hours        | 50                         |
| Spike              | Test autoscaling and burst capacity                                          | 10 minutes     | Ramp 100 → 500 in 30 s, hold 5 min, return |
| Multi-tenant       | Realistic mix of tenants; ensure RLS hot paths still meet SLO                | 30 minutes     | 100 VUs × 50 tenants       |

---

## 16.2 Workload Profile

Each virtual user iterates the following block (one iteration is one
"user transaction"):

1. **Login** (`POST /api/v1/auth/login`) — once per VU per session;
   tokens kept warm for the rest of the iteration.
2. **List records** (`GET /api/v1/records?ktype=crm.deal&limit=20`).
3. **Create record** (`POST /api/v1/records`, ktype `crm.deal`).
4. **Workflow transition** (`POST /api/v1/records/{id}/transitions`,
   move stage `qualification → proposal`).
5. **Filter records** (`GET /api/v1/records?ktype=crm.deal&filter=...`).
6. **Generate report** (`POST /api/v1/reports`, simple CRM
   pipeline report).
7. **Upload file** (`POST /api/v1/files`, 100 KB synthetic payload).

Approximately **7 requests per iteration**. With 100 VUs at a 1 s
think time, sustained throughput ≈ 700 rps.

Token reuse is critical: do **not** login on every iteration —
production traffic re-uses long-lived access tokens (15-minute TTL,
auto-refreshed). The smoke test re-validates the login path daily;
the load test runs against a warm session.

---

## 16.3 SLO Thresholds

The load test asserts the following thresholds; the run fails if any
is breached. These match the production SLOs in
[OBSERVABILITY_GUIDE.md §8.4](./OBSERVABILITY_GUIDE.md#84-sli-slo-implementation).

| Metric                              | Threshold                       |
| ----------------------------------- | ------------------------------- |
| p50 latency (CRUD endpoints)        | < 100 ms                        |
| p95 latency (CRUD)                  | < 300 ms                        |
| p99 latency (CRUD)                  | < 500 ms                        |
| p99 latency (reports)               | < 5 s                           |
| Error rate (HTTP 5xx)               | < 0.1 %                         |
| Throughput per pod                   | > 100 rps at 1 vCPU, 200 Mi limit |
| Outbox lag during the test          | < 5 s                           |
| Database CPU during the test        | < 60 % sustained                |

---

## 16.4 Running Tests

### k6 against staging

The k6 script lives at `scripts/loadtest/k6/kapp.js` (planned).

```bash
# Smoke (1 VU, 1 min):
k6 run --vus 1 --duration 1m \
  -e API_BASE=https://staging.kapp.example.com \
  -e SMOKE_TOKEN=$SMOKE_TOKEN \
  scripts/loadtest/k6/kapp.js

# Load (100 VUs, 30 min):
k6 run --vus 100 --duration 30m \
  -e API_BASE=https://staging.kapp.example.com \
  -e SMOKE_TOKEN=$SMOKE_TOKEN \
  scripts/loadtest/k6/kapp.js

# Stress (ramp 100 → 1000):
k6 run --stage 5m:100 --stage 20m:1000 --stage 5m:0 \
  -e API_BASE=https://staging.kapp.example.com \
  -e SMOKE_TOKEN=$SMOKE_TOKEN \
  scripts/loadtest/k6/kapp.js

# Spike (rapid burst):
k6 run --stage 30s:500 --stage 5m:500 --stage 30s:0 \
  -e API_BASE=https://staging.kapp.example.com \
  -e SMOKE_TOKEN=$SMOKE_TOKEN \
  scripts/loadtest/k6/kapp.js

# Soak (memory-leak detection):
k6 run --vus 50 --duration 4h \
  -e API_BASE=https://staging.kapp.example.com \
  -e SMOKE_TOKEN=$SMOKE_TOKEN \
  scripts/loadtest/k6/kapp.js
```

### Go harness (in-tree)

```bash
# Phase G acceptance (multi-tenant load, runs in CI):
go test -tags=loadtest ./internal/integrationtest/loadtest \
  -run TestPhaseGAcceptance -v -timeout 30m

# ZK Fabric load:
go test -tags=loadtest ./internal/integrationtest/loadtest \
  -run TestZKFabricLoad -v -timeout 10m
```

### Local compose loop (developer iteration)

```bash
# Bring up the compose stack:
make compose-up

# Run a 5-minute smoke against the local API:
k6 run --vus 10 --duration 5m \
  -e API_BASE=http://localhost:8080 \
  -e SMOKE_TOKEN=$LOCAL_TOKEN \
  scripts/loadtest/k6/kapp.js
```

---

## 16.5 Interpreting Results

k6 emits JSON metrics:

```bash
k6 run --out json=loadtest.json ... scripts/loadtest/k6/kapp.js
jq -c '.metric+"\t"+(.data.value|tostring)' loadtest.json | sort -u | head
```

Mandatory observations during every run:

- **Latency histograms** (Grafana → "Kapp Load Test" dashboard).
- **Database utilization** (CPU, connection pool, lock waits).
- **NATS queue depth** (`kapp_outbox_drain_duration_seconds`).
- **Pod CPU / memory** (top during ramp-up; verify the API HPA's
  `targetCPUUtilizationPercentage`).
- **Error log volume** (Loki — count of `level=ERROR` per service).

Baseline comparison:

```bash
# Last green run is stored as the baseline:
aws s3 cp s3://kapp-loadtest-baseline/staging/latest.json baseline.json

# Compare:
python scripts/loadtest/compare_baseline.py loadtest.json baseline.json
# Fails if any threshold regresses by > 10%.
```

---

## 16.6 Performance Regression Gate

CI runs a 5-minute smoke test on every PR; the script asserts:

| Step          | Required                                                          |
| ------------- | ----------------------------------------------------------------- |
| Build         | Service binaries compile.                                          |
| Migrate       | Migrations apply cleanly to a fresh DB.                            |
| Seed          | Sample tenant + KType registered.                                   |
| Run smoke      | k6 1-VU run completes with all endpoints returning < 500 ms p99.  |
| Tear down     | DB & service teardown succeeds.                                    |

Output is uploaded as a CI artefact (`loadtest-summary.json`).

A separate release gate runs the **30-minute load test** against
staging on every release candidate; failure blocks promotion to
production.

---

## 16.7 Test Data Hygiene

Production-shaped test data is critical — toy data hides real
problems.

- **Tenants**: 50 baseline tenants seeded by
  `scripts/loadtest/seed.sh`, each with realistic record counts (see
  [CAPACITY_PLANNING.md §1.1](./CAPACITY_PLANNING.md#1-resource-sizing-matrix) for the per-tenant baseline).
- **Distribution**: 80 % of traffic targets 20 % of tenants
  (Pareto), matching observed production patterns.
- **Cleanup**: load tests run against a dedicated `loadtest-*`
  tenant set. The teardown step deletes them via the admin destroy
  endpoint; the cleanup MUST run even if the test fails (set
  `trap 'scripts/loadtest/cleanup.sh' EXIT`).
- **Identifiers**: tenant + user IDs are UUIDv7 (deterministic
  ordering) so partition pruning behaves identically across runs.
