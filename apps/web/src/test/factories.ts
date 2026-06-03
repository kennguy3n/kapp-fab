// Test data factories for the apps/web unit + integration suites.
//
// Every factory returns a fully-populated object that satisfies the
// corresponding @kapp/client type, while letting callers override any
// subset of fields. IDs and timestamps are derived from a per-process
// monotonic counter (see `seq()`) rather than Date.now()/Math.random()
// so a test that snapshots a factory output is stable across runs and
// machines. Call `resetFactories()` in a beforeEach if a test asserts
// on the exact generated ids.
//
// These mirror the server fixtures the Go integration tests build, but
// only the slice the React pages actually read — so a page test can do
//   listRecords.mockResolvedValue([makeKRecord({ data: { title: "Acme" } })])
// without hand-writing the tenant_id / version / timestamp boilerplate
// the KRecord type requires.

import type {
  InsightsDashboard,
  InsightsQuery,
  InsightsShare,
  KRecord,
  KType,
  KTypeSchema,
  SavedView,
  Tenant,
  WorkflowRun,
} from "@kapp/client";

// Monotonic counter shared by every factory so generated ids are
// unique within a test file but deterministic across runs.
let counter = 0;
function seq(): number {
  counter += 1;
  return counter;
}

/** Reset the factory id counter. Call from beforeEach when a test
 *  asserts on the literal generated id strings. */
export function resetFactories(): void {
  counter = 0;
}

// A fixed epoch so generated created_at/updated_at are deterministic.
// 2024-01-01T00:00:00Z. Each call adds `seq()` seconds so ordering by
// timestamp matches creation order.
const EPOCH = Date.UTC(2024, 0, 1, 0, 0, 0);
function isoAt(offsetSeconds: number): string {
  return new Date(EPOCH + offsetSeconds * 1000).toISOString();
}

/** Deterministic, well-formed v4-shaped UUID seeded from `n`. Not a
 *  real random UUID — just a stable, unique, correctly-formatted id so
 *  code that splits/validates the UUID shape (e.g. RLS guards) accepts
 *  it. */
export function makeId(prefix: string, n: number = seq()): string {
  const hex = n.toString(16).padStart(12, "0");
  return `${prefix.padEnd(8, "0").slice(0, 8)}-0000-4000-8000-${hex}`;
}

export function makeTenant(overrides: Partial<Tenant> = {}): Tenant {
  const n = seq();
  return {
    id: makeId("tenant", n),
    slug: `tenant-${n}`,
    name: `Tenant ${n}`,
    cell: "cell-local",
    status: "active",
    plan: "pro",
    quota: null,
    created_at: isoAt(n),
    updated_at: isoAt(n),
    ...overrides,
  };
}

// There is no canonical User type in @kapp/client (auth is JWT-only and
// the server never ships a user record to the SPA), so the factory
// owns the minimal shape the UI-adjacent tests need: an actor that can
// be referenced as a record's created_by / a workflow actor_id / a
// share grantee.
export interface TestUser {
  id: string;
  tenant_id: string;
  email: string;
  display_name: string;
  roles: string[];
  created_at: string;
}

export function makeUser(overrides: Partial<TestUser> = {}): TestUser {
  const n = seq();
  return {
    id: makeId("user", n),
    tenant_id: makeId("tenant", 1),
    email: `user${n}@acme.example`,
    display_name: `User ${n}`,
    roles: ["member"],
    created_at: isoAt(n),
    ...overrides,
  };
}

// A small but realistic CRM-deal schema used as the default KType. The
// fields cover the input types KTypeForm renders (string / number /
// enum) plus a workflow so RightPane / workflow tests have transitions
// to assert on.
function defaultSchema(name: string): KTypeSchema {
  return {
    name,
    version: 1,
    fields: [
      { name: "title", type: "string", required: true },
      { name: "stage", type: "enum", values: ["open", "won", "lost"] },
      { name: "value", type: "number" },
    ],
    views: {
      list: { columns: ["title", "stage", "value"] },
    },
    workflow: {
      name: "deal",
      initial_state: "open",
      states: ["open", "won", "lost"],
      transitions: [
        { from: ["open"], to: "won", action: "win" },
        { from: ["open"], to: "lost", action: "lose" },
      ],
    },
  };
}

export function makeKType(overrides: Partial<KType> = {}): KType {
  const name = overrides.name ?? "crm.deal";
  const schema = overrides.schema ?? defaultSchema(name);
  return {
    name,
    version: overrides.version ?? schema.version ?? 1,
    schema,
  };
}

export function makeKRecord(overrides: Partial<KRecord> = {}): KRecord {
  const n = seq();
  return {
    id: makeId("record", n),
    tenant_id: makeId("tenant", 1),
    ktype: "crm.deal",
    ktype_version: 1,
    data: { title: `Deal ${n}`, stage: "open", value: 100 * n },
    status: "active",
    version: 1,
    created_at: isoAt(n),
    updated_at: isoAt(n),
    ...overrides,
  };
}

export function makeSavedView(overrides: Partial<SavedView> = {}): SavedView {
  const n = seq();
  return {
    id: makeId("view", n),
    tenant_id: makeId("tenant", 1),
    user_id: makeId("user", 1),
    ktype: "crm.deal",
    name: `View ${n}`,
    filters: {},
    sort: "",
    columns: [],
    is_default: false,
    shared: false,
    created_at: isoAt(n),
    updated_at: isoAt(n),
    ...overrides,
  };
}

export function makeWorkflowRun(
  overrides: Partial<WorkflowRun> = {},
): WorkflowRun {
  const n = seq();
  return {
    id: makeId("run", n),
    tenant_id: makeId("tenant", 1),
    workflow: "deal",
    record_id: makeId("record", 1),
    state: "open",
    history: [],
    created_at: isoAt(n),
    updated_at: isoAt(n),
    ...overrides,
  };
}

export function makeInsightsQuery(
  overrides: Partial<InsightsQuery> = {},
): InsightsQuery {
  const n = seq();
  return {
    id: makeId("query", n),
    tenant_id: makeId("tenant", 1),
    name: `Query ${n}`,
    mode: "visual",
    definition: { source: "ktype:crm.deal", columns: ["title", "value"] },
    cache_ttl_seconds: 300,
    created_by: makeId("user", 1),
    created_at: isoAt(n),
    updated_at: isoAt(n),
    ...overrides,
  };
}

export function makeInsightsDashboard(
  overrides: Partial<InsightsDashboard> = {},
): InsightsDashboard {
  const n = seq();
  return {
    id: makeId("dash", n),
    tenant_id: makeId("tenant", 1),
    name: `Dashboard ${n}`,
    layout: { linked_filters: {} },
    auto_refresh_seconds: 0,
    created_by: makeId("user", 1),
    created_at: isoAt(n),
    updated_at: isoAt(n),
    widgets: [],
    ...overrides,
  };
}

export function makeInsightsShare(
  overrides: Partial<InsightsShare> = {},
): InsightsShare {
  const n = seq();
  return {
    id: makeId("share", n),
    tenant_id: makeId("tenant", 1),
    resource_type: "query",
    resource_id: makeId("query", 1),
    grantee_type: "user",
    grantee: makeId("user", 2),
    permission: "view",
    created_at: isoAt(n),
    ...overrides,
  };
}
