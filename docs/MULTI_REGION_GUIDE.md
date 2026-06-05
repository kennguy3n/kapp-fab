# Multi-Region Automation Guide

How Kapp runs across geographic regions and how the autoscaler
**automatically** provisions, drains, and tears down cells. This guide
covers the *automation* layer added in Stream 5; the manual day-to-day
cell operations live in
[MULTI_CELL_OPERATIONS.md](./MULTI_CELL_OPERATIONS.md), capacity maths in
[CAPACITY_PLANNING.md](./CAPACITY_PLANNING.md), and failover drills in
[DR_RUNBOOK.md](./DR_RUNBOOK.md).

Cross-references:

- Cell concepts & manual ops: [MULTI_CELL_OPERATIONS.md](./MULTI_CELL_OPERATIONS.md)
- Autoscaler policy internals: `internal/platform/autoscaler.go`
- Provisioner implementations: `internal/platform/cell_provisioner.go`
- Tenant migration (rebalancer): `internal/platform/cell_rebalancer.go`
- Disaster recovery: [DR_RUNBOOK.md](./DR_RUNBOOK.md)

---

## 1. Cell topology: one cell per region, shared control plane

Kapp is a **cellular** architecture. A *cell* is one self-contained
deployment (PostgreSQL cluster, `api`/`worker`/`kchat-bridge`, NATS,
Redis, object storage). Cells share no operational data — each cell's
database is the source of truth for the tenants placed on it.

The recommended multi-region layout is **one (or more) cells per
region**, fronted by a **shared control plane**:

```
                         ┌──────────────────────────┐
                         │      Shared control       │
                         │         plane             │
                         │  • tenant registry        │
                         │    (tenants.cell_id)      │
                         │  • cells table            │
                         │  • autoscaler (worker)     │
                         │  • Route 53 / global DNS  │
                         └────────────┬──────────────┘
                                      │ tenant → region → cell
          ┌───────────────────────────┼───────────────────────────┐
          ▼                           ▼                           ▼
  ┌────────────────┐         ┌────────────────┐         ┌────────────────┐
  │ region: us-east│         │ region: eu-west│         │ region: ap-south│
  │ cell-us-east-1 │         │ cell-eu-west-1 │         │ cell-ap-south-1│
  │  RDS + api/wkr │         │  RDS + api/wkr │         │  RDS + api/wkr │
  └────────────────┘         └────────────────┘         └────────────────┘
```

The control plane holds only **placement metadata**, never tenant
business data:

- `tenants.cell_id` — which cell currently hosts each tenant.
- `cells` — the fleet inventory: `region`, capacity (`max_tenants`),
  observed load (`cpu_pct`, `mem_pct`, `conn_saturation_pct`), and the
  region metadata added in
  `migrations/000081_cell_region_metadata.sql` (`provider`, `zone`,
  `endpoint`, `status`, `provisioner`, `metadata`).

Because the control-plane tables carry no `tenant_id`, they have no
row-level security and live in `public` so the worker's regular pool
can read/write them (see the RLS note in
`migrations/000041_cell_capacity.sql`).

---

## 2. DNS-based routing: tenant → region → cell

Routing resolves in three hops:

1. **Tenant → region.** A tenant is pinned to a region at signup (see
   §4, data residency). The region is stored alongside the tenant's
   placement.
2. **Region → cell.** Within a region the control plane picks the cell
   that currently hosts the tenant (`tenants.cell_id`). New tenants are
   placed on the least-loaded active cell in their region.
3. **Cell → endpoint.** `cells.endpoint` (added in migration 000081)
   tells the L7 router where the cell actually lives.

A practical DNS layout uses a **latency- or geo-routing policy** on a
shared zone:

```
app.kapp.example.com            (global, geo-routed)
  ├── us.app.kapp.example.com   → us-east ALB  → cell-us-east-1
  ├── eu.app.kapp.example.com   → eu-west ALB  → cell-eu-west-1
  └── ap.app.kapp.example.com   → ap-south ALB → cell-ap-south-1
```

The control plane is authoritative: even if geo-DNS sends a request to
the "wrong" regional edge, the edge looks up `tenants.cell_id` and
proxies to the correct cell. DNS is an optimization for latency, not a
correctness boundary.

