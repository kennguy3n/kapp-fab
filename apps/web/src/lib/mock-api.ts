// Demo / screenshot mock API client.
//
// Exposes the same surface as `ApiClient` from `@kapp/client` but
// resolves to in-memory fixtures from `mock-data.ts`. Wired in by
// `lib/api.ts` when `import.meta.env.VITE_DEMO_MODE === "true"`. Only
// the subset of methods actually used by the UI is implemented in
// detail — anything else falls through a Proxy and returns a friendly
// stub so unconfigured calls don't blow up the page.

import type {
  Approval,
  ApiClient,
  AuditEntry,
  DashboardSummary,
  ExchangeRate,
  FinanceAccount,
  IncomeStatement,
  InsightsDashboard,
  InsightsDashboardBundle,
  InsightsQuery,
  InsightsRunResult,
  InventoryItem,
  InventoryValuationReport,
  InventoryWarehouse,
  JournalEntry,
  KRecord,
  KType,
  Plan,
  PlacementPolicy,
  RetentionPolicy,
  ReportResult,
  SLAPolicy,
  SavedReport,
  SavedView,
  SearchResponse,
  StockLevel,
  Tenant,
  TenantFeaturesResponse,
  TenantUsageHistoryResponse,
  TenantUsageResponse,
  TrialBalanceReport,
  Webhook,
  WebhookDelivery,
} from "@kapp/client";

import {
  ALL_KTYPES,
  APPROVALS,
  AUDIT_LOG,
  DASHBOARD_SUMMARY,
  DEMO_TENANT_ID,
  EXCHANGE_RATES,
  FINANCE_ACCOUNTS,
  INCOME_STATEMENT,
  INSIGHTS_DASHBOARDS,
  INSIGHTS_DASHBOARD_BUNDLE,
  INSIGHTS_QUERIES,
  INVENTORY_ITEMS,
  INVENTORY_VALUATION,
  INVENTORY_WAREHOUSES,
  JOURNAL_ENTRIES,
  PLACEMENT_POLICY,
  PLANS,
  PORTAL_TICKETS,
  RECORDS_BY_KTYPE,
  RETENTION_POLICIES,
  SAVED_REPORTS,
  SAVED_VIEWS_BY_KTYPE,
  SLA_POLICIES,
  STOCK_LEVELS,
  TENANTS,
  TENANT_FEATURES,
  TENANT_USAGE,
  TENANT_USAGE_HISTORY,
  TRIAL_BALANCE,
  WEBHOOKS,
  WEBHOOK_DELIVERIES,
  getKTypeByName,
  searchResults,
  widgetResultForQuery,
} from "./mock-data";

// 100–200ms artificial latency so loading skeletons flash briefly and
// the UI behaves like a real network round-trip.
async function delay<T>(value: T, ms = 120): Promise<T> {
  await new Promise((r) => setTimeout(r, ms));
  return value;
}

// installDemoLocalStorage primes the localStorage values that the
// app shell reads on mount (tenant id + dummy bearer token). Called
// once from `api.ts` before React renders.
export function installDemoLocalStorage(): void {
  if (typeof window === "undefined") return;
  if (!localStorage.getItem("kapp.tenant")) {
    localStorage.setItem("kapp.tenant", DEMO_TENANT_ID);
  }
  if (!localStorage.getItem("kapp.token")) {
    localStorage.setItem(
      "kapp.token",
      // Decoy JWT — three base64 segments so any code that splits
      // on "." gets three pieces. Not a real signed token.
      "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZW1vQGFjbWUuZXhhbXBsZSIsInRlbmFudF9pZCI6Ijk5OTk5OTk5LTk5OTktOTk5OS05OTk5LTk5OTk5OTk5OTk5OSJ9.demo-signature"
    );
  }
}

// Mutable demo state so create / update / delete actions round-trip
// inside the UI without re-mocking after each call.
const records: Record<string, KRecord[]> = {};
for (const [k, v] of Object.entries(RECORDS_BY_KTYPE)) {
  records[k] = [...v];
}

function nextId(): string {
  return `00000000-0000-4000-8000-${Math.floor(Math.random() * 1e12)
    .toString()
    .padStart(12, "0")}`;
}

function nowIso(): string {
  return new Date().toISOString();
}

// --- Method handlers --------------------------------------------------

