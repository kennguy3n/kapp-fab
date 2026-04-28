// Hand-written client surface used by apps/web until the generated
// client from the OpenAPI spec lands. Run `npm run generate-client`
// (or `npm run generate -w @kapp/client`) once the API is up to
// replace the types in this file with generated ones.

export interface FieldSpec {
  name: string;
  type: string;
  required?: boolean;
  max_length?: number;
  min?: number;
  max?: number;
  pattern?: string;
  values?: string[];
  ref?: string;
  ktype?: string;
  default?: unknown;
}

export interface KanbanView {
  group_by: string;
  card_title?: string;
  card_subtitle?: string;
}

export interface WorkflowTransition {
  from: string[];
  to: string;
  action: string;
  post?: string[];
}

export interface WorkflowDefinition {
  name: string;
  initial_state: string;
  states: string[];
  transitions: WorkflowTransition[];
}

export interface KTypeSchema {
  name: string;
  version: number;
  fields: FieldSpec[];
  views?: {
    list?: { columns?: string[]; default_sort?: string };
    form?: { sections?: Array<{ title?: string; fields: string[] }> };
    kanban?: KanbanView;
  };
  workflow?: WorkflowDefinition;
  cards?: { summary?: string };
}

export interface KType {
  name: string;
  version: number;
  schema: KTypeSchema;
}

export interface KRecord {
  id: string;
  tenant_id: string;
  ktype: string;
  ktype_version: number;
  data: Record<string, unknown>;
  status: string;
  version: number;
  created_at: string;
  updated_at: string;
}

export interface Tenant {
  id: string;
  slug: string;
  name: string;
  cell: string;
  status: "active" | "suspended" | "archived" | "deleting";
  plan: string;
  quota: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
}

export interface CreateTenantInput {
  slug: string;
  name: string;
  cell: string;
  plan: string;
  quota?: Record<string, unknown>;
}

export interface TenantFeaturesResponse {
  tenant_id: string;
  features: Record<string, boolean>;
}

export interface PlacementPolicy {
  tenant: string;
  bucket?: string;
  policy: {
    encryption: { mode: string; kms?: string };
    placement: {
      provider: string[];
      region?: string[];
      country?: string[];
      storage_class?: string[];
      cache_location?: string;
    };
  };
}

export interface RetentionPolicy {
  tenant_id: string;
  category: string;
  retention_days: number;
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface IsolationCheck {
  name: string;
  passed: boolean;
  detail?: string;
  elapsed?: string;
}

export interface IsolationReport {
  passed: boolean;
  ran_at: string;
  duration: string;
  checks: IsolationCheck[];
}

// --- Consolidation (Phase M Task 7) -----------------------------------

export interface EliminationPair {
  from_tenant: string;
  to_tenant: string;
  account_code: string;
}

export interface ConsolidationGroup {
  id: string;
  name: string;
  presentation_currency: string;
  member_tenant_ids: string[];
  elimination_pairs?: EliminationPair[];
  created_at?: string;
  updated_at?: string;
}

export interface TenantBalanceRow {
  tenant_id: string;
  debit: string;
  credit: string;
}

export interface ConsolidatedRow {
  account_code: string;
  debit: string;
  credit: string;
  balance: string;
  contributions?: TenantBalanceRow[];
}

export interface ConsolidatedTrialBalance {
  group_id: string;
  as_of: string;
  presentation_currency: string;
  rows: ConsolidatedRow[];
  eliminated: ConsolidatedRow[];
  total_debit: string;
  total_credit: string;
}

export interface PlanLimits {
  api_calls?: number;
  storage_bytes?: number;
  krecord_count?: number;
  user_seats?: number;
  [key: string]: number | undefined;
}

export interface Plan {
  name: string;
  display_name: string;
  limits: PlanLimits;
  features: Record<string, boolean>;
}

export interface UsageRow {
  tenant_id: string;
  period_start: string;
  metric: string;
  value: number;
  updated_at: string;
}

export interface TenantUsageResponse {
  tenant_id: string;
  plan: string;
  period_start: string;
  usage: Record<string, number>;
  limits: PlanLimits;
  rows: UsageRow[];
  features: Record<string, boolean>;
}

export interface TenantUsageHistoryRow {
  period_start: string;
  metric: string;
  value: number;
}

export interface TenantUsageHistoryResponse {
  tenant_id: string;
  rows: TenantUsageHistoryRow[];
  months: number;
}

interface ClientConfig {
  baseUrl: string;
  headers: () => Record<string, string>;
}

export class ApiClient {
  constructor(private readonly cfg: ClientConfig) {}

  private async request<T>(
    path: string,
    init: RequestInit = {}
  ): Promise<T> {
    const res = await fetch(`${this.cfg.baseUrl}${path}`, {
      ...init,
      headers: {
        "Content-Type": "application/json",
        ...this.cfg.headers(),
        ...(init.headers ?? {}),
      },
    });
    if (!res.ok) {
      throw new Error(`${res.status} ${res.statusText}`);
    }
    if (res.status === 204) return undefined as T;
    return (await res.json()) as T;
  }

  // --- Tenant control plane ---------------------------------------------
  listTenants(): Promise<Tenant[]> {
    return this.request("/tenants");
  }

  getTenant(id: string): Promise<Tenant> {
    return this.request(`/tenants/${encodeURIComponent(id)}`);
  }

  createTenant(input: CreateTenantInput): Promise<Tenant> {
    return this.request("/tenants", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  // --- Feature flags ----------------------------------------------------
  listTenantFeatures(id: string): Promise<TenantFeaturesResponse> {
    return this.request(`/tenants/${encodeURIComponent(id)}/features`);
  }

  updateTenantFeatures(
    id: string,
    features: Record<string, boolean>
  ): Promise<TenantFeaturesResponse> {
    return this.request(`/tenants/${encodeURIComponent(id)}/features`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({ features }),
    });
  }

  // --- Placement policy -------------------------------------------------
  getPlacementPolicy(id: string): Promise<PlacementPolicy> {
    return this.request(`/tenants/${encodeURIComponent(id)}/placement`);
  }

  updatePlacementPolicy(
    id: string,
    policy: PlacementPolicy
  ): Promise<PlacementPolicy> {
    return this.request(`/tenants/${encodeURIComponent(id)}/placement`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(policy),
    });
  }