---

## 3. Cell autoscaling automation

The autoscaler is a 60-second loop in the worker
(`internal/platform/autoscaler.go`, wired in `services/worker/main.go`).
Each tick it snapshots every cell, applies the policy (`Decide`), and
records a `scale_up` / `scale_down` / `hold` decision into
`platform_scale_events`.

By default the loop is **observe-only** — it records decisions and emits
structured logs but touches no infrastructure. This is the historic
behaviour and remains the default.

### 3.1 Enabling auto-provisioning

Set these environment variables on the **worker**:

| Variable | Default | Meaning |
|---|---|---|
| `KAPP_AUTOSCALE_PROVISION` | `false` | Master switch. When `true`, the autoscaler actuates decisions. |
| `KAPP_AUTOSCALE_PROVISIONER` | `noop` | Which provisioner to use: `script`, `webhook`, or `noop`. |
| `KAPP_AUTOSCALE_WEBHOOK_URL` | — | Required when provisioner is `webhook`. |
| `KAPP_AUTOSCALE_PROVISION_SCRIPT` | `scripts/provision-cell.sh` | Script path for the `script` provisioner. |
| `KAPP_AUTOSCALE_PROVISION_TIMEOUT` | `5m` | Per-invocation timeout for the `script` provisioner. |
| `KAPP_AUTOSCALE_WEBHOOK_TIMEOUT` | `30s` | Per-request timeout for the `webhook` provisioner. |

With `KAPP_AUTOSCALE_PROVISION=true` but the provisioner left at `noop`,
the loop logs exactly what it *would* provision — a safe dry-run before
wiring real infrastructure.

### 3.2 Provisioner implementations

All three satisfy the `CellProvisioner` interface
(`Provision` / `Deprovision` / `Status`):

- **`NoopProvisioner`** (default) — logs the decision, mutates nothing.
- **`ScriptProvisioner`** — shells out to `scripts/provision-cell.sh`
  with `provision|deprovision|status` subcommands. The script's last
  stdout line must be the JSON result. This is the pragmatic choice for
  docker-compose and bare-metal fleets. See the script header for the
  exact contract.
- **`WebhookProvisioner`** — POSTs a JSON envelope
  (`{action, region, cell_id, spec}`) to `KAPP_AUTOSCALE_WEBHOOK_URL`
  and expects `{cell:{…}}` (provision) or `{status:{…}}` (status). This
  suits custom infrastructure where a small adapter service translates
  the request into Terraform / Pulumi / cloud-API calls.

### 3.3 What the loop does on each decision

- **`scale_up`** — calls `Provision(region, spec)` for the region of the
  cell that tripped the threshold. `spec.MaxTenants` comes from the
  policy. Each overloaded cell's decision is independent, so if **N**
  cells in a region trip `scale_up` in the same tick the loop issues
  **N** `Provision` calls (one per trigger cell). This is intentional —
  each is a genuinely overloaded cell — but operators sizing a region
  should be aware a single tick can add several cells.
- **`scale_down`** — drains the cell first, then deprovisions it (§3.4).
- **`hold`** — nothing.

Every provider interaction is **best-effort**: a failure is logged and
the loop continues to the next cell. A slow or broken provider can never
wedge the autoscaler. The decision is persisted to `platform_scale_events`
*before* actuation, and **only decisions whose audit row was written are
actuated** — so the audit trail is the authoritative record of what the
autoscaler acted on (no infrastructure change happens without a
corresponding `platform_scale_events` row). The autoscaler evaluates
cells whose `status` is `active` **or** `draining` — the latter so a
teardown that was deferred to a later tick resumes (§3.5); cells still
`provisioning` or already `deprovisioned` are skipped, and a `draining`
cell is never chosen as a drain target.

### 3.4 Safe scale-down: drain before teardown

A cell is **never** torn down while it still hosts tenants. On
`scale_down` the engine:

1. Skips the implicit `default` cell entirely (legacy / NULL-cell
   tenants live there; it is the placement of last resort).
