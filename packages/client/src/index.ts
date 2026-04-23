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