  // --- Data retention policies ----------------------------------------
  listRetentionPolicies(
    id: string
  ): Promise<{ policies: RetentionPolicy[] }> {
    return this.request(`/tenants/${encodeURIComponent(id)}/retention`);
  }

  upsertRetentionPolicy(
    id: string,
    body: { category: string; retention_days: number; enabled?: boolean }
  ): Promise<RetentionPolicy> {
    return this.request(`/tenants/${encodeURIComponent(id)}/retention`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(body),
    });
  }

  // --- Runtime isolation audit ----------------------------------------
  runIsolationAudit(): Promise<IsolationReport> {
    return this.request(`/admin/isolation-audit`);
  }

  // --- Consolidation (admin-only, operator-scoped) --------------------
  createConsolidationGroup(input: {
    name: string;
    presentation_currency: string;
    member_tenant_ids: string[];
    elimination_pairs?: { from_tenant: string; to_tenant: string; account_code: string }[];
  }): Promise<ConsolidationGroup> {
    return this.request(`/admin/consolidation/groups`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  runConsolidation(
    groupID: string,
    asOf?: Date
  ): Promise<ConsolidatedTrialBalance> {
    return this.request(
      `/admin/consolidation/groups/${encodeURIComponent(groupID)}/run`,
      {
        method: "POST",
        body: JSON.stringify(asOf ? { as_of: asOf.toISOString() } : {}),
      }
    );
  }

  // --- Metering + plans -------------------------------------------------
  getTenantUsage(id: string): Promise<TenantUsageResponse> {
    return this.request(`/tenants/${encodeURIComponent(id)}/usage`);
  }

  getTenantUsageHistory(
    id: string,
    months = 6
  ): Promise<TenantUsageHistoryResponse> {
    return this.request(
      `/tenants/${encodeURIComponent(id)}/usage/history?months=${months}`
    );
  }

  changeTenantPlan(id: string, plan: string): Promise<{ tenant_id: string; plan: Plan }> {
    return this.request(`/tenants/${encodeURIComponent(id)}/plan`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({ plan }),
    });
  }

  listPlans(): Promise<{ plans: Plan[] }> {
    return this.request("/plans");
  }

  // --- KType registry ----------------------------------------------------
  listKTypes(): Promise<KType[]> {
    return this.request("/ktypes");
  }

  getKType(name: string): Promise<KType> {
    return this.request(`/ktypes/${encodeURIComponent(name)}`);
  }

  registerKType(kt: {
    name: string;
    version: number;
    schema: KTypeSchema;
  }): Promise<{ name: string; version: number }> {
    return this.request("/ktypes", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(kt),
    });
  }

  // --- KRecord CRUD ------------------------------------------------------
  listRecords(ktype: string): Promise<KRecord[]> {
    return this.request(`/records/${encodeURIComponent(ktype)}`);
  }

  getRecord(ktype: string, id: string): Promise<KRecord> {
    return this.request(
      `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(id)}`
    );
  }