2. If the cell is non-empty and a rebalancer is wired, **drains** it:
   migrates every tenant onto sibling cells **in the same region** that
   have spare capacity, spreading load onto the emptiest sibling first.
3. Re-checks the **live** tenant count (a fresh query, not the tick-old
   snapshot) and only calls `Deprovision` once the cell is genuinely
   empty. This closes the window where a tenant could be placed between
   the snapshot and teardown: such a late arrival is drained first (or,
   with no rebalancer, the cell is left `draining` for a later tick)
   rather than stranded on a cell about to be removed.

If there is insufficient sibling capacity in the region, the drain stops
and the cell is **left in place** — tenants are never stranded and never
moved across a region boundary automatically (that would change their
data residency; see §4). Draining is capped per tick (`maxDrainPerTick`)
and resumes on subsequent ticks.

Under the `noop` (observe-only) provisioner this whole `scale_down`
actuation is short-circuited: the loop logs the teardown it *would* run
and returns before any drain, so a dry run migrates **no** tenants and
writes **no** `cells.status` row (the rebalancer holds a real pool even in
dry-run wiring, so the engine — not the wiring — is what guarantees the
no-mutation contract). Switch to `script`/`webhook` to actually drain and
tear down. The draining-resume override (§3.5) is likewise suppressed under
`noop`: a cell left `draining` by a prior real-provisioner run is reported
with its natural policy decision rather than being forced to `scale_down`
every tick, so a dry run does not accumulate per-tick `platform_scale_events`
rows for a teardown it will never actuate.

Before any tenant is moved, a cell committed to teardown is marked
`status = 'draining'` (§3.5) so the cell-router stops placing new tenants
on it and it drops out of the `active` drain-target pool immediately.

When several cells in the **same region** scale_down in one tick, capacity
accounting is shared across all of their drains: as one drain places
tenants on a sibling, that sibling's headroom is decremented for the
remaining drains, so two concurrent drains can never collectively push a
sibling past its `max_tenants`. A cell that is itself being torn down — in
this tick (in-memory) or already persisted as `draining` from an earlier
tick — is also excluded as a drain target, so tenants are never moved onto
a cell that is about to be deprovisioned.

### 3.5 Who owns the `cells.status` lifecycle

The **autoscaler** owns the control-plane `cells.status` lifecycle
(`active` → `draining` → `deprovisioned`); the **provisioner** owns only
the matching *infrastructure* work (creating/destroying the cell's VMs,
containers, DNS, etc.). The split is deliberate: control-plane state and
the physical resource have different failure modes, and keeping the DB
transition in the orchestrator is what makes a multi-tick teardown
resumable.

On a `scale_down` the engine:

1. Marks the cell `draining` **before** it moves a single tenant. From
   that moment the cell-router stops placing new tenants on it, and the
   next tick's snapshot filter no longer treats it as a normal `active`
   cell or a drain target.
2. Drains it (capped at `maxDrainPerTick`). If the drain has to be
   deferred — tick cap reached, transient sibling-capacity shortage, or a
   migration error — the row **stays `draining`**. Because the snapshot
   query surfaces `draining` cells too, the next `Evaluate` re-selects it
   and forces a `scale_down` (regardless of its current metrics) so the
   teardown picks up exactly where it left off. No tenant is stranded.
3. Marks the cell `deprovisioned` only **after** `Deprovision` succeeds,
   at which point it drops out of evaluation entirely. If `Deprovision`
   fails the row stays `draining` and the next tick retries.

**Dry-run safety.** The `noop` provisioner opts out of these writes
(`observeOnly`), so running with `KAPP_AUTOSCALE_PROVISION=true` and the
`noop` provisioner still mutates **nothing** — neither infrastructure nor
the `cells.status` row. Lifecycle writes happen only when provisioning is
enabled with a provisioner that actually changes infrastructure
(`script` / `webhook`). A custom provisioner that prefers to manage the
row itself can do the same by implementing `observeOnly() bool { return
true }`.

---

## 4. Data residency: region assignment at signup

A tenant's region is assigned in the **setup wizard** (see
[ADMIN_GUIDE.md §11.1](./ADMIN_GUIDE.md)) and is a hard constraint:

