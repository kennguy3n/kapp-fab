# Developer Guide

This guide is for engineers contributing to Kapp. It covers the
monorepo layout, local setup, the test suite, and the patterns
the codebase uses for new features (KType + KRecord, agent tools,
events / scheduled jobs, frontend modules).

## Monorepo layout

```
kennguy3n/kapp-fab/
├── services/
│   ├── api/              # HTTP API (Chi router, middleware stack)
│   ├── worker/           # Outbox drain + scheduled actions + queues
│   ├── kchat-bridge/     # KChat slash commands and card rendering
│   └── kapp-backup/      # Per-tenant extract / restore CLI
├── internal/
│   ├── ktype/            # Schema + field validation engine
│   ├── record/           # Generic KRecord CRUD store
│   ├── workflow/         # State-machine workflow engine
│   ├── ledger/           # Double-entry GL + invoice posting
│   ├── reporting/        # Saved reports + scheduling + runner
│   ├── inventory/        # Items, warehouses, append-only moves
│   ├── helpdesk/         # SLA policies + tickets + portal
│   ├── lms/              # Courses + enrollments + certificates
│   ├── agents/           # Agent tools (commit / dry-run modes)
│   ├── events/           # Outbox publisher
│   ├── scheduler/        # Tenant-scoped scheduled actions
│   ├── platform/         # Tenant context, RLS, middleware, LRU
│   ├── notifications/    # SMTP, KChat DM, in-app notifications
│   ├── files/            # ZK Object Fabric per-tenant S3 stores
│   ├── exporter/         # Phase K data export queue
│   └── …
├── apps/
│   ├── web/              # React / Vite frontend
│   └── storybook/        # Component library
├── migrations/           # Hand-rolled SQL, applied in lexicographic order
├── docs/                 # OPERATOR_GUIDE, DEVELOPER_GUIDE, SECURITY_REVIEW, DR_RUNBOOK, …
└── scripts/              # ops scripts (migrate, upgrade-tier, dr-drill)
```

## Prerequisites

* Go ≥ 1.22 (see `go.mod`)
* Node.js ≥ 20 + npm
* Docker + docker compose
* `psql` ≥ 16

## Local setup

```sh
# 1. Bring up dependencies
docker compose up -d              # postgres, nats, mailhog, minio

# 2. Run migrations
make migrate

# 3. Build and run the three Go services
make build
make run-api &
make run-worker &
make run-kchat-bridge &

# 4. Frontend
npm install
npm run dev -w @kapp/web          # http://localhost:5173
```

## Running tests

```sh
go test ./...                                  # unit tests
go test -tags integration ./internal/integrationtest/...
go test -tags loadtest ./internal/integrationtest/loadtest/
npm run test -w @kapp/web                      # vitest unit
npm run typecheck -w @kapp/web                 # tsc --noEmit
npm run build -w @kapp/web                     # production bundle
npm run build -w @kapp/storybook               # static storybook
```

The integration tests in `internal/integrationtest/phase_*_test.go`
expect a live local Postgres + NATS via `docker compose up`. They
provision a fresh tenant per test, set `app.tenant_id`, and rely
on the same RLS policies the production cell enforces.

## Lint and format

```sh
go vet ./...
gofmt -l .                        # output should be empty
npm run lint -w @kapp/web
```

CI runs the same commands. The pre-commit hook
(`.pre-commit-config.yaml`) wraps them; install with
`pre-commit install` after the first checkout.

## Adding a new KApp / KType

1. Pick a package under `internal/<domain>/` — e.g. `internal/lms/`.
2. Declare the KType schema as a JSON byte slice (
   `internal/lms/lms.go::courseSchema`). Fields, views, cards,
   permissions, and an optional workflow live in this one literal.
3. Add the KType to `All()` so the wizard registers it on every
   tenant.
4. Add hand-rolled SQL in `migrations/0000NN_<your-feature>.sql` if
   you need extra tables / indexes / RLS policies. Migrations apply
   in lexicographic order; pick the next free number.
5. Wire any custom store logic in a sibling file (`store.go`,
   `certificates.go`, …). Reads / writes must go through
   `dbutil.WithTenantTx` so RLS is enforced.
6. Expose HTTP handlers in `services/api/` if you need typed
   endpoints beyond the generic `POST /api/v1/krecords`.
7. Surface in the frontend in `apps/web/src/pages/`.

A worked example lives in `docs/KTYPE_AUTHORING_GUIDE.md`.

## Adding an agent tool

Agent tools live in `internal/agents/<domain>_tools.go` and
implement the `Tool` interface (`Name() string`,
`RequiresConfirmation() bool`, `Invoke(ctx, Invocation)`). Two
modes matter:

* `ModeDryRun` — return a preview, never mutate state.
* default (commit) — execute the action.

Tools that mutate state (post invoice, reverse stock move, issue
certificate) should set `RequiresConfirmation() = true`. The
agents layer surfaces a confirmation card in KChat before calling
back with `Mode = commit`.

## Adding a scheduled action

Three places:

1. Register the handler in `services/worker/main.go::run` with
   `schedRegistry.Register("<action_type>", h)`.
2. Seed the row in `internal/tenant/wizard.go::seedDefaultScheduledActions`
   so every new tenant gets it.
3. Implement the handler. It receives `(ctx, tenantID, action)`
   with the tenant context already set; do your work and return.

Polling cadence is 10 s (the scheduler runloop); per-row eligibility
is owned by the handler.

## Adding an event consumer

Producers append to the outbox via
`events.Publisher.Publish(ctx, tx, …)`. The worker drains the
outbox on a fixed cadence and dispatches to NATS subjects of the
form `kapp.<domain>.<event>` (e.g. `kapp.inventory.move.reversed`).
Add a NATS subscriber if you need a side-effect; do not poll.

## Frontend

* Routes live in `apps/web/src/App.tsx` (Routes block).
* API calls go through `apps/web/src/api/client.ts` which honours
  the X-Tenant-Id and idempotency headers automatically.
* Feature gates: `useFeature("multi_currency")` returns a
  boolean tracking `tenant_features`.
* New page: copy the closest existing page (e.g.
  `ReportBuilderPage.tsx`); register in `App.tsx`; add a link
  somewhere in `Layout.tsx` if it should appear in the nav.

## Submitting a PR

1. Branch off `main`: `git checkout -b yourname/feature-XYZ`.
2. Hack. Run `go vet ./... && gofmt -l . && go build ./...` until clean.
3. Run `npm run typecheck -w @kapp/web && npm run build -w @kapp/web`.
4. Commit with a message that explains *why* the change is needed.
5. Open the PR; CI runs the full lint + build matrix.
6. After merge, the deployment is automatic to staging; promotion
   to production is a manual step on the release tracker.
