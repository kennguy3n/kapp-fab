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
  BOM,
  Budget,
  BudgetLine,
  BudgetLineInput,
  BudgetVarianceReport,
  CapacityPlan,
  CreateBOMInput,
  CreateBudgetInput,
  CreateRoutingInput,
  CreateSubcontractOrderInput,
  CreateWorkCenterInput,
  CreateWorkOrderInput,
  UpdateBudgetInput,
  CycleCountLine,
  CycleCountSession,
  CycleCountSessionWithLines,
  DashboardSummary,
  ExchangeRate,
  FinanceAccount,
  IncomeStatement,
  InsightsDashboard,
  InsightsDashboardBundle,
  InsightsDataSource,
  InsightsDataSourceInput,
  InsightsQuery,
  InsightsRunResult,
  InsightsShare,
  InsightsShareInput,
  InsightsWidget,
  InsightsWidgetInput,
  InstallMarketplaceExtensionInput,
  InstallMarketplaceExtensionResponse,
  InventoryItem,
  InventoryValuationReport,
  InventoryWarehouse,
  Interview,
  CompleteInterviewInput,
  CreateInterviewInput,
  JobApplication,
  CreateApplicationInput,
  UpdateApplicationInput,
  JobCard,
  JobOpening,
  JobOpeningInput,
  JournalEntry,
  KRecord,
  KType,
  LearningPath,
  LearningPathCourse,
  Badge,
  BadgeAward,
  LandedCostCharge,
  LandedCostPostResult,
  LandedCostTarget,
  LandedCostVoucher,
  LandedCostVoucherWithLines,
  UpsertLandedCostVoucherInput,
  UpsertLandedCostChargeInput,
  UpsertLandedCostTargetInput,
  MarketplaceExtension,
  MarketplaceGetExtensionResponse,
  MarketplaceInstallation,
  MarketplaceListExtensionsOptions,
  MarketplaceListExtensionsResponse,
  MarketplaceListInstallationsResponse,
  MarketplaceListVersionsResponse,
  MarketplaceRatingSummary,
  MarketplaceUpdateSettingsResponse,
  MRPRun,
  PayslipGenerateResult,
  Plan,
  PlacementPolicy,
  RetentionPolicy,
  ReportResult,
  Routing,
  RunMRPInput,
  SLAPolicy,
  SavedReport,
  SavedView,
  SearchResponse,
  StockLevel,
  SubcontractOrder,
  Tenant,
  TenantFeaturesResponse,
  TenantUsageHistoryResponse,
  TenantUsageResponse,
  TrialBalanceReport,
  UpgradeMarketplaceInstallationInput,
  UpgradeMarketplaceInstallationResponse,
  Webhook,
  WebhookDelivery,
  WorkCenter,
  WorkOrder,
} from "@kapp/client";