- New-tenant placement only ever considers cells in the tenant's region.
- The autoscaler's automatic drain only moves tenants **within** their
  region. A cross-region move changes the legal location of the data
  (GDPR Art. 44–50, US-region-only contracts) and is therefore **always
  an explicit operator action**, never an automatic side effect.

### 4.1 Operator-driven cross-region migration

To relocate a tenant deliberately (e.g. a residency change request), an
operator uses the rebalancer's `MigrateTenant`. The intended HTTP
surface is:

```
POST /api/v1/admin/tenants/{id}/migrate-cell
{ "from_cell_id": "cell-eu-west-1", "to_cell_id": "cell-us-east-1" }
```

`MigrateTenant` repoints `tenants.cell_id`, invalidates the tenant
cache, and writes a `tenant.cell_migrated` audit entry — all in one
transaction, so the move and its audit record commit together. The move
is idempotent: replaying it after the tenant has already moved returns a
"not on source cell" error rather than corrupting state.

> **Note.** Migrating placement metadata does **not** copy the tenant's
> data between cell databases. A cross-region move must be paired with a
> data migration of the tenant's schema/rows (logical dump + restore, or
> the per-tenant export tooling). Repointing `cell_id` is the final
> cut-over step, performed during a maintenance window once the data is
> in place.

---

## 5. Cross-region failover: active-passive with PG streaming replication

For regional resilience, run each cell's PostgreSQL as a **primary with
a streaming replica in a second region** (active-passive):

```
  region us-east (active)                region us-west (passive)
  ┌─────────────────────┐   WAL stream   ┌─────────────────────┐
  │ cell-us-east-1       │ ─────────────▶ │ cell-us-east-1-dr    │
  │  RDS primary         │                │  RDS replica (RO)    │
  └─────────────────────┘                └─────────────────────┘
```

Failover procedure (see [DR_RUNBOOK.md](./DR_RUNBOOK.md) for the full
drill):

1. Promote the replica in the passive region to primary.
2. Update the cell's `endpoint` (and DNS) to point at the promoted
   instance.
3. Repoint the cell-router; tenants on that cell now resolve to the new
   region.
4. Re-establish replication in the reverse direction once the original
   region recovers.

Because the control plane is the routing authority, failover is a
metadata update (`cells.endpoint` + DNS) plus a database promotion — no
tenant rows move.

---

## 6. Terraform / Pulumi examples (AWS multi-region)

A minimal AWS layout: one VPC per region, one RDS instance per cell, a
shared Route 53 zone. These snippets are **illustrative skeletons** —
adapt naming, sizing, and security groups to your environment.

### 6.1 Terraform: VPC + RDS per region

```hcl
variable "regions" {
  type    = map(string)            # logical name → AWS region
  default = { us = "us-east-1", eu = "eu-west-1" }
}

# One provider alias per region.
provider "aws" {
  alias  = "us"
  region = var.regions["us"]
}
provider "aws" {
  alias  = "eu"
  region = var.regions["eu"]
}

module "cell_us" {
  source       = "./modules/cell"
  providers    = { aws = aws.us }
  cell_id      = "cell-us-east-1"
  region       = var.regions["us"]
  cidr_block   = "10.10.0.0/16"
  db_instance  = "db.r6g.xlarge"
  max_tenants  = 1000
}

module "cell_eu" {
  source       = "./modules/cell"
  providers    = { aws = aws.eu }
  cell_id      = "cell-eu-west-1"
  region       = var.regions["eu"]
  cidr_block   = "10.20.0.0/16"
  db_instance  = "db.r6g.xlarge"
  max_tenants  = 1000
}
```

`modules/cell/main.tf` (sketch):

```hcl
resource "aws_vpc" "this" {
  cidr_block = var.cidr_block
  tags       = { Name = var.cell_id, Cell = var.cell_id }
}

resource "aws_db_instance" "cell" {
  identifier        = var.cell_id
  engine            = "postgres"
  engine_version    = "16"
  instance_class    = var.db_instance
  allocated_storage = 100
  multi_az          = true            # in-region HA
  # Cross-region replica is declared separately as a replicate_source_db.
}

output "endpoint" {
  value = "https://${aws_db_instance.cell.address}"
}
```