const handlers = {
  // --- Tenants / features / placement / retention ----------------------
  listTenants: () => delay<Tenant[]>([...TENANTS]),
  listTenantFeatures: () => delay<TenantFeaturesResponse>({ ...TENANT_FEATURES }),
  updateTenantFeatures: (_tid: string, features: Record<string, boolean>) =>
    delay<TenantFeaturesResponse>({ tenant_id: DEMO_TENANT_ID, features }),
  getPlacementPolicy: () => delay<PlacementPolicy>({ ...PLACEMENT_POLICY }),
  updatePlacementPolicy: () => delay<PlacementPolicy>({ ...PLACEMENT_POLICY }),
  listRetentionPolicies: () => delay<{ policies: RetentionPolicy[] }>({ policies: [...RETENTION_POLICIES] }),
  upsertRetentionPolicy: (_tid: string, p: RetentionPolicy) =>
    delay<RetentionPolicy>({ ...p, tenant_id: DEMO_TENANT_ID, created_at: nowIso(), updated_at: nowIso() }),

  // --- Plans / usage --------------------------------------------------
  listPlans: () => delay<{ plans: Plan[] }>({ plans: [...PLANS] }),
  getTenantUsage: () => delay<TenantUsageResponse>({ ...TENANT_USAGE }),
  getTenantUsageHistory: () => delay<TenantUsageHistoryResponse>({ ...TENANT_USAGE_HISTORY }),

  // --- KTypes ---------------------------------------------------------
  listKTypes: () => delay<KType[]>([...ALL_KTYPES]),
  getKType: (name: string) => {
    const kt = getKTypeByName(name);
    if (!kt) {
      // Synthesize a minimal KType so the kanban / form pages still render
      // for previously unknown metadata names rather than crashing.
      return delay<KType>({
        name,
        version: 1,
        schema: { name, version: 1, fields: [{ name: "name", type: "string" }] },
      });
    }
    return delay<KType>(kt);
  },

  // --- Records --------------------------------------------------------
  listRecords: (ktype: string) => delay<KRecord[]>([...(records[ktype] ?? [])]),
  getRecord: (ktype: string, id: string) => {
    const r = (records[ktype] ?? []).find((x) => x.id === id);
    return delay<KRecord>(r ?? ({ id, tenant_id: DEMO_TENANT_ID, ktype, ktype_version: 1, data: {}, status: "active", version: 1, created_at: nowIso(), updated_at: nowIso() } as KRecord));
  },
  createRecord: (ktype: string, data: Record<string, unknown>) => {
    const r: KRecord = {
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      ktype,
      ktype_version: 1,
      data,
      status: "active",
      version: 1,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    records[ktype] = [...(records[ktype] ?? []), r];
    return delay<KRecord>(r);
  },
  updateRecord: (ktype: string, id: string, data: Record<string, unknown>) => {
    const list = records[ktype] ?? [];
    const idx = list.findIndex((x) => x.id === id);
    let updated: KRecord;
    if (idx === -1) {
      updated = { id, tenant_id: DEMO_TENANT_ID, ktype, ktype_version: 1, data, status: "active", version: 1, created_at: nowIso(), updated_at: nowIso() };
      records[ktype] = [...list, updated];
    } else {
      updated = { ...list[idx], data: { ...list[idx].data, ...data }, updated_at: nowIso(), version: list[idx].version + 1 };
      list[idx] = updated;
    }
    return delay<KRecord>(updated);
  },
  deleteRecord: () => delay<void>(undefined as unknown as void),
  bulkRecords: () => delay({ created: 0, updated: 0, deleted: 0, errors: [] }),
  bulkExportRecords: () => delay<KRecord[]>([]),
  recordPdf: () => delay<Blob>(new Blob()),
  recordHtml: () => delay<string>("<html><body>demo</body></html>"),
  runAction: (ktype: string, id: string, action: string) => {
    const list = records[ktype] ?? [];
    const idx = list.findIndex((x) => x.id === id);
    if (idx === -1) return delay<KRecord>({ id, tenant_id: DEMO_TENANT_ID, ktype, ktype_version: 1, data: {}, status: action, version: 1, created_at: nowIso(), updated_at: nowIso() });
    list[idx] = { ...list[idx], status: action, updated_at: nowIso() };
    return delay<KRecord>(list[idx]);
  },

  // --- Search ---------------------------------------------------------
  searchRecords: (params: { q: string }) => delay<SearchResponse>(searchResults(params.q ?? "")),

  // --- Saved views ----------------------------------------------------
  listViews: (ktype: string) => delay<SavedView[]>([...(SAVED_VIEWS_BY_KTYPE[ktype] ?? [])]),
  createView: () => delay<SavedView>({} as SavedView),
  updateView: () => delay<SavedView>({} as SavedView),
  deleteView: () => delay<void>(undefined as unknown as void),

  // --- Approvals ------------------------------------------------------
  listApprovals: () => delay<Approval[]>([...APPROVALS]),
  decideApproval: (id: string) => {
    const a = APPROVALS.find((x) => x.id === id);
    return delay<Approval>(a ?? APPROVALS[0]);
  },

  // --- Audit / webhooks ----------------------------------------------
  listAuditLog: () => delay<AuditEntry[]>([...AUDIT_LOG]),
  listWebhooks: () => delay<{ webhooks: Webhook[] }>({ webhooks: [...WEBHOOKS] }),
  getWebhook: (id: string) => delay<Webhook>(WEBHOOKS.find((w) => w.id === id) ?? WEBHOOKS[0]),
  createWebhook: (input: Partial<Webhook>) => {
    const wh: Webhook = {
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      url: input.url ?? "",
      secret: input.secret ?? "",
      event_filters: input.event_filters ?? [],
      conditions: input.conditions,
      max_retries: input.max_retries ?? 5,
      backoff_base_seconds: input.backoff_base_seconds ?? 10,
      active: true,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    WEBHOOKS.push(wh);
    return delay<Webhook>(wh);
  },
  updateWebhook: (id: string, patch: Partial<Webhook>) => {
    const i = WEBHOOKS.findIndex((w) => w.id === id);
    if (i === -1) return delay<Webhook>(WEBHOOKS[0]);
    WEBHOOKS[i] = { ...WEBHOOKS[i], ...patch, updated_at: nowIso() };
    return delay<Webhook>(WEBHOOKS[i]);
  },
  deleteWebhook: () => delay<void>(undefined as unknown as void),
  listWebhookDeliveries: (webhookId: string) =>
    delay<{ deliveries: WebhookDelivery[] }>({ deliveries: WEBHOOK_DELIVERIES.filter((d) => d.webhook_id === webhookId) }),

  // --- Finance --------------------------------------------------------
  listAccounts: () => delay<FinanceAccount[]>([...FINANCE_ACCOUNTS]),
  getAccount: (code: string) =>
    delay<FinanceAccount>(FINANCE_ACCOUNTS.find((a) => a.code === code) ?? FINANCE_ACCOUNTS[0]),
  listJournalEntries: () => delay<JournalEntry[]>([...JOURNAL_ENTRIES]),
  getTrialBalance: () => delay<TrialBalanceReport>({ ...TRIAL_BALANCE }),
  getIncomeStatement: () => delay<IncomeStatement>({ ...INCOME_STATEMENT }),
  getARAgingReport: () => delay({ as_of: TRIAL_BALANCE.as_of, currency: "USD", buckets: [], rows: [], total: "45000.00" }),
  getAPAgingReport: () => delay({ as_of: TRIAL_BALANCE.as_of, currency: "USD", buckets: [], rows: [], total: "18000.00" }),
  postInvoice: (id: string) => {
    const list = records["finance.ar_invoice"];
    const idx = list.findIndex((x) => x.id === id);
    if (idx === -1) return delay<KRecord>(list?.[0] ?? ({ id } as KRecord));
    list[idx] = { ...list[idx], data: { ...list[idx].data, status: "posted" }, updated_at: nowIso() };
    return delay<KRecord>(list[idx]);
  },
  postBill: (id: string) => {
    const list = records["finance.ap_bill"];
    const idx = list.findIndex((x) => x.id === id);
    if (idx === -1) return delay<KRecord>(list?.[0] ?? ({ id } as KRecord));
    list[idx] = { ...list[idx], data: { ...list[idx].data, status: "posted" }, updated_at: nowIso() };
    return delay<KRecord>(list[idx]);
  },
  listExchangeRates: () => delay<{ rates: ExchangeRate[] }>({ rates: [...EXCHANGE_RATES] }),
  upsertExchangeRate: (input: Partial<ExchangeRate>) => {
    const er: ExchangeRate = {
      tenant_id: DEMO_TENANT_ID,
      from_currency: input.from_currency ?? "USD",
      to_currency: input.to_currency ?? "USD",
      rate_date: input.rate_date ?? new Date().toISOString().slice(0, 10),
      rate: input.rate ?? "1.0",
      provider: input.provider,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    EXCHANGE_RATES.unshift(er);
    return delay<ExchangeRate>(er);
  },

  // --- Inventory ------------------------------------------------------
  listInventoryItems: () => delay<InventoryItem[]>([...INVENTORY_ITEMS]),
  listInventoryWarehouses: () => delay<InventoryWarehouse[]>([...INVENTORY_WAREHOUSES]),
  listStockLevels: () => delay<StockLevel[]>([...STOCK_LEVELS]),
  getInventoryValuation: () => delay<InventoryValuationReport>({ ...INVENTORY_VALUATION }),
  listInventoryBatchesByItem: () => delay<KRecord[]>([]),

  // --- POS ------------------------------------------------------------
  finalizePOSInvoice: () => delay<KRecord>({ id: nextId(), tenant_id: DEMO_TENANT_ID, ktype: "sales.pos_invoice", ktype_version: 1, data: { status: "finalized" }, status: "finalized", version: 1, created_at: nowIso(), updated_at: nowIso() }),

  // --- Helpdesk -------------------------------------------------------
  listSLAPolicies: () => delay<{ policies: SLAPolicy[] }>({ policies: [...SLA_POLICIES] }),
  upsertSLAPolicy: (input: Partial<SLAPolicy>) => {
    const p: SLAPolicy = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      name: input.name ?? "Standard",
      priority: (input.priority as SLAPolicy["priority"]) ?? "medium",
      response_minutes: input.response_minutes ?? 60,
      resolution_minutes: input.resolution_minutes ?? 480,
      active: input.active ?? true,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    SLA_POLICIES.push(p);
    return delay<SLAPolicy>(p);
  },

  // --- Reports & dashboard summary -----------------------------------
  listReports: () => delay<{ reports: SavedReport[] }>({ reports: [...SAVED_REPORTS] }),
  runAdhocReport: () =>
    delay<ReportResult>({
      columns: ["name", "stage", "value"],
      rows: [
        { name: "Hooli — Enterprise Tier", stage: "proposal", value: 124000 },
        { name: "Umbrella — POS Rollout", stage: "negotiation", value: 67500 },
        { name: "Globex — Annual License", stage: "prospecting", value: 42000 },
        { name: "Globex — Q1 Renewal", stage: "closed_won", value: 36000 },
        { name: "Initech — Pilot Expansion", stage: "qualification", value: 18000 },
      ],
    }),
  createReport: (input: Partial<SavedReport>) => {
    const r: SavedReport = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      name: input.name ?? "Untitled report",
      description: input.description ?? "",
      definition: input.definition!,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    SAVED_REPORTS.push(r);
    return delay<SavedReport>(r);
  },
  getDashboardSummary: () => delay<DashboardSummary>({ ...DASHBOARD_SUMMARY }),

  // --- Insights -------------------------------------------------------
  listInsightsQueries: () => delay<{ queries: InsightsQuery[] }>({ queries: [...INSIGHTS_QUERIES] }),
  getInsightsQuery: (id: string) =>
    delay<InsightsQuery>(INSIGHTS_QUERIES.find((q) => q.id === id) ?? INSIGHTS_QUERIES[0]),
  createInsightsQuery: (input: Partial<InsightsQuery>) => {
    const q: InsightsQuery = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      name: input.name ?? "Untitled query",
      description: input.description,
      definition: input.definition!,
      mode: input.mode ?? "visual",
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    INSIGHTS_QUERIES.push(q);
    return delay<InsightsQuery>(q);
  },
  updateInsightsQuery: (id: string, input: Partial<InsightsQuery>) => {
    const i = INSIGHTS_QUERIES.findIndex((q) => q.id === id);
    if (i === -1) return delay<InsightsQuery>(INSIGHTS_QUERIES[0]);
    INSIGHTS_QUERIES[i] = { ...INSIGHTS_QUERIES[i], ...input, updated_at: nowIso() };
    return delay<InsightsQuery>(INSIGHTS_QUERIES[i]);
  },
  deleteInsightsQuery: () => delay<void>(undefined as unknown as void),
  runInsightsQuery: (id: string) =>
    delay<InsightsRunResult>(
      widgetResultForQuery(id) ?? {
        result: { columns: ["value"], rows: [{ value: 0 }] },
        cache_hit: false,
        query_hash: "h-default",
        filter_hash: "f-default",
        expires_at: null,
      }
    ),
  runInsightsQuerySQL: () =>
    delay<InsightsRunResult>({
      result: { columns: ["value"], rows: [{ value: 0 }] },
      cache_hit: false,
      query_hash: "h-sql",
      filter_hash: "f-sql",
      expires_at: null,
    }),
  listInsightsQueryShares: () => delay<{ shares: [] }>({ shares: [] }),
  shareInsightsQuery: () => delay<{ share: null }>({ share: null }),
  deleteInsightsQueryShare: () => delay<void>(undefined as unknown as void),
  listInsightsDashboards: () =>
    delay<{ dashboards: InsightsDashboard[] }>({ dashboards: [...INSIGHTS_DASHBOARDS] }),
  getInsightsDashboard: () => delay<InsightsDashboardBundle>({ ...INSIGHTS_DASHBOARD_BUNDLE }),
  createInsightsDashboard: (input: Partial<InsightsDashboard>) => {
    const d: InsightsDashboard = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      name: input.name ?? "Untitled dashboard",
      description: input.description,
      layout: input.layout ?? { linked_filters: {} },
      auto_refresh_seconds: input.auto_refresh_seconds ?? 0,
      created_at: nowIso(),
      updated_at: nowIso(),
      widgets: [],
    };
    INSIGHTS_DASHBOARDS.push(d);
    return delay<InsightsDashboard>(d);
  },
  updateInsightsDashboard: () => delay<InsightsDashboard>(INSIGHTS_DASHBOARDS[0]),
  deleteInsightsDashboard: () => delay<void>(undefined as unknown as void),
  upsertInsightsWidget: () => delay<InsightsDashboard>(INSIGHTS_DASHBOARDS[0]),
  deleteInsightsWidget: () => delay<void>(undefined as unknown as void),
  listInsightsDashboardShares: () => delay<{ shares: [] }>({ shares: [] }),
  shareInsightsDashboard: () => delay<{ share: null }>({ share: null }),
  deleteInsightsDashboardShare: () => delay<void>(undefined as unknown as void),
  listInsightsDataSources: () => delay<{ data_sources: [] }>({ data_sources: [] }),
  createInsightsDataSource: () => delay({}),
  updateInsightsDataSource: () => delay({}),
  deleteInsightsDataSource: () => delay<void>(undefined as unknown as void),
  testInsightsDataSource: () => delay({ ok: true }),

  // --- Misc fallbacks -------------------------------------------------
  getPublicForm: () => delay({ id: "demo-form", title: "Demo form", fields: [] }),
  submitPublicForm: () => delay({ ok: true }),
  generatePayslips: () => delay({ created: 5, updated: 0 }),
  postPayRun: () => delay({ posted: true }),
  listPayRunPayslips: (id: string) =>
    delay<KRecord[]>(records["hr.payslip"]?.filter((p) => (p.data as { pay_run_id?: string }).pay_run_id === id) ?? []),
  createConsolidationGroup: () => delay({ id: nextId(), name: "" }),
  runConsolidation: () => delay({ ok: true }),
  getWorkflowRun: () => delay({ steps: [], status: "completed" }),
} as unknown as Record<string, (...args: unknown[]) => unknown>;

// Wrap in a Proxy so any unimplemented method becomes a no-op rather
// than throwing — this keeps the demo resilient as new endpoints
// land in `ApiClient` without forcing us to update mock-api.ts in
// lockstep.
export const mockApi = new Proxy({} as ApiClient, {
  get(_target, prop: string | symbol) {
    if (typeof prop !== "string") return undefined;
    const handler = handlers[prop];
    if (handler) return handler;
    return async (..._args: unknown[]) => {
      // eslint-disable-next-line no-console
      console.warn(`[mock-api] unimplemented method: ${prop} — returning null`);
      return null;
    };
  },
}) as ApiClient;

export const PORTAL_TICKETS_FIXTURE = PORTAL_TICKETS;

// installPortalDemoFetch overrides window.fetch for /api/v1/portal/*
// requests so the customer portal pages work without a real backend.
// Limited surface: list tickets, get ticket, request/verify magic link.
export function installPortalDemoFetch(): void {
  if (typeof window === "undefined") return;
  const origFetch = window.fetch.bind(window);
  window.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    if (!url.includes("/api/v1/portal")) {
      return origFetch(input as RequestInfo, init);
    }
    await new Promise((r) => setTimeout(r, 80));
    if (url.endsWith("/portal/auth/request")) {
      return new Response(null, { status: 204 });
    }
    if (url.endsWith("/portal/auth/verify")) {
      return new Response(
        JSON.stringify({
          token: "demo-portal-token",
          expires_at: Date.now() / 1000 + 3600,
          user: { id: "demo-user", tenant_id: DEMO_TENANT_ID, email: "buyer@globex.example", display_name: "Globex Buyer" },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } }
      );
    }
    if (url.endsWith("/portal/tickets/")) {
      return new Response(JSON.stringify({ tickets: PORTAL_TICKETS }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    if (url.includes("/portal/tickets/")) {
      const id = url.split("/portal/tickets/")[1].split(/[/?#]/)[0];
      const t = PORTAL_TICKETS.find((x) => x.id === id) ?? PORTAL_TICKETS[0];
      return new Response(JSON.stringify(t), { status: 200, headers: { "Content-Type": "application/json" } });
    }
    return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
  }) as typeof window.fetch;
}