import {
  ALL_KTYPES,
  APPROVALS,
  AUDIT_LOG,
  BANK_FEED_RULES_FIXTURE,
  BANK_FEED_SUGGESTIONS_FIXTURE,
  BOMS,
  BUDGETS,
  BUDGET_LINES_BY_ID,
  buildBudgetVariance,
  buildCapacityPlan,
  CYCLE_COUNT_LINES_BY_SESSION,
  CYCLE_COUNT_SESSIONS,
  DASHBOARD_SUMMARY,
  DEMO_BASE_CURRENCY,
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
  JOB_APPLICATIONS,
  JOB_CARDS_BY_WO,
  JOB_OPENINGS,
  INTERVIEWS,
  JOURNAL_ENTRIES,
  LEARNING_PATHS,
  LEARNING_PATH_COURSES_BY_PATH,
  BADGES,
  BADGE_AWARDS,
  LANDED_COST_CHARGES_BY_VOUCHER,
  LANDED_COST_TARGETS_BY_VOUCHER,
  LANDED_COST_VOUCHERS,
  MARKETPLACE_EXTENSIONS,
  MARKETPLACE_INSTALLATIONS,
  MARKETPLACE_MY_RATINGS,
  MARKETPLACE_VERSIONS,
  MRP_RUNS,
  PLACEMENT_POLICY,
  PLANS,
  PORTAL_TICKETS,
  RECORDS_BY_KTYPE,
  RETENTION_POLICIES,
  ROUTINGS,
  SAVED_REPORTS,
  SAVED_VIEWS_BY_KTYPE,
  SLA_POLICIES,
  STOCK_LEVELS,
  SUBCONTRACT_ORDERS,
  TENANTS,
  TENANT_FEATURES,
  TENANT_USAGE,
  TENANT_USAGE_HISTORY,
  TRIAL_BALANCE,
  WEBHOOKS,
  WEBHOOK_DELIVERIES,
  WORK_CENTERS,
  WORK_ORDERS,
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

// Mutable bank-feed review state so accept/reject round-trips in the
// demo: accepting marks the matched bank line and clears its candidate
// suggestions, rejecting just clears the one candidate.
let bankFeedSuggestions = [...BANK_FEED_SUGGESTIONS_FIXTURE];

// Mutable budget demo state so create / update / delete + line edits
// round-trip inside the UI. Cloned from the fixtures so reloading the
// page resets to the seeded plans.
const budgets: Budget[] = BUDGETS.map((b) => ({ ...b }));
const budgetLines: Record<string, BudgetLine[]> = {};
for (const [k, v] of Object.entries(BUDGET_LINES_BY_ID)) {
  budgetLines[k] = v.map((l) => ({ ...l }));
}

// Mutable manufacturing demo state so create + lifecycle transitions
// (release / start / complete / issue / receive …) round-trip in the
// UI. Cloned from the fixtures so a page reload resets to the seed.
const workCenters: WorkCenter[] = WORK_CENTERS.map((w) => ({ ...w }));
const boms: BOM[] = BOMS.map((b) => ({ ...b }));
const routings: Routing[] = ROUTINGS.map((r) => ({ ...r }));
const workOrders: WorkOrder[] = WORK_ORDERS.map((w) => ({ ...w }));
const jobCardsByWO: Record<string, JobCard[]> = {};
for (const [k, v] of Object.entries(JOB_CARDS_BY_WO)) {
  jobCardsByWO[k] = v.map((c) => ({ ...c }));
}
const mrpRuns: MRPRun[] = MRP_RUNS.map((r) => ({ ...r }));
const subcontractOrders: SubcontractOrder[] = SUBCONTRACT_ORDERS.map((o) => ({
  ...o,
}));

// Mutable inventory demo state for landed-cost vouchers and
// cycle-count sessions so create + allocate / post / count edits
// round-trip in the UI. Cloned from the fixtures so a reload resets.
const landedCostVouchers: LandedCostVoucher[] = LANDED_COST_VOUCHERS.map(
  (v) => ({ ...v }),
);
const landedCostCharges: Record<string, LandedCostCharge[]> = {};
for (const [k, v] of Object.entries(LANDED_COST_CHARGES_BY_VOUCHER)) {
  landedCostCharges[k] = v.map((c) => ({ ...c }));
}
const landedCostTargets: Record<string, LandedCostTarget[]> = {};
for (const [k, v] of Object.entries(LANDED_COST_TARGETS_BY_VOUCHER)) {
  landedCostTargets[k] = v.map((t) => ({ ...t }));
}
const cycleCountSessions: CycleCountSession[] = CYCLE_COUNT_SESSIONS.map(
  (s) => ({ ...s }),
);
const cycleCountLines: Record<string, CycleCountLine[]> = {};
for (const [k, v] of Object.entries(CYCLE_COUNT_LINES_BY_SESSION)) {
  cycleCountLines[k] = v.map((l) => ({ ...l }));
}

// Mutable recruitment demo state so create + lifecycle transitions
// (publish / close, advance / reject, schedule / complete interview)
// round-trip in the UI. Cloned from the fixtures so a reload resets.
const jobOpenings: JobOpening[] = JOB_OPENINGS.map((o) => ({ ...o }));
const applications: JobApplication[] = JOB_APPLICATIONS.map((a) => ({ ...a }));
const interviews: Interview[] = INTERVIEWS.map((i) => ({ ...i }));

// Mutable LMS demo state so creating a learning path round-trips in
// the UI. Cloned from the fixtures so a reload resets.
const learningPaths: LearningPath[] = LEARNING_PATHS.map((p) => ({ ...p }));

// Mutable marketplace demo state so install / uninstall / upgrade /
// rate actions round-trip inside the UI. Cloned from the fixtures so
// reloading the page resets to the seeded catalogue. Rating updates the
// cross-tenant rollup on the cloned extension so Browse + Detail
// reflect the new average immediately.
const mktExtensions: MarketplaceExtension[] = MARKETPLACE_EXTENSIONS.map(
  (e) => ({ ...e }),
);
let mktInstallations: MarketplaceInstallation[] = MARKETPLACE_INSTALLATIONS.map(
  (i) => ({ ...i }),
);
const mktMyRatings: Record<string, number> = { ...MARKETPLACE_MY_RATINGS };

function findExtension(extId: string): MarketplaceExtension {
  return mktExtensions.find((e) => e.id === extId) ?? mktExtensions[0]!;
}

function findInstallation(installId: string): MarketplaceInstallation {
  return (
    mktInstallations.find((i) => i.id === installId) ??
    syntheticInstallation(installId)
  );
}

// syntheticInstallation backs the demo when an install id can't be
// resolved — e.g. every seeded installation has been uninstalled, so
// mktInstallations is empty. Returning a valid, mutable object (rather
// than mktInstallations[0], which would be undefined) keeps the
// settings/upgrade round-trips from operating on undefined and
// crashing the demo, matching the mock's "always return something"
// posture (see getTenant / findExtension).
function syntheticInstallation(installId: string): MarketplaceInstallation {
  return {
    id: installId,
    tenant_id: DEMO_TENANT_ID,
    extension_id: mktExtensions[0]?.id ?? "",
    extension_version_id: "",
    status: "active",
    settings: {},
    webhook_base: "",
    installed_at: nowIso(),
    updated_at: nowIso(),
    last_health_check_at: nowIso(),
    last_health_check_status: "ok",
  };
}

function nextId(): string {
  return `00000000-0000-4000-8000-${Math.floor(Math.random() * 1e12)
    .toString()
    .padStart(12, "0")}`;
}

function nowIso(): string {
  return new Date().toISOString();
}

function makeDemoDataSource(input: InsightsDataSourceInput): InsightsDataSource {
  return {
    tenant_id: DEMO_TENANT_ID,
    id: nextId(),
    name: input.name,
    description: input.description,
    dialect: input.dialect,
    // Server returns plaintext only on create/update; mirror that here.
    connection_string: input.connection_string,
    enabled: input.enabled ?? true,
    created_at: nowIso(),
    updated_at: nowIso(),
  };
}

// --- Method handlers --------------------------------------------------

const handlers = {
  // --- Tenants / features / placement / retention ----------------------
  listTenants: () => delay<Tenant[]>([...TENANTS]),
  getTenant: (id: string) =>
    delay<Tenant>(TENANTS.find((t) => t.id === id) ?? TENANTS[0]!),
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
  bulkRecords: (_ktype: string, input: { ids: string[] }) =>
    delay<{ succeeded: string[]; failed: { id: string; error: string }[] }>({
      succeeded: [...(input?.ids ?? [])],
      failed: [],
    }),
  bulkExportRecords: (ktype: string, ids: string[]) => {
    // Mirror the real client's CSV-text response so callers that wrap
    // it in `new Blob([csv])` produce a valid download in demo mode.
    const list = (records[ktype] ?? []).filter((r) => ids.includes(r.id));
    const header = "id,status,updated_at";
    const body = list
      .map((r) => `${r.id},${r.status},${r.updated_at}`)
      .join("\n");
    return delay<string>(list.length ? `${header}\n${body}\n` : `${header}\n`);
  },
  recordPdf: () => delay<Blob>(new Blob(["%PDF-1.4 demo"], { type: "application/pdf" })),
  recordHtml: () =>
    delay<Blob>(
      new Blob(["<html><body>demo</body></html>"], { type: "text/html" })
    ),
  runAction: (ktype: string, id: string, action: string) => {
    const list = records[ktype] ?? [];
    const idx = list.findIndex((x) => x.id === id);
    if (idx === -1) return delay<KRecord>({ id, tenant_id: DEMO_TENANT_ID, ktype, ktype_version: 1, data: {}, status: action, version: 1, created_at: nowIso(), updated_at: nowIso() });
    list[idx] = { ...list[idx], status: action, updated_at: nowIso() };
    return delay<KRecord>(list[idx]);
  },

  // --- Bank feeds / reconciliation ------------------------------------
  listBankFeedSuggestions: (bankAccountId: string) => {
    const txnIds = new Set(
      (records["finance.bank_transaction"] ?? [])
        .filter((r) => (r.data as { bank_account_id?: string }).bank_account_id === bankAccountId)
        .map((r) => r.id),
    );
    return delay(
      bankFeedSuggestions.filter((s) => txnIds.has(s.transaction_id)),
    );
  },
  acceptBankFeedSuggestion: (id: string) => {
    const accepted = bankFeedSuggestions.find((s) => s.id === id);
    if (accepted) {
      const list = records["finance.bank_transaction"] ?? [];
      const idx = list.findIndex((r) => r.id === accepted.transaction_id);
      if (idx !== -1) {
        list[idx] = {
          ...list[idx],
          data: { ...list[idx].data, status: "matched", matched_entry_id: accepted.journal_entry_id },
          updated_at: nowIso(),
        };
      }
      // Clear every candidate for the now-matched line.
      bankFeedSuggestions = bankFeedSuggestions.filter(
        (s) => s.transaction_id !== accepted.transaction_id,
      );
    }
    return delay(accepted ?? bankFeedSuggestions[0] ?? null);
  },
  rejectBankFeedSuggestion: (id: string) => {
    bankFeedSuggestions = bankFeedSuggestions.filter((s) => s.id !== id);
    return delay<void>(undefined as unknown as void);
  },
  acceptBankFeedSplit: (
    transactionId: string,
    allocations: ReadonlyArray<{
      journal_entry_id: string;
      amount: string;
      suggestion_id?: string;
    }>,
  ) => {
    const list = records["finance.bank_transaction"] ?? [];
    const idx = list.findIndex((r) => r.id === transactionId);
    // A split clears matched_entry_id (NULL): the allocations are the
    // source of truth, mirroring the server projection.
    if (idx !== -1) {
      const { matched_entry_id: _drop, ...rest } = list[idx].data as Record<string, unknown>;
      list[idx] = {
        ...list[idx],
        data: { ...rest, status: "matched" },
        updated_at: nowIso(),
      };
    }
    // Collapse every candidate for the now-reconciled line.
    bankFeedSuggestions = bankFeedSuggestions.filter(
      (s) => s.transaction_id !== transactionId,
    );
    const txn = idx !== -1 ? list[idx] : undefined;
    // tenant_id is a top-level KRecord field; bank_account_id lives in data.
    const data = (txn?.data ?? {}) as Record<string, unknown>;
    return delay({
      id: transactionId,
      tenant_id: txn?.tenant_id ?? "",
      bank_account_id: (data.bank_account_id as string) ?? "",
      amount: String(data.amount ?? allocations.reduce((a, x) => a + Number(x.amount), 0)),
      currency: (data.currency as string) ?? "USD",
      status: "matched",
    });
  },
  listBankFeedRules: () => delay([...BANK_FEED_RULES_FIXTURE]),

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
  // --- Finance: budgets -----------------------------------------------
  listBudgets: () => delay<Budget[]>([...budgets]),
  getBudget: (id: string) =>
    delay<Budget>(budgets.find((b) => b.id === id) ?? budgets[0]),
  createBudget: (input: CreateBudgetInput) => {
    const now = nowIso();
    const b: Budget = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      name: input.name,
      fiscal_year: input.fiscal_year,
      status: input.status ?? "draft",
      cost_center: input.cost_center,
      notes: input.notes,
      variance_threshold: input.variance_threshold ?? null,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
    };
    budgets.unshift(b);
    budgetLines[b.id] = [];
    return delay<Budget>(b);
  },
  updateBudget: (id: string, input: UpdateBudgetInput) => {
    const idx = budgets.findIndex((b) => b.id === id);
    if (idx === -1) return delay<Budget>(budgets[0]);
    budgets[idx] = {
      ...budgets[idx],
      name: input.name,
      status: input.status,
      cost_center: input.cost_center,
      notes: input.notes,
      variance_threshold: input.variance_threshold ?? null,
      updated_at: nowIso(),
    };
    return delay<Budget>(budgets[idx]);
  },
  deleteBudget: (id: string) => {
    const idx = budgets.findIndex((b) => b.id === id);
    if (idx !== -1) budgets.splice(idx, 1);
    delete budgetLines[id];
    return delay<void>(undefined as unknown as void);
  },
  listBudgetLines: (budgetId: string) =>
    delay<BudgetLine[]>([...(budgetLines[budgetId] ?? [])]),
  upsertBudgetLine: (budgetId: string, input: BudgetLineInput) => {
    const list = budgetLines[budgetId] ?? (budgetLines[budgetId] = []);
    const months = input.months.slice(0, 12);
    const annual = months
      .reduce((sum, m) => sum + (Number(m) || 0), 0)
      .toFixed(2);
    const now = nowIso();
    if (input.id) {
      const idx = list.findIndex((l) => l.id === input.id);
      if (idx !== -1) {
        list[idx] = {
          ...list[idx],
          account_code: input.account_code,
          cost_center: input.cost_center,
          months,
          annual_total: annual,
          updated_at: now,
        };
        return delay<BudgetLine>(list[idx]);
      }
    }
    const line: BudgetLine = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      budget_id: budgetId,
      account_code: input.account_code,
      cost_center: input.cost_center,
      months,
      annual_total: annual,
      created_at: now,
      updated_at: now,
    };
    list.push(line);
    return delay<BudgetLine>(line);
  },
  deleteBudgetLine: (budgetId: string, lineId: string) => {
    const list = budgetLines[budgetId] ?? [];
    const idx = list.findIndex((l) => l.id === lineId);
    if (idx !== -1) list.splice(idx, 1);
    return delay<void>(undefined as unknown as void);
  },
  budgetVariance: (budgetId: string) => {
    const b = budgets.find((x) => x.id === budgetId) ?? budgets[0];
    return delay<BudgetVarianceReport>(
      buildBudgetVariance(b, budgetLines[b.id] ?? []),
    );
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

  // --- Manufacturing: work centers ------------------------------------
  listWorkCenters: (status?: string) =>
    delay<WorkCenter[]>(
      workCenters.filter((w) => !status || w.status === status).map((w) => ({ ...w })),
    ),
  getWorkCenter: (id: string) =>
    delay<WorkCenter>(workCenters.find((w) => w.id === id) ?? workCenters[0]),
  createWorkCenter: (input: CreateWorkCenterInput) => {
    const now = nowIso();
    const wc: WorkCenter = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      name: input.name,
      capacity_per_hour: input.capacity_per_hour,
      operating_hours_per_day: input.operating_hours_per_day,
      efficiency_percent: input.efficiency_percent,
      status: "active",
      notes: input.notes,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
    };
    workCenters.unshift(wc);
    return delay<WorkCenter>(wc);
  },
  setWorkCenterStatus: (id: string, status: string) => {
    const wc = workCenters.find((w) => w.id === id);
    if (wc) {
      wc.status = status as WorkCenter["status"];
      wc.updated_at = nowIso();
    }
    return delay<WorkCenter>(wc ?? workCenters[0]);
  },

  // --- Manufacturing: BOMs --------------------------------------------
  listBOMs: (status?: string) =>
    delay<BOM[]>(
      boms
        .filter((b) => !status || b.status === status)
        .map(({ components: _omit, ...rest }) => ({ ...rest })),
    ),
  getBOM: (id: string) => {
    const b = boms.find((x) => x.id === id) ?? boms[0];
    return delay<BOM>({ ...b, components: (b.components ?? []).map((c) => ({ ...c })) });
  },
  createBOM: (input: CreateBOMInput) => {
    const now = nowIso();
    const id = nextId();
    const activate = input.activate ?? false;
    if (activate) {
      // Activating a new BOM demotes any currently-active BOM for the
      // same item to obsolete, mirroring the server's single-active rule.
      boms.forEach((b) => {
        if (b.item_id === input.item_id && b.status === "active") {
          b.status = "obsolete";
          b.updated_at = now;
        }
      });
    }
    const bom: BOM = {
      tenant_id: DEMO_TENANT_ID,
      id,
      item_id: input.item_id,
      version: input.version,
      status: activate ? "active" : "draft",
      output_qty: input.output_qty,
      uom: input.uom,
      notes: input.notes,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
      components: input.components.map((c, i) => ({
        bom_id: id,
        component_item_id: c.component_item_id,
        qty: c.qty,
        uom: c.uom,
        scrap_percent: c.scrap_percent ?? null,
        sort_order: i + 1,
      })),
    };
    boms.unshift(bom);
    return delay<BOM>(bom);
  },
  setBOMStatus: (id: string, status: string) => {
    const b = boms.find((x) => x.id === id);
    if (b) {
      const next = status as BOM["status"];
      if (next === "active") {
        boms.forEach((other) => {
          if (other.item_id === b.item_id && other.id !== b.id && other.status === "active") {
            other.status = "obsolete";
            other.updated_at = nowIso();
          }
        });
      }
      b.status = next;
      b.updated_at = nowIso();
    }
    return delay<BOM>(b ?? boms[0]);
  },

  // --- Manufacturing: routings ----------------------------------------
  listRoutings: (status?: string) =>
    delay<Routing[]>(
      routings
        .filter((r) => !status || r.status === status)
        .map(({ operations: _omit, ...rest }) => ({ ...rest })),
    ),
  getRouting: (id: string) => {
    const r = routings.find((x) => x.id === id) ?? routings[0];
    return delay<Routing>({ ...r, operations: (r.operations ?? []).map((o) => ({ ...o })) });
  },
  createRouting: (input: CreateRoutingInput) => {
    const now = nowIso();
    const id = nextId();
    const activate = input.activate ?? false;
    if (activate) {
      routings.forEach((r) => {
        if (r.item_id === input.item_id && r.status === "active") {
          r.status = "obsolete";
          r.updated_at = now;
        }
      });
    }
    const routing: Routing = {
      tenant_id: DEMO_TENANT_ID,
      id,
      item_id: input.item_id,
      version: input.version,
      status: activate ? "active" : "draft",
      notes: input.notes,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
      operations: input.operations.map((o, i) => ({
        routing_id: id,
        sequence: i + 1,
        operation_name: o.operation_name,
        work_center_id: o.work_center_id,
        setup_time_minutes: o.setup_time_minutes,
        cycle_time_minutes: o.cycle_time_minutes,
        description: o.description,
      })),
    };
    routings.unshift(routing);
    return delay<Routing>(routing);
  },
  setRoutingStatus: (id: string, status: string) => {
    const r = routings.find((x) => x.id === id);
    if (r) {
      const next = status as Routing["status"];
      if (next === "active") {
        routings.forEach((other) => {
          if (other.item_id === r.item_id && other.id !== r.id && other.status === "active") {
            other.status = "obsolete";
            other.updated_at = nowIso();
          }
        });
      }
      r.status = next;
      r.updated_at = nowIso();
    }
    return delay<Routing>(r ?? routings[0]);
  },

  // --- Manufacturing: capacity ----------------------------------------
  capacityPlan: (params?: { start?: string; end?: string }) => {
    const today = nowIso().slice(0, 10);
    const start = params?.start ?? today;
    const end = params?.end ?? start;
    return delay<CapacityPlan>(buildCapacityPlan(start, end));
  },

  // --- Manufacturing: work orders -------------------------------------
  listWorkOrders: (status?: string) =>
    delay<WorkOrder[]>(
      workOrders.filter((w) => !status || w.status === status).map((w) => ({ ...w })),
    ),
  getWorkOrder: (id: string) =>
    delay<WorkOrder>(workOrders.find((w) => w.id === id) ?? workOrders[0]),
  createWorkOrder: (input: CreateWorkOrderInput) => {
    const now = nowIso();
    const wo: WorkOrder = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      item_id: input.item_id,
      bom_id: null,
      routing_id: null,
      warehouse_id: input.warehouse_id,
      planned_qty: input.planned_qty,
      actual_qty: null,
      status: "draft",
      scheduled_start: input.scheduled_start ?? null,
      scheduled_end: input.scheduled_end ?? null,
      started_at: null,
      completed_at: null,
      notes: input.notes,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
    };
    workOrders.unshift(wo);
    return delay<WorkOrder>(wo);
  },
  releaseWorkOrder: (id: string) => {
    const wo = workOrders.find((w) => w.id === id);
    if (wo) {
      const activeBom = boms.find((b) => b.item_id === wo.item_id && b.status === "active");
      const activeRouting = routings.find(
        (r) => r.item_id === wo.item_id && r.status === "active",
      );
      wo.bom_id = activeBom?.id ?? wo.bom_id ?? null;
      wo.routing_id = activeRouting?.id ?? wo.routing_id ?? null;
      wo.status = "released";
      wo.updated_at = nowIso();
      // Job cards are generated one-per-operation when a routing exists.
      if (activeRouting?.operations && !jobCardsByWO[wo.id]) {
        jobCardsByWO[wo.id] = activeRouting.operations.map((op) => ({
          tenant_id: DEMO_TENANT_ID,
          id: nextId(),
          work_order_id: wo.id,
          routing_operation_seq: op.sequence,
          work_center_id: op.work_center_id,
          status: "pending",
          planned_start: null,
          planned_end: null,
          actual_start: null,
          actual_end: null,
          operator_id: null,
          qty_produced: "0",
          qty_rejected: "0",
          created_at: nowIso(),
          updated_at: nowIso(),
        }));
      }
    }
    return delay<WorkOrder>(wo ?? workOrders[0]);
  },
  startWorkOrder: (id: string) => {
    const wo = workOrders.find((w) => w.id === id);
    if (wo) {
      wo.status = "in_progress";
      wo.started_at = nowIso();
      wo.updated_at = nowIso();
    }
    return delay<WorkOrder>(wo ?? workOrders[0]);
  },
  completeWorkOrder: (id: string, actualQty?: string) => {
    const wo = workOrders.find((w) => w.id === id);
    if (wo) {
      wo.status = "completed";
      wo.actual_qty = actualQty ?? wo.planned_qty;
      wo.completed_at = nowIso();
      wo.updated_at = nowIso();
    }
    return delay<WorkOrder>(wo ?? workOrders[0]);
  },
  cancelWorkOrder: (id: string) => {
    const wo = workOrders.find((w) => w.id === id);
    if (wo) {
      wo.status = "cancelled";
      wo.updated_at = nowIso();
    }
    return delay<WorkOrder>(wo ?? workOrders[0]);
  },
  closeWorkOrder: (id: string) => {
    const wo = workOrders.find((w) => w.id === id);
    if (wo) {
      wo.status = "closed";
      wo.updated_at = nowIso();
    }
    return delay<WorkOrder>(wo ?? workOrders[0]);
  },

  // --- Manufacturing: job cards ---------------------------------------
  listJobCards: (workOrderId: string) =>
    delay<JobCard[]>((jobCardsByWO[workOrderId] ?? []).map((c) => ({ ...c }))),
  getJobCard: (id: string) => {
    for (const list of Object.values(jobCardsByWO)) {
      const found = list.find((c) => c.id === id);
      if (found) return delay<JobCard>({ ...found });
    }
    return delay<JobCard>(null as unknown as JobCard);
  },
  startJobCard: (id: string) => {
    for (const list of Object.values(jobCardsByWO)) {
      const card = list.find((c) => c.id === id);
      if (card) {
        card.status = "in_progress";
        card.actual_start = nowIso();
        card.updated_at = nowIso();
        return delay<JobCard>({ ...card });
      }
    }
    return delay<JobCard>(null as unknown as JobCard);
  },
  completeJobCard: (
    id: string,
    input?: { qty_produced?: string; qty_rejected?: string; notes?: string },
  ) => {
    for (const [woId, list] of Object.entries(jobCardsByWO)) {
      const card = list.find((c) => c.id === id);
      if (card) {
        card.status = "completed";
        card.actual_end = nowIso();
        if (input?.qty_produced) card.qty_produced = input.qty_produced;
        if (input?.qty_rejected) card.qty_rejected = input.qty_rejected;
        card.updated_at = nowIso();
        // Completing the last open card auto-completes the work order.
        if (list.every((c) => c.status === "completed")) {
          const wo = workOrders.find((w) => w.id === woId);
          if (wo && (wo.status === "released" || wo.status === "in_progress")) {
            wo.status = "completed";
            wo.actual_qty = wo.planned_qty;
            wo.completed_at = nowIso();
            wo.updated_at = nowIso();
          }
        }
        return delay<JobCard>({ ...card });
      }
    }
    return delay<JobCard>(null as unknown as JobCard);
  },

  // --- Manufacturing: MRP ---------------------------------------------
  listMRPRuns: () =>
    delay<MRPRun[]>(
      mrpRuns.map(({ demand_lines: _d, planned_orders: _p, ...rest }) => ({ ...rest })),
    ),
  getMRPRun: (id: string) => {
    const run = mrpRuns.find((r) => r.id === id) ?? mrpRuns[0];
    return delay<MRPRun>({
      ...run,
      demand_lines: (run.demand_lines ?? []).map((d) => ({ ...d })),
      planned_orders: (run.planned_orders ?? []).map((p) => ({ ...p })),
    });
  },
  runMRP: (input: RunMRPInput) => {
    const now = nowIso();
    const runId = nextId();
    const buyLead = input.buy_lead_time_days && input.buy_lead_time_days > 0
      ? input.buy_lead_time_days
      : 7;
    const shift = (iso: string, days: number) => {
      const d = new Date(`${iso}T00:00:00Z`);
      d.setUTCDate(d.getUTCDate() - days);
      return d.toISOString().slice(0, 10);
    };
    const demandLines = (input.demand ?? []).map((d) => ({
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      run_id: runId,
      item_id: d.item_id,
      qty: d.qty,
      due_date: d.due_date,
      source: d.source ?? ("manual" as const),
      source_ref: d.source_ref,
      created_at: now,
    }));
    const plannedOrders: MRPRun["planned_orders"] = [];
    demandLines.forEach((d) => {
      const activeBom = boms.find((b) => b.item_id === d.item_id && b.status === "active");
      if (activeBom) {
        plannedOrders.push({
          tenant_id: DEMO_TENANT_ID,
          id: nextId(),
          run_id: runId,
          item_id: d.item_id,
          order_type: "make",
          qty: d.qty,
          due_date: d.due_date,
          suggested_start_date: shift(d.due_date, 5),
          explosion_level: 0,
          bom_id: activeBom.id,
          routing_id: null,
          lead_time_days: 5,
          created_at: now,
        });
        (activeBom.components ?? []).forEach((c) => {
          plannedOrders.push({
            tenant_id: DEMO_TENANT_ID,
            id: nextId(),
            run_id: runId,
            item_id: c.component_item_id,
            order_type: "buy",
            qty: String(Number(c.qty) * Number(d.qty)),
            due_date: shift(d.due_date, 5),
            suggested_start_date: shift(d.due_date, 5 + buyLead),
            explosion_level: 1,
            bom_id: null,
            routing_id: null,
            lead_time_days: buyLead,
            created_at: now,
          });
        });
      } else {
        plannedOrders.push({
          tenant_id: DEMO_TENANT_ID,
          id: nextId(),
          run_id: runId,
          item_id: d.item_id,
          order_type: "buy",
          qty: d.qty,
          due_date: d.due_date,
          suggested_start_date: shift(d.due_date, buyLead),
          explosion_level: 0,
          bom_id: null,
          routing_id: null,
          lead_time_days: buyLead,
          created_at: now,
        });
      }
    });
    const makeCount = plannedOrders.filter((p) => p.order_type === "make").length;
    const run: MRPRun = {
      tenant_id: DEMO_TENANT_ID,
      id: runId,
      status: "completed",
      horizon_start: input.horizon_start,
      horizon_end: input.horizon_end,
      include_min_stock: input.include_min_stock ?? false,
      buy_lead_time_days: buyLead,
      demand_line_count: demandLines.length,
      planned_order_count: plannedOrders.length,
      make_order_count: makeCount,
      buy_order_count: plannedOrders.length - makeCount,
      notes: input.notes,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
      demand_lines: demandLines,
      planned_orders: plannedOrders,
    };
    mrpRuns.unshift(run);
    return delay<MRPRun>(run);
  },

  // --- Manufacturing: subcontracting ----------------------------------
  listSubcontractOrders: (status?: string) =>
    delay<SubcontractOrder[]>(
      subcontractOrders
        .filter((o) => !status || o.status === status)
        .map(({ components: _omit, ...rest }) => ({ ...rest })),
    ),
  getSubcontractOrder: (id: string) => {
    const o = subcontractOrders.find((x) => x.id === id) ?? subcontractOrders[0];
    return delay<SubcontractOrder>({
      ...o,
      components: (o.components ?? []).map((c) => ({ ...c })),
    });
  },
  createSubcontractOrder: (input: CreateSubcontractOrderInput) => {
    const now = nowIso();
    const id = nextId();
    const order: SubcontractOrder = {
      tenant_id: DEMO_TENANT_ID,
      id,
      work_order_id: input.work_order_id ?? null,
      routing_operation_seq: input.routing_operation_seq ?? null,
      supplier_id: input.supplier_id ?? null,
      item_id: input.item_id,
      warehouse_id: input.warehouse_id,
      qty: input.qty,
      received_qty: "0",
      status: "draft",
      charge_amount: input.charge_amount ?? "0.00",
      charge_currency: input.charge_currency ?? "USD",
      issued_at: null,
      received_at: null,
      notes: input.notes,
      created_by: "demo-user",
      created_at: now,
      updated_at: now,
      components: input.components.map((c) => ({
        tenant_id: DEMO_TENANT_ID,
        id: nextId(),
        subcontract_order_id: id,
        item_id: c.item_id,
        qty: c.qty,
        issued_qty: "0",
        created_at: now,
      })),
    };
    subcontractOrders.unshift(order);
    return delay<SubcontractOrder>(order);
  },
  issueSubcontractOrder: (id: string) => {
    const o = subcontractOrders.find((x) => x.id === id);
    if (o) {
      o.status = "issued";
      o.issued_at = nowIso();
      o.updated_at = nowIso();
      (o.components ?? []).forEach((c) => {
        c.issued_qty = c.qty;
      });
    }
    return delay<SubcontractOrder>(o ?? subcontractOrders[0]);
  },
  receiveSubcontractOrder: (id: string, input?: { actual_qty?: string }) => {
    const o = subcontractOrders.find((x) => x.id === id);
    if (o) {
      o.status = "received";
      o.received_qty = input?.actual_qty ?? o.qty;
      o.received_at = nowIso();
      o.updated_at = nowIso();
    }
    return delay<SubcontractOrder>(o ?? subcontractOrders[0]);
  },
  closeSubcontractOrder: (id: string) => {
    const o = subcontractOrders.find((x) => x.id === id);
    if (o) {
      o.status = "closed";
      o.updated_at = nowIso();
    }
    return delay<SubcontractOrder>(o ?? subcontractOrders[0]);
  },
  cancelSubcontractOrder: (id: string) => {
    const o = subcontractOrders.find((x) => x.id === id);
    if (o) {
      o.status = "cancelled";
      o.updated_at = nowIso();
    }
    return delay<SubcontractOrder>(o ?? subcontractOrders[0]);
  },

  // --- Inventory: landed-cost vouchers --------------------------------
  listLandedCostVouchers: (params?: { status?: string }) => {
    const rows = landedCostVouchers.filter(
      (v) => !params?.status || v.status === params.status,
    );
    return delay<LandedCostVoucher[]>(rows.map((v) => ({ ...v })));
  },
  getLandedCostVoucher: (id: string) => {
    const voucher =
      landedCostVouchers.find((v) => v.id === id) ?? landedCostVouchers[0];
    return delay<LandedCostVoucherWithLines>({
      voucher: { ...voucher },
      charges: (landedCostCharges[voucher.id] ?? []).map((c) => ({ ...c })),
      targets: (landedCostTargets[voucher.id] ?? []).map((t) => ({ ...t })),
    });
  },
  createLandedCostVoucher: (input: UpsertLandedCostVoucherInput) => {
    const v: LandedCostVoucher = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      voucher_number: input.voucher_number,
      description: input.description,
      status: "draft",
      allocation_method: input.allocation_method ?? "by_qty",
      posted_at: null,
      je_id: null,
      created_by: "demo",
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    landedCostVouchers.unshift(v);
    landedCostCharges[v.id] = [];
    landedCostTargets[v.id] = [];
    return delay<LandedCostVoucher>({ ...v });
  },
  updateLandedCostVoucher: (id: string, input: UpsertLandedCostVoucherInput) => {
    const v = landedCostVouchers.find((x) => x.id === id);
    if (v) {
      v.voucher_number = input.voucher_number;
      v.description = input.description;
      if (input.allocation_method) v.allocation_method = input.allocation_method;
      v.updated_at = nowIso();
    }
    return delay<LandedCostVoucher>({ ...(v ?? landedCostVouchers[0]) });
  },
  deleteLandedCostVoucher: (id: string) => {
    const i = landedCostVouchers.findIndex((x) => x.id === id);
    if (i >= 0) landedCostVouchers.splice(i, 1);
    delete landedCostCharges[id];
    delete landedCostTargets[id];
    return delay<void>(undefined);
  },
  upsertLandedCostCharge: (
    voucherId: string,
    input: UpsertLandedCostChargeInput,
  ) => {
    const list = (landedCostCharges[voucherId] ??= []);
    const existing = input.id ? list.find((c) => c.id === input.id) : undefined;
    if (existing) {
      existing.description = input.description;
      existing.amount = String(input.amount);
      existing.account_code = input.account_code;
      existing.updated_at = nowIso();
      return delay<LandedCostCharge>({ ...existing });
    }
    const charge: LandedCostCharge = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      voucher_id: voucherId,
      description: input.description,
      amount: String(input.amount),
      account_code: input.account_code,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    list.push(charge);
    return delay<LandedCostCharge>({ ...charge });
  },
  deleteLandedCostCharge: (voucherId: string, chargeId: string) => {
    const list = landedCostCharges[voucherId] ?? [];
    const i = list.findIndex((c) => c.id === chargeId);
    if (i >= 0) list.splice(i, 1);
    return delay<void>(undefined);
  },
  upsertLandedCostTarget: (
    voucherId: string,
    input: UpsertLandedCostTargetInput,
  ) => {
    const list = (landedCostTargets[voucherId] ??= []);
    const qty = String(input.qty);
    const unitCost = String(input.unit_cost);
    const amount = (Number(qty) * Number(unitCost)).toFixed(2);
    const weight = input.weight !== undefined ? String(input.weight) : "0";
    const existing = input.id ? list.find((t) => t.id === input.id) : undefined;
    if (existing) {
      existing.source_id = input.source_id;
      existing.item_id = input.item_id;
      existing.warehouse_id = input.warehouse_id;
      existing.qty = qty;
      existing.unit_cost = unitCost;
      existing.amount = amount;
      existing.weight = weight;
      existing.updated_at = nowIso();
      return delay<LandedCostTarget>({ ...existing });
    }
    const target: LandedCostTarget = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      voucher_id: voucherId,
      source_ktype: input.source_ktype ?? "inventory.goods_receipt",
      source_id: input.source_id,
      item_id: input.item_id,
      warehouse_id: input.warehouse_id,
      qty,
      unit_cost: unitCost,
      amount,
      weight,
      allocated_amount: "0.00",
      applied: false,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    list.push(target);
    return delay<LandedCostTarget>({ ...target });
  },
  deleteLandedCostTarget: (voucherId: string, targetId: string) => {
    const list = landedCostTargets[voucherId] ?? [];
    const i = list.findIndex((t) => t.id === targetId);
    if (i >= 0) list.splice(i, 1);
    return delay<void>(undefined);
  },
  allocateLandedCostVoucher: (id: string) => {
    const voucher = landedCostVouchers.find((v) => v.id === id);
    const targets = landedCostTargets[id] ?? [];
    if (voucher) {
      const totalCharges = (landedCostCharges[id] ?? []).reduce(
        (s, c) => s + Number(c.amount),
        0,
      );
      const basisOf = (t: LandedCostTarget) =>
        voucher.allocation_method === "by_amount"
          ? Number(t.amount)
          : voucher.allocation_method === "by_weight"
            ? Number(t.weight)
            : Number(t.qty);
      const totalBasis = targets.reduce((s, t) => s + basisOf(t), 0) || 1;
      targets.forEach((t) => {
        t.allocated_amount = (
          (basisOf(t) / totalBasis) *
          totalCharges
        ).toFixed(2);
        t.updated_at = nowIso();
      });
      voucher.status = "allocated";
      voucher.updated_at = nowIso();
    }
    return delay<LandedCostTarget[]>(targets.map((t) => ({ ...t })));
  },
  postLandedCostVoucher: (id: string) => {
    const voucher =
      landedCostVouchers.find((v) => v.id === id) ?? landedCostVouchers[0];
    const postedAt = nowIso();
    voucher.status = "posted";
    voucher.posted_at = postedAt;
    voucher.je_id = nextId();
    voucher.updated_at = postedAt;
    (landedCostTargets[voucher.id] ?? []).forEach((t) => {
      t.applied = true;
      t.updated_at = postedAt;
    });
    return delay<LandedCostPostResult>({
      voucher: { ...voucher },
      journal_entry: { id: voucher.je_id, posted_at: postedAt },
    });
  },

  // --- Inventory: cycle-count sessions --------------------------------
  listCycleCountSessions: (filter?: {
    status?: string;
    warehouse_id?: string;
  }) => {
    const rows = cycleCountSessions.filter(
      (s) =>
        (!filter?.status || s.status === filter.status) &&
        (!filter?.warehouse_id || s.warehouse_id === filter.warehouse_id),
    );
    return delay<CycleCountSession[]>(rows.map((s) => ({ ...s })));
  },
  createCycleCountSession: (input: {
    code: string;
    description?: string;
    warehouse_id: string;
  }) => {
    const s: CycleCountSession = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      code: input.code,
      description: input.description,
      warehouse_id: input.warehouse_id,
      status: "draft",
      created_by: "demo",
      created_at: nowIso(),
      updated_at: nowIso(),
      posted_at: null,
    };
    cycleCountSessions.unshift(s);
    cycleCountLines[s.id] = [];
    return delay<CycleCountSession>({ ...s });
  },
  getCycleCountSession: (id: string) => {
    const session =
      cycleCountSessions.find((s) => s.id === id) ?? cycleCountSessions[0];
    return delay<CycleCountSessionWithLines>({
      session: { ...session },
      lines: (cycleCountLines[session.id] ?? []).map((l) => ({ ...l })),
    });
  },
  updateCycleCountSession: (
    id: string,
    input: {
      code: string;
      description?: string;
      warehouse_id: string;
      status?: string;
    },
  ) => {
    const s = cycleCountSessions.find((x) => x.id === id);
    if (s) {
      s.code = input.code;
      s.description = input.description;
      s.warehouse_id = input.warehouse_id;
      if (input.status) s.status = input.status as CycleCountSession["status"];
      s.updated_at = nowIso();
    }
    return delay<CycleCountSession>({ ...(s ?? cycleCountSessions[0]) });
  },
  deleteCycleCountSession: (id: string) => {
    const i = cycleCountSessions.findIndex((x) => x.id === id);
    if (i >= 0) cycleCountSessions.splice(i, 1);
    delete cycleCountLines[id];
    return delay<void>(undefined);
  },
  seedCycleCountSession: (id: string) => {
    const session = cycleCountSessions.find((s) => s.id === id);
    const wh = session?.warehouse_id;
    const lines: CycleCountLine[] = STOCK_LEVELS.filter(
      (lvl) => !wh || lvl.warehouse_id === wh,
    )
      .slice(0, 6)
      .map((lvl) => ({
        tenant_id: DEMO_TENANT_ID,
        id: nextId(),
        session_id: id,
        item_id: lvl.item_id,
        expected_qty: lvl.qty,
        counted_qty: "0",
        variance: (0 - Number(lvl.qty)).toString(),
        notes: undefined,
        created_at: nowIso(),
        updated_at: nowIso(),
      }));
    cycleCountLines[id] = lines;
    if (session && session.status === "draft") {
      session.status = "counting";
      session.updated_at = nowIso();
    }
    return delay<CycleCountLine[]>(lines.map((l) => ({ ...l })));
  },
  upsertCycleCountLine: (
    sessionId: string,
    input: {
      id?: string;
      item_id: string;
      expected_qty: string;
      counted_qty: string;
      notes?: string;
    },
  ) => {
    const list = (cycleCountLines[sessionId] ??= []);
    const variance = (
      Number(input.counted_qty) - Number(input.expected_qty)
    ).toString();
    const existing = input.id ? list.find((l) => l.id === input.id) : undefined;
    if (existing) {
      existing.item_id = input.item_id;
      existing.expected_qty = input.expected_qty;
      existing.counted_qty = input.counted_qty;
      existing.variance = variance;
      existing.notes = input.notes;
      existing.updated_at = nowIso();
      return delay<CycleCountLine>({ ...existing });
    }
    const line: CycleCountLine = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      session_id: sessionId,
      item_id: input.item_id,
      expected_qty: input.expected_qty,
      counted_qty: input.counted_qty,
      variance,
      notes: input.notes,
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    list.push(line);
    return delay<CycleCountLine>({ ...line });
  },
  deleteCycleCountLine: (sessionId: string, lineId: string) => {
    const list = cycleCountLines[sessionId] ?? [];
    const i = list.findIndex((l) => l.id === lineId);
    if (i >= 0) list.splice(i, 1);
    return delay<void>(undefined);
  },
  postCycleCountSession: (id: string) => {
    const s = cycleCountSessions.find((x) => x.id === id);
    if (s) {
      s.status = "posted";
      s.posted_at = nowIso();
      s.updated_at = nowIso();
    }
    return delay<CycleCountSession>({ ...(s ?? cycleCountSessions[0]) });
  },

  // --- Recruitment: job openings -------------------------------------
  listJobOpenings: (filter?: { status?: string; department?: string }) => {
    const rows = jobOpenings.filter(
      (o) =>
        (!filter?.status || o.status === filter.status) &&
        (!filter?.department || o.department === filter.department),
    );
    return delay<JobOpening[]>(rows.map((o) => ({ ...o })));
  },
  getJobOpening: (id: string) => {
    const o = jobOpenings.find((x) => x.id === id) ?? jobOpenings[0];
    return delay<JobOpening>({ ...o });
  },
  createJobOpening: (input: JobOpeningInput) => {
    const o: JobOpening = {
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      title: input.title,
      department: input.department,
      description: input.description,
      requirements: input.requirements,
      employment_type: input.employment_type ?? "full_time",
      location: input.location,
      salary_range_min: input.salary_range_min ?? null,
      salary_range_max: input.salary_range_max ?? null,
      currency: input.currency ?? DEMO_BASE_CURRENCY,
      status: "draft",
      hiring_manager_id: input.hiring_manager_id ?? null,
      max_positions: input.max_positions ?? 1,
      positions_filled: 0,
      published_at: null,
      closes_at: input.closes_at ?? null,
      created_by: "demo",
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    jobOpenings.unshift(o);
    return delay<JobOpening>({ ...o });
  },
  updateJobOpening: (id: string, input: JobOpeningInput) => {
    const o = jobOpenings.find((x) => x.id === id);
    if (o) {
      o.title = input.title;
      o.department = input.department;
      o.description = input.description;
      o.requirements = input.requirements;
      if (input.employment_type) o.employment_type = input.employment_type;
      o.location = input.location;
      o.salary_range_min = input.salary_range_min ?? null;
      o.salary_range_max = input.salary_range_max ?? null;
      if (input.currency) o.currency = input.currency;
      o.hiring_manager_id = input.hiring_manager_id ?? null;
      if (input.max_positions !== undefined) o.max_positions = input.max_positions;
      o.closes_at = input.closes_at ?? null;
      o.updated_at = nowIso();
    }
    return delay<JobOpening>({ ...(o ?? jobOpenings[0]) });
  },
  publishJobOpening: (id: string) => {
    const o = jobOpenings.find((x) => x.id === id);
    if (o) {
      o.status = "open";
      o.published_at = nowIso();
      o.updated_at = nowIso();
    }
    return delay<JobOpening>({ ...(o ?? jobOpenings[0]) });
  },
  closeJobOpening: (id: string) => {
    const o = jobOpenings.find((x) => x.id === id);
    if (o) {
      o.status = "closed";
      o.updated_at = nowIso();
    }
    return delay<JobOpening>({ ...(o ?? jobOpenings[0]) });
  },

  // --- Recruitment: applications -------------------------------------
  listApplications: (filter?: {
    job_opening_id?: string;
    status?: string;
  }) => {
    const rows = applications.filter(
      (a) =>
        (!filter?.job_opening_id ||
          a.job_opening_id === filter.job_opening_id) &&
        (!filter?.status || a.status === filter.status),
    );
    return delay<JobApplication[]>(rows.map((a) => ({ ...a })));
  },
  getApplication: (id: string) => {
    const a = applications.find((x) => x.id === id) ?? applications[0];
    return delay<JobApplication>({ ...a });
  },
  createApplication: (input: CreateApplicationInput) => {
    const a: JobApplication = {
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      job_opening_id: input.job_opening_id,
      applicant_name: input.applicant_name,
      applicant_email: input.applicant_email,
      phone: input.phone,
      resume_file_id: input.resume_file_id ?? null,
      cover_letter: input.cover_letter,
      source: input.source ?? "website",
      referrer_employee_id: input.referrer_employee_id ?? null,
      status: "applied",
      rating: input.rating ?? null,
      notes: input.notes,
      hired_employee_id: null,
      applied_at: nowIso(),
      created_by: "demo",
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    applications.unshift(a);
    return delay<JobApplication>({ ...a });
  },
  updateApplication: (id: string, input: UpdateApplicationInput) => {
    const a = applications.find((x) => x.id === id);
    if (a) {
      a.applicant_name = input.applicant_name;
      a.applicant_email = input.applicant_email;
      a.phone = input.phone;
      a.resume_file_id = input.resume_file_id ?? null;
      a.cover_letter = input.cover_letter;
      if (input.source) a.source = input.source;
      a.referrer_employee_id = input.referrer_employee_id ?? null;
      a.rating = input.rating ?? null;
      a.notes = input.notes;
      a.updated_at = nowIso();
    }
    return delay<JobApplication>({ ...(a ?? applications[0]) });
  },
  advanceApplication: (id: string, status: string) => {
    const a = applications.find((x) => x.id === id);
    if (a) {
      a.status = status as JobApplication["status"];
      a.updated_at = nowIso();
    }
    return delay<JobApplication>({ ...(a ?? applications[0]) });
  },
  rejectApplication: (id: string, reason?: string) => {
    const a = applications.find((x) => x.id === id);
    if (a) {
      a.status = "rejected";
      if (reason) a.notes = reason;
      a.updated_at = nowIso();
    }
    return delay<JobApplication>({ ...(a ?? applications[0]) });
  },

  // --- Recruitment: interviews ---------------------------------------
  listInterviews: (filter?: { application_id?: string; status?: string }) => {
    const rows = interviews.filter(
      (i) =>
        (!filter?.application_id ||
          i.application_id === filter.application_id) &&
        (!filter?.status || i.status === filter.status),
    );
    return delay<Interview[]>(rows.map((i) => ({ ...i })));
  },
  createInterview: (input: CreateInterviewInput) => {
    const i: Interview = {
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      application_id: input.application_id,
      interviewer_id: input.interviewer_id ?? null,
      interview_type: input.interview_type ?? "video",
      scheduled_at: input.scheduled_at ?? null,
      duration_minutes: input.duration_minutes ?? 45,
      location: input.location,
      meeting_link: input.meeting_link,
      status: "scheduled",
      rating: null,
      feedback: undefined,
      recommendation: undefined,
      created_by: "demo",
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    interviews.unshift(i);
    return delay<Interview>({ ...i });
  },
  completeInterview: (id: string, input: CompleteInterviewInput) => {
    const i = interviews.find((x) => x.id === id);
    if (i) {
      i.status = "completed";
      i.rating = input.rating ?? null;
      i.feedback = input.feedback;
      i.recommendation = input.recommendation;
      i.updated_at = nowIso();
    }
    return delay<Interview>({ ...(i ?? interviews[0]) });
  },

  // --- LMS: learning paths + badges ----------------------------------
  listLearningPaths: (status?: string) => {
    const rows = learningPaths.filter((p) => !status || p.status === status);
    return delay<{ learning_paths: LearningPath[] }>({
      learning_paths: rows.map((p) => ({ ...p })),
    });
  },
  getLearningPath: (id: string) => {
    const path = learningPaths.find((p) => p.id === id) ?? learningPaths[0];
    return delay<{
      learning_path: LearningPath;
      courses: LearningPathCourse[];
    }>({
      learning_path: { ...path },
      courses: (LEARNING_PATH_COURSES_BY_PATH[path.id] ?? []).map((c) => ({
        ...c,
      })),
    });
  },
  createLearningPath: (input: {
    title: string;
    description?: string;
    status?: string;
    target_roles?: string[];
    estimated_duration_hours?: number;
    difficulty?: string;
  }) => {
    const p: LearningPath = {
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      title: input.title,
      description: input.description ?? "",
      status: (input.status as LearningPath["status"]) ?? "draft",
      target_roles: input.target_roles ?? null,
      estimated_duration_hours: input.estimated_duration_hours ?? 0,
      difficulty: input.difficulty ?? "beginner",
      created_by: "demo",
      created_at: nowIso(),
      updated_at: nowIso(),
    };
    learningPaths.unshift(p);
    return delay<LearningPath>({ ...p });
  },
  enrollInLearningPath: (id: string, userId?: string) => {
    return delay<KRecord>({
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      ktype: "lms.enrollment",
      ktype_version: 1,
      data: {
        learning_path_id: id,
        user_id: userId ?? "demo-user",
        status: "enrolled",
        progress: 0,
      },
      status: "enrolled",
      version: 1,
      created_at: nowIso(),
      updated_at: nowIso(),
    });
  },
  listBadges: () =>
    delay<{ badges: Badge[] }>({ badges: BADGES.map((b) => ({ ...b })) }),
  listBadgeAwards: () =>
    delay<{ awards: BadgeAward[] }>({
      awards: BADGE_AWARDS.map((a) => ({ ...a })),
    }),

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
  listInsightsQueryShares: () => delay<{ shares: InsightsShare[] }>({ shares: [] }),
  shareInsightsQuery: (id: string, input: InsightsShareInput) =>
    delay<InsightsShare>({
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      resource_type: "query",
      resource_id: id,
      grantee_type: input?.grantee_type ?? "user",
      grantee: input?.grantee ?? "demo@acme.example",
      permission: input?.permission ?? "view",
      created_at: nowIso(),
    }),
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
  upsertInsightsWidget: (dashboardId: string, input: InsightsWidgetInput) =>
    delay<InsightsWidget>({
      tenant_id: DEMO_TENANT_ID,
      id: input?.id ?? nextId(),
      dashboard_id: dashboardId,
      query_id: input?.query_id ?? null,
      viz_type: input?.viz_type ?? "bar",
      position: input?.position ?? { x: 0, y: 0, w: 6, h: 4 },
      config: input?.config ?? {},
      created_at: nowIso(),
      updated_at: nowIso(),
    }),
  deleteInsightsWidget: () => delay<void>(undefined as unknown as void),
  listInsightsDashboardShares: () =>
    delay<{ shares: InsightsShare[] }>({ shares: [] }),
  shareInsightsDashboard: (id: string, input: InsightsShareInput) =>
    delay<InsightsShare>({
      tenant_id: DEMO_TENANT_ID,
      id: nextId(),
      resource_type: "dashboard",
      resource_id: id,
      grantee_type: input?.grantee_type ?? "user",
      grantee: input?.grantee ?? "demo@acme.example",
      permission: input?.permission ?? "view",
      created_at: nowIso(),
    }),
  deleteInsightsDashboardShare: () => delay<void>(undefined as unknown as void),
  listInsightsDataSources: () =>
    delay<{ data_sources: InsightsDataSource[] }>({ data_sources: [] }),
  createInsightsDataSource: (input: InsightsDataSourceInput) =>
    delay<InsightsDataSource>(makeDemoDataSource(input)),
  updateInsightsDataSource: (_id: string, input: InsightsDataSourceInput) =>
    delay<InsightsDataSource>(makeDemoDataSource(input)),
  deleteInsightsDataSource: () => delay<void>(undefined as unknown as void),
  testInsightsDataSource: () => delay<{ ok: boolean }>({ ok: true }),

  // --- Marketplace ----------------------------------------------------
  listMarketplaceExtensions: (opts: MarketplaceListExtensionsOptions = {}) => {
    const q = opts.q?.toLowerCase().trim();
    const publisher = opts.publisher?.toLowerCase().trim();
    let items = mktExtensions.filter((e) => e.status === "listed");
    if (publisher) {
      items = items.filter((e) => e.publisher.toLowerCase().includes(publisher));
    }
    if (opts.category) {
      items = items.filter((e) => e.category === opts.category);
    }
    if (q) {
      items = items.filter(
        (e) =>
          e.display_name.toLowerCase().includes(q) ||
          e.description.toLowerCase().includes(q) ||
          e.name.toLowerCase().includes(q),
      );
    }
    return delay<MarketplaceListExtensionsResponse>({
      items: items.map((e) => ({ ...e })),
    });
  },
  getMarketplaceExtension: (extId: string) => {
    const ext = findExtension(extId);
    return delay<MarketplaceGetExtensionResponse>({
      extension: { ...ext },
      versions: (MARKETPLACE_VERSIONS[ext.id] ?? []).map((v) => ({ ...v })),
      my_rating: mktMyRatings[ext.id] ?? null,
    });
  },
  listMarketplaceVersions: (extId: string) =>
    delay<MarketplaceListVersionsResponse>({
      items: (MARKETPLACE_VERSIONS[extId] ?? []).map((v) => ({ ...v })),
    }),
  rateMarketplaceExtension: (extId: string, stars: number) => {
    const ext = findExtension(extId);
    const prev = mktMyRatings[extId] ?? 0;
    const sum = ext.rating_average * ext.rating_count;
    if (prev > 0) {
      // Revising an existing rating — count is unchanged, swap the
      // contribution of the old star value for the new one.
      ext.rating_average =
        ext.rating_count > 0 ? (sum - prev + stars) / ext.rating_count : stars;
    } else {
      const count = ext.rating_count + 1;
      ext.rating_average = (sum + stars) / count;
      ext.rating_count = count;
    }
    mktMyRatings[extId] = stars;
    return delay<MarketplaceRatingSummary>({
      rating_average: ext.rating_average,
      rating_count: ext.rating_count,
      my_rating: stars,
    });
  },
  listMarketplaceInstallations: () =>
    delay<MarketplaceListInstallationsResponse>({
      items: mktInstallations.map((i) => ({ ...i })),
    }),
  getMarketplaceInstallation: (installId: string) =>
    delay<MarketplaceInstallation>({ ...findInstallation(installId) }),
  installMarketplaceExtension: (input: InstallMarketplaceExtensionInput) => {
    const inst: MarketplaceInstallation = {
      id: nextId(),
      tenant_id: DEMO_TENANT_ID,
      extension_id: input.extension_id,
      extension_version_id: input.version_id,
      status: "active",
      settings: input.settings ?? {},
      webhook_base: input.webhook_base,
      installed_at: nowIso(),
      updated_at: nowIso(),
      last_health_check_at: nowIso(),
      last_health_check_status: "ok",
    };
    mktInstallations = [...mktInstallations, inst];
    return delay<InstallMarketplaceExtensionResponse>({
      installation: { ...inst },
      signing_secret: `whsec_demo_${nextId().replace(/-/g, "")}`,
    });
  },
  updateMarketplaceInstallationSettings: (
    installId: string,
    settings: Record<string, unknown>,
  ) => {
    const inst = findInstallation(installId);
    inst.settings = settings;
    inst.updated_at = nowIso();
    return delay<MarketplaceUpdateSettingsResponse>({
      installation: { ...inst },
    });
  },
  upgradeMarketplaceInstallation: (
    installId: string,
    input: UpgradeMarketplaceInstallationInput,
  ) => {
    const inst = findInstallation(installId);
    inst.extension_version_id = input.to_version_id;
    inst.updated_at = nowIso();
    if (input.settings != null) inst.settings = input.settings;
    return delay<UpgradeMarketplaceInstallationResponse>({
      installation: { ...inst },
      from_version_id: input.from_version_id,
    });
  },
  uninstallMarketplaceExtension: (installId: string) => {
    mktInstallations = mktInstallations.filter((i) => i.id !== installId);
    return delay<void>(undefined as unknown as void);
  },

  // --- Misc fallbacks -------------------------------------------------
  getPublicForm: () => delay({ id: "demo-form", title: "Demo form", fields: [] }),
  submitPublicForm: () => delay({ ok: true }),
  generatePayslips: () =>
    delay<PayslipGenerateResult>({
      payslip_ids: (records["hr.payslip"] ?? []).map((p) => p.id),
      created_count: (records["hr.payslip"] ?? []).length,
      skipped_existing: 0,
      skipped_no_structure: 0,
    }),
  postPayRun: () =>
    delay<JournalEntry>({
      ...JOURNAL_ENTRIES[0],
    }),
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
//
// The "then"/"catch"/"finally" properties are explicitly omitted so the
// proxy doesn't masquerade as a thenable. If we returned a function for
// `.then`, anything that resolves a Promise with `mockApi` (e.g. a
// dynamic-import chain that returns the mock client) would hang
// forever waiting for our stub `then(resolve, reject)` to fulfil.
const PROMISE_PROTOCOL = new Set(["then", "catch", "finally"]);

export const mockApi = new Proxy({} as ApiClient, {
  get(_target, prop: string | symbol) {
    if (typeof prop !== "string") return undefined;
    if (PROMISE_PROTOCOL.has(prop)) return undefined;
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