### 6.2 Shared Route 53 (geo routing)

```hcl
resource "aws_route53_zone" "app" {
  name = "app.kapp.example.com"
}

resource "aws_route53_record" "us" {
  zone_id        = aws_route53_zone.app.zone_id
  name           = "app.kapp.example.com"
  type           = "A"
  set_identifier = "us-east"
  geolocation_routing_policy { continent = "NA" }
  alias {
    name                   = module.cell_us.alb_dns_name
    zone_id                = module.cell_us.alb_zone_id
    evaluate_target_health = true
  }
}

resource "aws_route53_record" "eu" {
  zone_id        = aws_route53_zone.app.zone_id
  name           = "app.kapp.example.com"
  type           = "A"
  set_identifier = "eu-west"
  geolocation_routing_policy { continent = "EU" }
  alias {
    name                   = module.cell_eu.alb_dns_name
    zone_id                = module.cell_eu.alb_zone_id
    evaluate_target_health = true
  }
}
```

### 6.3 Pulumi (TypeScript) equivalent

```ts
import * as aws from "@pulumi/aws";

const regions = { us: "us-east-1", eu: "eu-west-1" };

for (const [name, region] of Object.entries(regions)) {
  const provider = new aws.Provider(`aws-${name}`, { region });
  const vpc = new aws.ec2.Vpc(`vpc-${name}`,
    { cidrBlock: name === "us" ? "10.10.0.0/16" : "10.20.0.0/16" },
    { provider });
  const db = new aws.rds.Instance(`cell-${region}`, {
    engine: "postgres",
    engineVersion: "16",
    instanceClass: "db.r6g.xlarge",
    allocatedStorage: 100,
    multiAz: true,
  }, { provider });
  // Register the cell with the control plane (cells table) out-of-band,
  // e.g. via the provisioning webhook or scripts/provision-cell.sh.
  db.address.apply(addr => console.log(`cell-${region} endpoint: https://${addr}`));
}
```

### 6.4 Hooking IaC into the autoscaler

The autoscaler does not run Terraform itself. Wire it through a
provisioner:

- **`script`** — `scripts/provision-cell.sh provision <region> <max>`
  runs `terraform apply` for a cell module and prints the resulting cell
  JSON (including `endpoint`) on its last stdout line.
- **`webhook`** — a small service receives the POST and triggers a
  Terraform Cloud / Atlantis run, then returns the cell JSON.
- **`noop`** — the default dry-run: it logs a synthetic cell and inserts
  **nothing**. Because no row is created and no load is shed, an
  overloaded cell keeps tripping the threshold; the per-cell cooldown
  (`MinHoldBetweenScales`, default 10 min) bounds how often that repeats,
  so you get a periodic decision in the logs (every ~10 min while the cell
  stays hot), not a burst every tick. This is expected for a dry run —
  switch to `script`/`webhook` to actually add capacity.

For `script`/`webhook`, the provisioner is responsible for **inserting the
new cell row** into the `cells` table on `Provision` (region, endpoint,
and an initial `status` of `active`/`provisioning`) so the next autoscaler
tick and the cell-router can see it. The teardown side of the lifecycle
(`draining` → `deprovisioned`) is driven by the **autoscaler**, not the
provisioner (see §3.5); `Deprovision` only has to tear down the
infrastructure and be idempotent.

---

## 7. Operational checklist

- [ ] Each region has ≥ 2 active cells before enabling auto-`scale_down`
      (otherwise a region has no drain target and scale-down is a no-op).
- [ ] `KAPP_AUTOSCALE_PROVISION` starts `false`; enable with provisioner
      `noop` first to dry-run the decisions in logs.
- [ ] On scale_up the provisioner inserts the `cells` row (region,
      endpoint, initial `status`) so routing and the next tick observe the
      new cell; the autoscaler then owns the teardown status transitions.
- [ ] Cross-region tenant moves are performed by an operator with a
      paired data migration — never left to the autoscaler.
- [ ] PG streaming replicas exist for every cell that hosts
      residency-sensitive or production tenants (see DR_RUNBOOK).