  createRecord(
    ktype: string,
    data: Record<string, unknown>
  ): Promise<KRecord> {
    return this.request(`/records/${encodeURIComponent(ktype)}`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({ data }),
    });
  }

  updateRecord(
    ktype: string,
    id: string,
    data: Record<string, unknown>
  ): Promise<KRecord> {
    return this.request(
      `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(id)}`,
      {
        method: "PATCH",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({ data }),
      }
    );
  }

  deleteRecord(ktype: string, id: string): Promise<void> {
    return this.request(
      `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(id)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  /** Run a bulk operation over the selected record ids. The backend
   *  runs every mutation inside one `WithTenantTx` transaction so
   *  the batch commits as a unit; export streams CSV directly. */
  bulkRecords(
    ktype: string,
    input: {
      ids: string[];
      action: "status_change" | "delete";
      payload?: Record<string, unknown>;
    }
  ): Promise<BulkRecordResult> {
    return this.request(`/records/${encodeURIComponent(ktype)}/bulk`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  /** Export the selected records as CSV. Returns the raw text
   *  response body so callers can plug it into Blob + download. */
  async bulkExportRecords(
    ktype: string,
    ids: string[]
  ): Promise<string> {
    const res = await fetch(
      `${this.cfg.baseUrl}/records/${encodeURIComponent(ktype)}/bulk`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": crypto.randomUUID(),
          ...this.cfg.headers(),
        },
        body: JSON.stringify({ ids, action: "export" }),
      }
    );
    if (!res.ok) {
      throw new Error(`${res.status} ${res.statusText}`);
    }
    return await res.text();
  }

  // --- Print / PDF --------------------------------------------------------

  /** Fetch the record's PDF as a Blob. Uses the regular Fetch
   *  pipeline so X-Tenant-ID + Authorization headers are attached;
   *  browser anchor navigation skips custom headers, so the callers
   *  must pipe this blob into an object URL + programmatic click. */
  async recordPdf(ktype: string, id: string): Promise<Blob> {
    return this.fetchBlob(
      `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(id)}/pdf`
    );
  }

  /** Same shape as recordPdf for the HTML preview variant. */
  async recordHtml(ktype: string, id: string): Promise<Blob> {
    return this.fetchBlob(
      `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(id)}/html`
    );
  }

  /** Shared helper: issue a GET with the configured auth/tenant
   *  headers and return the response body as a Blob. Used by the
   *  print endpoints which return binary (PDF) or text (HTML). */
  private async fetchBlob(path: string): Promise<Blob> {
    const res = await fetch(`${this.cfg.baseUrl}${path}`, {
      headers: { ...this.cfg.headers() },
    });
    if (!res.ok) {
      throw new Error(`${res.status} ${res.statusText}`);
    }
    return await res.blob();
  }

  // --- Webhooks ----------------------------------------------------------

  listWebhooks(): Promise<{ webhooks: Webhook[] }> {
    return this.request(`/webhooks`);
  }
  getWebhook(id: string): Promise<Webhook> {
    return this.request(`/webhooks/${encodeURIComponent(id)}`);
  }
  createWebhook(input: {
    url: string;
    secret: string;
    event_filters?: string[];
    conditions?: Record<string, unknown>;
    max_retries?: number;
    backoff_base_seconds?: number;
    active?: boolean;
  }): Promise<Webhook> {
    return this.request(`/webhooks`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }
  updateWebhook(
    id: string,
    input: {
      url?: string;
      secret?: string;
      event_filters?: string[];
      conditions?: Record<string, unknown>;
      max_retries?: number;
      backoff_base_seconds?: number;
      active?: boolean;
    }
  ): Promise<Webhook> {
    return this.request(`/webhooks/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }
  deleteWebhook(id: string): Promise<void> {
    return this.request(`/webhooks/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }
  listWebhookDeliveries(
    id: string,
    limit?: number
  ): Promise<{ deliveries: WebhookDelivery[] }> {
    const qs = limit ? `?limit=${limit}` : "";
    return this.request(`/webhooks/${encodeURIComponent(id)}/deliveries${qs}`);
  }

  // --- Search ------------------------------------------------------------

  /** Full-text search across krecords for the current tenant.
   *  `ktypes`, when supplied, restricts the scan to the listed KType
   *  names so the UI can render per-domain tabs. Results are ranked
   *  by `ts_rank` server-side; callers should render them in order. */
  searchRecords(params: {
    q: string;
    ktypes?: string[];
    limit?: number;
  }): Promise<SearchResponse> {
    const qs = new URLSearchParams({ q: params.q });
    for (const k of params.ktypes ?? []) {
      qs.append("ktype", k);
    }
    if (params.limit !== undefined) {
      qs.set("limit", String(params.limit));
    }
    return this.request(`/search?${qs.toString()}`);
  }

  // --- Workflow ----------------------------------------------------------

  /** Drives a workflow transition on a record. Callers pick the action
   *  name from the KType's workflow.transitions list.
   */
  runAction(
    ktype: string,
    id: string,
    action: string
  ): Promise<{ record: KRecord; run: WorkflowRun }> {
    return this.request(
      `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(id)}/actions/${encodeURIComponent(action)}`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      }
    );
  }

  /** Fetch the authoritative workflow run for a record. Returns null
   *  when the record has no run yet so callers can render the "no
   *  workflow" state without branching on fetch errors.
   */
  async getWorkflowRun(ktype: string, recordId: string): Promise<WorkflowRun | null> {
    try {
      return await this.request<WorkflowRun>(
        `/records/${encodeURIComponent(ktype)}/${encodeURIComponent(recordId)}/workflow-run`
      );
    } catch (err) {
      if (err instanceof Error && err.message.startsWith("404")) {
        return null;
      }
      throw err;
    }
  }

  // --- Approvals ---------------------------------------------------------

  listApprovals(): Promise<Approval[]> {
    return this.request("/approvals");
  }

  getApproval(id: string): Promise<Approval> {
    return this.request(`/approvals/${encodeURIComponent(id)}`);
  }

  requestApproval(input: {
    record_ktype: string;
    record_id: string;
    chain: ApprovalChain;
  }): Promise<Approval> {
    return this.request("/approvals", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  decideApproval(id: string, decision: "approve" | "reject"): Promise<Approval> {
    return this.request(`/approvals/${encodeURIComponent(id)}/decide`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({ decision }),
    });
  }

  // --- Agent tools -------------------------------------------------------

  listAgentTools(): Promise<{ tools: string[] }> {
    return this.request("/agents/tools");
  }

  invokeAgentTool(
    name: string,
    invocation: {
      mode: "dry_run" | "commit";
      inputs: Record<string, unknown>;
      confirmed?: boolean;
    }
  ): Promise<AgentInvocationResult> {
    return this.request(`/agents/tools/${encodeURIComponent(name)}`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(invocation),
    });
  }

  // --- Audit log ---------------------------------------------------------

  listAuditLog(params?: {
    target_ktype?: string;
    target_id?: string;
    limit?: number;
    offset?: number;
  }): Promise<AuditEntry[]> {
    const qs = new URLSearchParams();
    if (params?.target_ktype) qs.set("target_ktype", params.target_ktype);
    if (params?.target_id) qs.set("target_id", params.target_id);
    if (params?.limit !== undefined) qs.set("limit", String(params.limit));
    if (params?.offset !== undefined) qs.set("offset", String(params.offset));
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return this.request(`/audit${suffix}`);
  }

  // --- Finance ----------------------------------------------------------

  /** Create a chart-of-accounts entry. The server enforces per-tenant
   *  uniqueness on `code`; conflicts surface as 409. */
  createAccount(input: FinanceAccountInput): Promise<FinanceAccount> {
    return this.request("/finance/accounts", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  listAccounts(params?: {
    type?: "asset" | "liability" | "equity" | "revenue" | "expense";
  }): Promise<FinanceAccount[]> {
    const qs = new URLSearchParams();
    if (params?.type) qs.set("type", params.type);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return this.request(`/finance/accounts${suffix}`);
  }

  getAccount(code: string): Promise<FinanceAccount> {
    return this.request(`/finance/accounts/${encodeURIComponent(code)}`);
  }

  postJournalEntry(input: PostJournalEntryInput): Promise<JournalEntry> {
    return this.request("/finance/journal-entries", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  listJournalEntries(params?: {
    from?: string;
    to?: string;
    source_ktype?: string;
    source_id?: string;
  }): Promise<JournalEntry[]> {
    const qs = new URLSearchParams();
    if (params?.from) qs.set("from", params.from);
    if (params?.to) qs.set("to", params.to);
    if (params?.source_ktype) qs.set("source_ktype", params.source_ktype);
    if (params?.source_id) qs.set("source_id", params.source_id);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return this.request(`/finance/journal-entries${suffix}`);
  }

  getJournalEntry(id: string): Promise<JournalEntry> {
    return this.request(`/finance/journal-entries/${encodeURIComponent(id)}`);
  }

  /** Post a draft finance.ar_invoice KRecord to the ledger. */
  postInvoice(id: string): Promise<JournalEntry> {
    return this.request(
      `/finance/invoices/${encodeURIComponent(id)}/post`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      }
    );
  }

  /** Post a draft finance.ap_bill KRecord to the ledger. */
  postBill(id: string): Promise<JournalEntry> {
    return this.request(`/finance/bills/${encodeURIComponent(id)}/post`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({}),
    });
  }

  /** Generate draft payslips for every eligible employee under a
   *  pay_run. Idempotent per (pay_run_id, employee_id). */
  generatePayslips(payRunId: string): Promise<PayslipGenerateResult> {
    return this.request(
      `/hr/pay-runs/${encodeURIComponent(payRunId)}/generate`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      }
    );
  }

  /** Post every approved payslip on a pay_run as a single journal
   *  entry (Dr salary expense / Cr salary payable). */
  postPayRun(payRunId: string): Promise<JournalEntry> {
    return this.request(
      `/hr/pay-runs/${encodeURIComponent(payRunId)}/post`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      }
    );
  }

  /** List every payslip KRecord attached to a pay_run. Uses the
   *  dedicated /hr/pay-runs/:id/payslips endpoint instead of the
   *  generic records list route, which caps at 500 rows and
   *  defaults to 50 — callers that need every slip for a run
   *  should prefer this. */
  listPayRunPayslips(payRunId: string): Promise<KRecord[]> {
    return this.request(
      `/hr/pay-runs/${encodeURIComponent(payRunId)}/payslips`
    );
  }

  /** Post a draft finance.credit_note KRecord. Reverses the AR posting
   *  of the referenced invoice (Dr Revenue, Cr AR). */
  postCreditNote(id: string): Promise<JournalEntry> {
    return this.request(
      `/finance/credit-notes/${encodeURIComponent(id)}/post`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      }
    );
  }

  /** Post a draft finance.debit_note KRecord. Reverses the AP posting
   *  of the referenced bill (Dr AP, Cr Expense). */
  postDebitNote(id: string): Promise<JournalEntry> {
    return this.request(
      `/finance/debit-notes/${encodeURIComponent(id)}/post`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      }
    );
  }

  listTaxCodes(): Promise<TaxCode[]> {
    return this.request("/finance/tax-codes");
  }

  getTaxCode(code: string): Promise<TaxCode> {
    return this.request(`/finance/tax-codes/${encodeURIComponent(code)}`);
  }

  upsertTaxCode(input: UpsertTaxCodeInput): Promise<TaxCode> {
    return this.request("/finance/tax-codes", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  lockPeriod(input: { period_start: string }): Promise<FiscalPeriod> {
    return this.request("/finance/periods/lock", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  getTrialBalance(asOf?: string): Promise<TrialBalanceReport> {
    const qs = asOf ? `?as_of=${encodeURIComponent(asOf)}` : "";
    return this.request(`/finance/reports/trial-balance${qs}`);
  }

  getARAgingReport(asOf?: string): Promise<AgingReport> {
    const qs = asOf ? `?as_of=${encodeURIComponent(asOf)}` : "";
    return this.request(`/finance/reports/ar-aging${qs}`);
  }

  getAPAgingReport(asOf?: string): Promise<AgingReport> {
    const qs = asOf ? `?as_of=${encodeURIComponent(asOf)}` : "";
    return this.request(`/finance/reports/ap-aging${qs}`);
  }

  getIncomeStatement(from: string, to: string): Promise<IncomeStatement> {
    const qs = new URLSearchParams({ from, to }).toString();
    return this.request(`/finance/reports/income-statement?${qs}`);
  }

  // --- Inventory (Phase D) ----------------------------------------------

  listInventoryItems(): Promise<InventoryItem[]> {
    return this.request("/inventory/items");
  }

  listInventoryWarehouses(): Promise<InventoryWarehouse[]> {
    return this.request("/inventory/warehouses");
  }

  listStockLevels(itemId?: string): Promise<StockLevel[]> {
    if (itemId) {
      return this.request(`/inventory/stock-levels/${encodeURIComponent(itemId)}`);
    }
    return this.request("/inventory/stock-levels");
  }

  getInventoryValuation(asOf?: string): Promise<InventoryValuationReport> {
    const qs = asOf ? `?as_of=${encodeURIComponent(asOf)}` : "";
    return this.request(`/inventory/reports/valuation${qs}`);
  }

  /** Create a per-tenant lot identifier for an item. */
  createInventoryBatch(input: {
    item_id: string;
    batch_no: string;
    manufactured_at?: string;
    expires_at?: string;
  }): Promise<InventoryBatch> {
    return this.request("/inventory/batches", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  /** List the batches defined for the supplied item, ordered FEFO. */
  listInventoryBatchesByItem(itemId: string): Promise<InventoryBatch[]> {
    return this.request(`/inventory/items/${encodeURIComponent(itemId)}/batches`);
  }

  // --- Saved views (Phase G) -------------------------------------------

  /** List saved views for the caller, scoped to a KType. Returns the
   *  user's own views plus any view another user has flagged
   *  `shared`. Ordered with default views first. */
  listViews(ktype: string): Promise<SavedView[]> {
    return this.request(`/views?ktype=${encodeURIComponent(ktype)}`);
  }

  createView(input: CreateViewInput): Promise<SavedView> {
    return this.request("/views", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateView(id: string, patch: UpdateViewInput): Promise<SavedView> {
    return this.request(`/views/${encodeURIComponent(id)}`, {
      method: "PATCH",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(patch),
    });
  }

  deleteView(id: string): Promise<void> {
    return this.request(`/views/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  // --- Forms ------------------------------------------------------------

  /** Public (unauthenticated) fetch of a form's schema + config by id. */
  getPublicForm(id: string): Promise<{ form: Form; schema: KTypeSchema }> {
    return this.request(`/forms/${encodeURIComponent(id)}`);
  }

  submitPublicForm(
    id: string,
    data: Record<string, unknown>
  ): Promise<KRecord> {
    return this.request(`/forms/${encodeURIComponent(id)}/submit`, {
      method: "POST",
      body: JSON.stringify({ data }),
    });
  }

  // --- Phase I: multi-currency ------------------------------------------

  listExchangeRates(params?: {
    from?: string;
    to?: string;
    limit?: number;
  }): Promise<{ rates: ExchangeRate[] }> {
    const qs = new URLSearchParams();
    if (params?.from) qs.set("from", params.from);
    if (params?.to) qs.set("to", params.to);
    if (params?.limit !== undefined) qs.set("limit", String(params.limit));
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return this.request(`/finance/exchange-rates${suffix}`);
  }

  upsertExchangeRate(input: UpsertExchangeRateInput): Promise<ExchangeRate> {
    return this.request("/finance/exchange-rates", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  convertCurrency(params: {
    from: string;
    to: string;
    amount: string;
    date?: string;
  }): Promise<ExchangeConversion> {
    const qs = new URLSearchParams({ from: params.from, to: params.to, amount: params.amount });
    if (params.date) qs.set("date", params.date);
    return this.request(`/finance/exchange-rates/convert?${qs.toString()}`);
  }

  unrealizedGainLoss(input: UnrealizedGLInput): Promise<{ unrealized_gain_loss: string }> {
    return this.request("/finance/exchange-rates/unrealized", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  // --- Phase I: helpdesk -------------------------------------------------

  listSLAPolicies(): Promise<{ policies: SLAPolicy[] }> {
    return this.request("/helpdesk/sla-policies");
  }

  upsertSLAPolicy(input: UpsertSLAPolicyInput): Promise<SLAPolicy> {
    return this.request("/helpdesk/sla-policies", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  getTicketSLALog(ticketId: string): Promise<{ entries: SLALogEntry[] }> {
    return this.request(`/helpdesk/tickets/${encodeURIComponent(ticketId)}/sla-log`);
  }

  // --- Phase I: reports --------------------------------------------------

  listReports(): Promise<{ reports: SavedReport[] }> {
    return this.request("/reports");
  }

  getReport(id: string): Promise<SavedReport> {
    return this.request(`/reports/${encodeURIComponent(id)}`);
  }

  createReport(input: {
    name: string;
    description?: string;
    definition: ReportDefinition;
  }): Promise<SavedReport> {
    return this.request("/reports", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateReport(
    id: string,
    input: { name: string; description?: string; definition: ReportDefinition }
  ): Promise<SavedReport> {
    return this.request(`/reports/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteReport(id: string): Promise<void> {
    return this.request(`/reports/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  runSavedReport(id: string): Promise<ReportResult> {
    return this.request(`/reports/${encodeURIComponent(id)}/run`);
  }

  runAdhocReport(def: ReportDefinition): Promise<ReportResult> {
    return this.request("/reports/run", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(def),
    });
  }

  // --- Phase I: dashboard ------------------------------------------------

  getDashboardSummary(): Promise<DashboardSummary> {
    return this.request("/dashboard/summary");
  }

  // --- Phase L: insights -------------------------------------------------

  listInsightsQueries(): Promise<{ queries: InsightsQuery[] }> {
    return this.request("/insights/queries");
  }

  getInsightsQuery(id: string): Promise<InsightsQuery> {
    return this.request(`/insights/queries/${encodeURIComponent(id)}`);
  }

  createInsightsQuery(input: InsightsQueryInput): Promise<InsightsQuery> {
    return this.request("/insights/queries", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateInsightsQuery(
    id: string,
    input: InsightsQueryInput
  ): Promise<InsightsQuery> {
    return this.request(`/insights/queries/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteInsightsQuery(id: string): Promise<void> {
    return this.request(`/insights/queries/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  runInsightsQuery(
    id: string,
    body: InsightsRunInput = {}
  ): Promise<InsightsRunResult> {
    return this.request(`/insights/queries/${encodeURIComponent(id)}/run`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(body),
    });
  }

  // Phase M raw-SQL editor mode. Gated server-side on both the
  // `insights` flag (parent route) and `insights_sql_editor`
  // (sub-route) — a non-enterprise plan returns a 403 envelope keyed
  // on `insights_sql_editor` so the UI can render an upgrade banner.
  runInsightsQuerySQL(
    id: string,
    body: InsightsRunSQLInput = {}
  ): Promise<InsightsRunResult> {
    return this.request(
      `/insights/queries/${encodeURIComponent(id)}/run-sql`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(body),
      }
    );
  }

  listInsightsQueryShares(id: string): Promise<{ shares: InsightsShare[] }> {
    return this.request(
      `/insights/queries/${encodeURIComponent(id)}/shares`
    );
  }

  shareInsightsQuery(
    id: string,
    input: InsightsShareInput
  ): Promise<InsightsShare> {
    return this.request(`/insights/queries/${encodeURIComponent(id)}/share`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteInsightsQueryShare(id: string, shareId: string): Promise<void> {
    return this.request(
      `/insights/queries/${encodeURIComponent(id)}/shares/${encodeURIComponent(
        shareId
      )}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  listInsightsDashboards(): Promise<{ dashboards: InsightsDashboard[] }> {
    return this.request("/insights/dashboards");
  }

  getInsightsDashboard(id: string): Promise<InsightsDashboardBundle> {
    return this.request(`/insights/dashboards/${encodeURIComponent(id)}`);
  }

  createInsightsDashboard(
    input: InsightsDashboardInput
  ): Promise<InsightsDashboard> {
    return this.request("/insights/dashboards", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateInsightsDashboard(
    id: string,
    input: InsightsDashboardInput
  ): Promise<InsightsDashboard> {
    return this.request(`/insights/dashboards/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteInsightsDashboard(id: string): Promise<void> {
    return this.request(`/insights/dashboards/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  upsertInsightsWidget(
    dashboardId: string,
    input: InsightsWidgetInput
  ): Promise<InsightsWidget> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(dashboardId)}/widgets`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }

  deleteInsightsWidget(dashboardId: string, widgetId: string): Promise<void> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(
        dashboardId
      )}/widgets/${encodeURIComponent(widgetId)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  listInsightsDashboardShares(
    id: string
  ): Promise<{ shares: InsightsShare[] }> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(id)}/shares`
    );
  }

  shareInsightsDashboard(
    id: string,
    input: InsightsShareInput
  ): Promise<InsightsShare> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(id)}/share`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }

  deleteInsightsDashboardShare(id: string, shareId: string): Promise<void> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(
        id
      )}/shares/${encodeURIComponent(shareId)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  // --- Phase L deferred: insights data sources --------------------------

  listInsightsDataSources(): Promise<{ data_sources: InsightsDataSource[] }> {
    return this.request("/insights/data-sources");
  }

  createInsightsDataSource(
    input: InsightsDataSourceInput
  ): Promise<InsightsDataSource> {
    return this.request("/insights/data-sources", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateInsightsDataSource(
    id: string,
    input: InsightsDataSourceInput
  ): Promise<InsightsDataSource> {
    return this.request(`/insights/data-sources/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteInsightsDataSource(id: string): Promise<void> {
    return this.request(`/insights/data-sources/${encodeURIComponent(id)}`, {
      method: "DELETE",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  testInsightsDataSource(id: string): Promise<{ ok: boolean }> {
    return this.request(
      `/insights/data-sources/${encodeURIComponent(id)}/test`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  // --- Phase L deferred: insights dashboard embeds ----------------------

  listInsightsEmbeds(
    dashboardId: string
  ): Promise<{ embeds: InsightsEmbed[] }> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(dashboardId)}/embeds`
    );
  }

  createInsightsEmbed(
    dashboardId: string,
    input: InsightsEmbedInput
  ): Promise<InsightsEmbed> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(dashboardId)}/embeds`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }

  revokeInsightsEmbed(
    dashboardId: string,
    embedId: string
  ): Promise<void> {
    return this.request(
      `/insights/dashboards/${encodeURIComponent(
        dashboardId
      )}/embeds/${encodeURIComponent(embedId)}/revoke`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  // --- Phase G: tenant tier upgrade -------------------------------------

  upgradeTenantTier(
    id: string,
    input: { target_tier: "dedicated_schema" | "dedicated_db" }
  ): Promise<Tenant> {
    return this.request(
      `/admin/tenants/${encodeURIComponent(id)}/upgrade-tier`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }
}

// --- Bulk actions -----------------------------------------------------

export interface BulkRecordError {
  id: string;
  error: string;
}

export interface BulkRecordResult {
  succeeded: string[];
  failed: BulkRecordError[];
}

// --- Search -----------------------------------------------------------

export interface SearchResult extends KRecord {
  rank: number;
}

export interface SearchResponse {
  query: string;
  results: SearchResult[];
}

// --- Webhooks ---------------------------------------------------------

export interface Webhook {
  id: string;
  tenant_id: string;
  url: string;
  secret: string;
  event_filters: string[];
  conditions?: Record<string, unknown>;
  max_retries: number;
  backoff_base_seconds: number;
  active: boolean;
  created_at: string;
  updated_at: string;
}

export interface WebhookDelivery {
  id: string;
  tenant_id: string;
  webhook_id: string;
  event_id: string;
  event_type: string;
  status_code?: number;
  response_body?: string;
  attempt: number;
  delivered: boolean;
  error?: string;
  next_retry_at?: string;
  created_at: string;
}

// --- Phase I auxiliary types ------------------------------------------

export interface ExchangeRate {
  tenant_id: string;
  from_currency: string;
  to_currency: string;
  rate_date: string;
  rate: string;
  provider?: string;
  created_by?: string | null;
  created_at: string;
  updated_at: string;
}

export interface UpsertExchangeRateInput {
  from_currency: string;
  to_currency: string;
  rate_date: string;
  rate: string;
  provider?: string;
}

export interface ExchangeConversion {
  amount: string;
  from: string;
  to: string;
  date: string;
  rate: string;
  converted: string;
}

export interface UnrealizedGLInput {
  foreign_amount: string;
  foreign_currency: string;
  functional_currency: string;
  original_rate: string;
  as_of?: string;
}

export interface SLAPolicy {
  tenant_id: string;
  id: string;
  name: string;
  priority: "low" | "medium" | "high" | "urgent";
  response_minutes: number;
  resolution_minutes: number;
  active: boolean;
  created_by?: string | null;
  created_at: string;
  updated_at: string;
}

export interface UpsertSLAPolicyInput {
  id?: string;
  name: string;
  priority: "low" | "medium" | "high" | "urgent";
  response_minutes: number;
  resolution_minutes: number;
  active?: boolean;
}

export interface SLALogEntry {
  id: number;
  tenant_id: string;
  ticket_id: string;
  event_kind: string;
  occurred_at: string;
  details?: Record<string, unknown>;
}

export interface ReportFilter {
  column: string;
  op: string;
  value?: unknown;
}

export interface ReportAggregation {
  op: "count" | "sum" | "avg" | "min" | "max";
  column?: string;
  alias?: string;
}

export interface ReportSort {
  column: string;
  direction?: "asc" | "desc";
}

export interface ReportChartSpec {
  type: "bar" | "line" | "pie";
  x_column: string;
  y_column: string;
}

export interface ReportPivotSpec {
  row_column: string;
  column_column: string;
  value_column: string;
}

export interface ReportDefinition {
  source: string;
  columns: string[];
  filters?: ReportFilter[];
  group_by?: string[];
  aggregations?: ReportAggregation[];
  sort?: ReportSort[];
  limit?: number;
  pivot?: ReportPivotSpec;
  chart?: ReportChartSpec;
}

export interface ReportPivotResult {
  row_headers: unknown[];
  column_headers: unknown[];
  cells: unknown[][];
}

export interface ReportResult {
  columns: string[];
  rows: Array<Record<string, unknown>>;
  pivot?: ReportPivotResult | null;
  chart?: ReportChartSpec | null;
  summary?: Record<string, unknown>;
}

export interface SavedReport {
  tenant_id: string;
  id: string;
  name: string;
  description?: string;
  definition: ReportDefinition;
  created_by?: string | null;
  created_at: string;
  updated_at: string;
}

export interface DashboardSummary {
  open_deals_count: number;
  pipeline_value: number;
  outstanding_ar: number;
  outstanding_ap: number;
  low_stock_items_count: number;
  pending_approvals: number;
  open_tickets_count: number;
  overdue_tickets_count: number;
  /** Distinct employees with an hr.attendance record dated today and
   *  status in {present, half_day}. Always present in API responses;
   *  optional on the type for backward compatibility with legacy
   *  fixtures the test suite hard-codes. */
  present_today?: number;
  /** Count of hr.appraisal records in the {submitted, reviewed}
   *  band — i.e. awaiting reviewer or employee action. Optional
   *  for backward compatibility with pre-Phase-M-Task-4 servers. */
  pending_reviews?: number;
  /** ISO-4217 functional currency the monetary fields are denominated in. */
  base_currency: string;
}

// --- Auxiliary types ---------------------------------------------------

export interface WorkflowRun {
  id: string;
  tenant_id: string;
  workflow: string;
  record_id: string;
  state: string;
  history: Array<{
    from_state: string;
    to_state: string;
    action: string;
    actor_id: string;
    timestamp: string;
  }>;
  created_at: string;
  updated_at: string;
}

export interface ApprovalStep {
  approvers: string[];
  required_count: number;
}

export interface ApprovalAction {
  step_index: number;
  actor_id: string;
  decision: "approve" | "reject";
  timestamp: string;
}

export interface ApprovalChain {
  steps: ApprovalStep[];
  current_step: number;
  requested_by: string;
  history: ApprovalAction[];
}

export interface Approval {
  id: string;
  tenant_id: string;
  record_ktype: string;
  record_id: string;
  chain: ApprovalChain;
  state: "pending" | "approved" | "rejected";
  created_at: string;
}

export interface AgentInvocationResult {
  tool: string;
  mode: "dry_run" | "commit";
  result: unknown;
  audit: {
    action: string;
    target_ktype?: string;
    target_id?: string | null;
  };
}

export interface AuditEntry {
  id: number;
  tenant_id: string;
  actor_id?: string | null;
  actor_kind: "user" | "agent" | "system";
  action: string;
  target_ktype?: string;
  target_id?: string | null;
  before?: unknown;
  after?: unknown;
  context?: unknown;
  created_at: string;
}

// --- Finance ------------------------------------------------------------

export type AccountType =
  | "asset"
  | "liability"
  | "equity"
  | "revenue"
  | "expense";

export interface FinanceAccount {
  tenant_id: string;
  code: string;
  name: string;
  type: AccountType;
  parent_code?: string;
  active: boolean;
}

export interface FinanceAccountInput {
  code: string;
  name: string;
  type: AccountType;
  parent_code?: string;
  active?: boolean;
}

export interface JournalLineInput {
  account_code: string;
  debit?: string | number;
  credit?: string | number;
  currency?: string;
  memo?: string;
}

export interface PostJournalEntryInput {
  posted_at?: string;
  memo?: string;
  source_ktype?: string;
  source_id?: string;
  lines: JournalLineInput[];
}

export interface JournalLine {
  id: number;
  tenant_id: string;
  entry_id: string;
  account_code: string;
  debit: string;
  credit: string;
  currency: string;
  memo: string;
}

export interface JournalEntry {
  id: string;
  tenant_id: string;
  posted_at: string;
  memo: string;
  source_ktype: string;
  source_id?: string | null;
  created_by: string;
  created_at: string;
  lines: JournalLine[];
}

/** Summary returned by POST /hr/pay-runs/:id/generate. */
export interface PayslipGenerateResult {
  payslip_ids: string[];
  created_count: number;
  skipped_existing: number;
  skipped_no_structure: number;
}

export interface TaxCode {
  tenant_id: string;
  code: string;
  name: string;
  rate: string;
  type: "inclusive" | "exclusive";
  active: boolean;
}

export interface UpsertTaxCodeInput {
  code: string;
  name: string;
  rate: string | number;
  type?: "inclusive" | "exclusive";
  active?: boolean;
}

export interface FiscalPeriod {
  tenant_id: string;
  period_start: string;
  period_end: string;
  locked: boolean;
  locked_at?: string | null;
  locked_by?: string | null;
}

export interface TrialBalanceRow {
  account_code: string;
  account_name: string;
  type: AccountType;
  debit: string;
  credit: string;
  balance: string;
}

export interface TrialBalanceReport {
  tenant_id: string;
  as_of: string;
  rows: TrialBalanceRow[];
  total_debit: string;
  total_credit: string;
  residual: string;
}

export interface AgingBucket {
  label: string;
  amount: string;
}

export interface AgingRow {
  source_id: string;
  counterparty_id: string;
  number: string;
  due_date?: string | null;
  total: string;
  currency: string;
  days_overdue: number;
  bucket: string;
}

export interface AgingReport {
  as_of: string;
  rows: AgingRow[];
  buckets: AgingBucket[];
  total: string;
}

export interface IncomeStatementLine {
  account_code: string;
  account_name: string;
  amount: string;
}

export interface IncomeStatement {
  from: string;
  to: string;
  revenue: IncomeStatementLine[];
  expense: IncomeStatementLine[];
  total_revenue: string;
  total_expense: string;
  net_income: string;
}

// --- Inventory ---------------------------------------------------------

export interface InventoryItem {
  tenant_id: string;
  id: string;
  sku: string;
  name: string;
  uom: string;
  active: boolean;
  reorder_level: string;
}

export interface InventoryWarehouse {
  tenant_id: string;
  id: string;
  code: string;
  name: string;
}

export interface StockLevel {
  tenant_id: string;
  item_id: string;
  warehouse_id: string;
  qty: string;
}

export interface InventoryBatch {
  tenant_id: string;
  id: string;
  item_id: string;
  batch_no: string;
  manufactured_at?: string | null;
  expires_at?: string | null;
  qty_on_hand: string;
  metadata?: unknown;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface InventoryValuationRow {
  item_id: string;
  sku: string;
  name: string;
  qty: string;
  value_cost: string;
}

export interface InventoryValuationReport {
  as_of: string;
  rows: InventoryValuationRow[];
  total_value: string;
}

// --- Saved views (Phase G) ---------------------------------------------

export interface SavedView {
  tenant_id: string;
  id: string;
  user_id: string;
  ktype: string;
  name: string;
  filters: Record<string, unknown>;
  sort: string;
  columns: string[];
  is_default: boolean;
  shared: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateViewInput {
  ktype: string;
  name: string;
  filters?: Record<string, unknown>;
  sort?: string;
  columns?: string[];
  is_default?: boolean;
  shared?: boolean;
}

export interface UpdateViewInput {
  name?: string;
  filters?: Record<string, unknown>;
  sort?: string;
  columns?: string[];
  is_default?: boolean;
  shared?: boolean;
}

export interface Form {
  id: string;
  tenant_id: string;
  ktype: string;
  config: {
    allow_anonymous?: boolean;
    require_auth?: boolean;
    redirect_url?: string;
    title?: string;
    description?: string;
  };
  status: string;
  created_at: string;
  updated_at: string;
}

// --- Phase L: Insights -------------------------------------------------

export interface CalculatedColumn {
  name: string;
  expression: string;
  type?: string;
}

export interface InsightsQueryDefinition extends ReportDefinition {
  calculated_columns?: CalculatedColumn[];
}

// QueryMode discriminates the two flavours a saved insights query can
// take. "visual" flows through the structured reporting.Definition
// grammar; "sql" carries a parameterised SQL body in raw_sql and runs
// under the per-tenant statement_timeout + RLS fences. Phase M; gated
// by the `insights_sql_editor` feature flag (enterprise plan only).
export type InsightsQueryMode = "visual" | "sql";

export interface InsightsQueryInput {
  name: string;
  description?: string;
  definition: InsightsQueryDefinition;
  cache_ttl_seconds?: number;
  mode?: InsightsQueryMode;
  raw_sql?: string;
}

export interface InsightsQuery {
  tenant_id: string;
  id: string;
  name: string;
  description?: string;
  definition: InsightsQueryDefinition;
  cache_ttl_seconds?: number;
  mode?: InsightsQueryMode;
  raw_sql?: string;
  created_by?: string | null;
  created_at: string;
  updated_at: string;
}

export interface InsightsRunInput {
  filter_params?: Record<string, unknown>;
  bypass_cache?: boolean;
}

export interface InsightsRunSQLInput {
  raw_sql?: string;
  params?: unknown[];
}

export interface InsightsRunResult {
  result: ReportResult;
  cache_hit: boolean;
  query_hash: string;
  filter_hash: string;
  expires_at?: string | null;
}

export type InsightsVizType =
  | "table"
  | "bar"
  | "line"
  | "pie"
  | "donut"
  | "funnel"
  | "number_card"
  | "pivot";

export interface InsightsWidgetPosition {
  x?: number;
  y?: number;
  w?: number;
  h?: number;
}

export interface InsightsWidgetConfig {
  title?: string;
  x_column?: string;
  y_column?: string;
  value_column?: string;
  category_column?: string;
  // Linked-filter binding: when present, the dashboard's globally
  // selected value for `linked_filter_key` is appended to the
  // widget's per-run filter_params under this column name.
  linked_filter_column?: string;
  linked_filter_key?: string;
  format?: string;
  [extra: string]: unknown;
}

export interface InsightsWidget {
  tenant_id: string;
  id: string;
  dashboard_id: string;
  query_id?: string | null;
  viz_type: InsightsVizType;
  position: InsightsWidgetPosition;
  config: InsightsWidgetConfig;
  created_at: string;
  updated_at: string;
}

export interface InsightsWidgetInput {
  id?: string;
  query_id?: string | null;
  viz_type: InsightsVizType;
  position?: InsightsWidgetPosition;
  config?: InsightsWidgetConfig;
}

export interface InsightsDashboardLayout {
  // Free-form JSON the dashboard page persists alongside widgets — used
  // by the grid layout component to track widget positioning at the
  // dashboard level. Per-widget position is the source of truth; layout
  // here covers cross-widget concerns (linked filter selections, panel
  // order, etc).
  linked_filters?: Record<string, unknown>;
  [extra: string]: unknown;
}

export interface InsightsDashboard {
  tenant_id: string;
  id: string;
  name: string;
  description?: string;
  layout: InsightsDashboardLayout;
  auto_refresh_seconds: number;
  created_by?: string | null;
  created_at: string;
  updated_at: string;
  widgets?: InsightsWidget[];
}

export interface InsightsDashboardInput {
  name: string;
  description?: string;
  layout?: InsightsDashboardLayout;
  auto_refresh_seconds?: number;
}

export interface InsightsDashboardBundle {
  dashboard: InsightsDashboard;
  widget_results: Record<string, InsightsRunResult | null>;
}

export type InsightsGranteeType = "user" | "role";
export type InsightsPermission = "view" | "edit";

export interface InsightsShare {
  tenant_id: string;
  id: string;
  resource_type: "query" | "dashboard";
  resource_id: string;
  grantee_type: InsightsGranteeType;
  grantee: string;
  permission: InsightsPermission;
  created_at: string;
}

export interface InsightsShareInput {
  grantee_type: InsightsGranteeType;
  grantee: string;
  permission?: InsightsPermission;
}

// --- Phase L deferred: external data sources --------------------------

export interface InsightsDataSource {
  tenant_id: string;
  id: string;
  name: string;
  description?: string;
  dialect: "postgres";
  // The server returns plaintext for connection_string only on
  // create/update; subsequent fetches return an empty string and the
  // UI displays a "credentials hidden" placeholder.
  connection_string?: string;
  enabled: boolean;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface InsightsDataSourceInput {
  name: string;
  description?: string;
  dialect: "postgres";
  connection_string?: string;
  secret_blob?: string;
  enabled?: boolean;
}

// --- Phase L deferred: dashboard embeds -------------------------------

export interface InsightsEmbed {
  tenant_id: string;
  id: string;
  dashboard_id: string;
  // Plaintext token is only ever populated by createInsightsEmbed.
  token?: string;
  token_digest: string;
  scoped_filters?: Record<string, unknown>;
  max_views?: number;
  view_count: number;
  expires_at?: string | null;
  revoked_at?: string | null;
  created_by?: string;
  created_at: string;
}

export interface InsightsEmbedInput {
  scoped_filters?: Record<string, unknown>;
  max_views?: number;
  expires_in_days?: number;
}
