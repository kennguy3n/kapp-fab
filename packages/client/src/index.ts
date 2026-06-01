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

// Phase N8b — tenant-authored (low-code) KType. Mirrors
// internal/ktype.TenantKType. `schema` is the same KTypeSchema
// shape the platform uses, but restricted at the API layer to
// the safe field-type subset (no posting hooks, no computed
// fields, no custom agent tools).
export type TenantKTypeStatus = "draft" | "active" | "archived";

export interface TenantKType {
  tenant_id: string;
  name: string;
  version: number;
  title: string;
  description: string;
  schema: KTypeSchema;
  status: TenantKTypeStatus;
  created_at: string;
  updated_at: string;
  created_by: string;
}

export interface UpsertTenantKTypeInput {
  name: string;
  version?: number;
  title: string;
  description?: string;
  schema: KTypeSchema;
  status?: TenantKTypeStatus;
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

// ApiError wraps a non-2xx response with the parsed body so callers
// can render the backend's diagnostic message verbatim. It extends
// Error so existing `err instanceof Error` / `err.message` consumers
// (every error display site in the web app today) keep working —
// they just see a richer `.message` than "409 Conflict".
export class ApiError extends Error {
  readonly status: number;
  readonly statusText: string;
  readonly body: unknown;

  constructor(status: number, statusText: string, body: unknown, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.statusText = statusText;
    this.body = body;
  }
}

// extractErrorMessage pulls a human-readable message out of a parsed
// response body. The kapp-fab backend uses two error shapes:
//
//   * http.Error() → text/plain body containing the error string
//     (used by inventory, finance, requisition, manufacturing
//     handlers — most of the API).
//   * writeJSONError() / json.NewEncoder().Encode() → application/json
//     bodies with an "error" or "message" field (used by a few
//     newer handlers).
//
// We try JSON first (covers both shapes since text bodies fail to
// parse), then fall back to the raw text. Caller layered on top of
// `${status} ${statusText}` so the UI never sees an empty message.
function extractErrorMessage(body: unknown, raw: string): string {
  if (typeof body === "object" && body !== null) {
    const obj = body as Record<string, unknown>;
    for (const key of ["error", "message", "detail"]) {
      const v = obj[key];
      if (typeof v === "string" && v.trim() !== "") return v.trim();
    }
  }
  if (typeof body === "string" && body.trim() !== "") return body.trim();
  if (raw.trim() !== "") return raw.trim();
  return "";
}

// buildApiError reads a non-OK Response once, attempts JSON parsing
// when the Content-Type indicates JSON, and falls back to the raw
// text body. Used by every fetch site in ApiClient so the entire
// SDK surface throws ApiError consistently — including the bulk
// export and blob (PDF/HTML) helpers that bypass `request<T>()`.
// The backend writes "ordered requisitions cannot be cancelled
// (cancel the PO instead)" and similar actionable diagnostics; those
// must reach the UI instead of being swallowed into a generic
// "409 Conflict" string.
async function buildApiError(res: Response): Promise<ApiError> {
  const raw = await res.text().catch(() => "");
  let parsed: unknown = raw;
  const contentType = res.headers.get("content-type") ?? "";
  if (raw !== "" && contentType.includes("application/json")) {
    try {
      parsed = JSON.parse(raw);
    } catch {
      // Leave parsed as the raw text — extractErrorMessage
      // handles either shape.
    }
  }
  const detail = extractErrorMessage(parsed, raw);
  const message = detail
    ? `${res.status} ${res.statusText}: ${detail}`
    : `${res.status} ${res.statusText}`;
  return new ApiError(res.status, res.statusText, parsed, message);
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
      throw await buildApiError(res);
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

  // --- Phase N8b: tenant-authored (low-code) KTypes ---------------------

  listTenantKTypes(): Promise<{
    items: TenantKType[];
    field_limit: number;
  }> {
    return this.request("/tenant-ktypes");
  }

  getTenantKType(name: string, version?: number): Promise<TenantKType> {
    const qs = version ? `?version=${version}` : "";
    return this.request(`/tenant-ktypes/${encodeURIComponent(name)}${qs}`);
  }

  upsertTenantKType(input: UpsertTenantKTypeInput): Promise<TenantKType> {
    return this.request("/tenant-ktypes", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  setTenantKTypeStatus(
    name: string,
    version: number,
    status: TenantKTypeStatus,
  ): Promise<{ name: string; version: number; status: TenantKTypeStatus }> {
    return this.request(
      `/tenant-ktypes/${encodeURIComponent(name)}/status?version=${version}`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({ status }),
      },
    );
  }

  // --- KRecord CRUD ------------------------------------------------------
  listRecords(ktype: string): Promise<KRecord[]> {
    return this.request(`/records/${encodeURIComponent(ktype)}`);
  }

  /** Finalize a sales.pos_invoice. Reuses the standard
   *  Idempotency-Key middleware so an offline-queue replay of the
   *  same call (matching idempotency_key) returns the prior
   *  pos_invoice unchanged instead of double-posting. */
  finalizePOSInvoice(id: string, idempotencyKey?: string): Promise<KRecord> {
    return this.request(`/pos/invoices/${encodeURIComponent(id)}/finalize`, {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey ?? crypto.randomUUID() },
    });
  }

  /** Drive a sales.return through its state machine. Mirrors the
   *  POS finalize idempotency contract: every transition rides the
   *  Idempotency-Key middleware so a flaky-network retry collapses
   *  to the same server-side outcome. The four supported verbs
   *  match the workflow declared in internal/sales/returns.go:
   *  - approve: requested → approved (no posting side-effects)
   *  - receive: approved → received (posts inventory receipt moves)
   *  - refund:  received → refunded (posts credit-note JE)
   *  - cancel:  pre-refund → cancelled. Pure status flip when
   *    cancelling from "requested" or "approved"; when cancelling
   *    from "received" the backend ReturnPoster.Cancel additionally
   *    posts contra inventory moves to reverse the earlier receipt
   *    (see internal/sales/returns_poster.go Cancel for details). */
  runSalesReturnTransition(
    id: string,
    verb: "approve" | "receive" | "refund" | "cancel",
    idempotencyKey?: string,
  ): Promise<KRecord> {
    return this.request(
      `/sales/returns/${encodeURIComponent(id)}/${encodeURIComponent(verb)}`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey ?? crypto.randomUUID() },
      },
    );
  }

  /** Run a lifecycle transition on a procurement.purchase_requisition.
   *  Verbs match the RequisitionPoster state machine (approve →
   *  status flip; convert → allocates procurement.purchase_order;
   *  cancel → status flip with no side-effects). Each call reuses
   *  the standard Idempotency-Key middleware so a retried convert
   *  reuses the prior PO instead of spawning a duplicate. */
  runRequisitionTransition(
    id: string,
    verb: "approve" | "convert" | "cancel",
    idempotencyKey?: string,
  ): Promise<KRecord> {
    return this.request(
      `/procurement/requisitions/${encodeURIComponent(id)}/${encodeURIComponent(verb)}`,
      {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey ?? crypto.randomUUID() },
      },
    );
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
      throw await buildApiError(res);
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
      throw await buildApiError(res);
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
    account_code?: string;
  }): Promise<JournalEntry[]> {
    const qs = new URLSearchParams();
    if (params?.from) qs.set("from", params.from);
    if (params?.to) qs.set("to", params.to);
    if (params?.source_ktype) qs.set("source_ktype", params.source_ktype);
    if (params?.source_id) qs.set("source_id", params.source_id);
    if (params?.account_code) qs.set("account_code", params.account_code);
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

  // --- Manufacturing (Phase N6) ----------------------------------------

  listBOMs(status?: string): Promise<BOM[]> {
    const qs = status ? `?status=${encodeURIComponent(status)}` : "";
    return this.request(`/manufacturing/boms${qs}`);
  }

  getBOM(id: string): Promise<BOM> {
    return this.request(`/manufacturing/boms/${encodeURIComponent(id)}`);
  }

  createBOM(input: CreateBOMInput): Promise<BOM> {
    return this.request("/manufacturing/boms", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  setBOMStatus(id: string, status: string): Promise<BOM> {
    return this.request(`/manufacturing/boms/${encodeURIComponent(id)}/status`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify({ status }),
    });
  }

  listWorkOrders(status?: string): Promise<WorkOrder[]> {
    const qs = status ? `?status=${encodeURIComponent(status)}` : "";
    return this.request(`/manufacturing/work-orders${qs}`);
  }

  getWorkOrder(id: string): Promise<WorkOrder> {
    return this.request(`/manufacturing/work-orders/${encodeURIComponent(id)}`);
  }

  createWorkOrder(input: CreateWorkOrderInput): Promise<WorkOrder> {
    return this.request("/manufacturing/work-orders", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  releaseWorkOrder(id: string): Promise<WorkOrder> {
    return this.request(`/manufacturing/work-orders/${encodeURIComponent(id)}/release`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  startWorkOrder(id: string): Promise<WorkOrder> {
    return this.request(`/manufacturing/work-orders/${encodeURIComponent(id)}/start`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  completeWorkOrder(id: string, actualQty?: string): Promise<WorkOrder> {
    return this.request(`/manufacturing/work-orders/${encodeURIComponent(id)}/complete`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: actualQty ? JSON.stringify({ actual_qty: actualQty }) : undefined,
    });
  }

  cancelWorkOrder(id: string): Promise<WorkOrder> {
    return this.request(`/manufacturing/work-orders/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
  }

  closeWorkOrder(id: string): Promise<WorkOrder> {
    return this.request(`/manufacturing/work-orders/${encodeURIComponent(id)}/close`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
    });
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

  // --- Phase N9c: landed cost vouchers ----------------------------------

  /** List landed cost vouchers, optionally filtered by status. */
  listLandedCostVouchers(params?: {
    status?: string;
  }): Promise<LandedCostVoucher[]> {
    const qs = new URLSearchParams();
    if (params?.status) qs.set("status", params.status);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return this.request(`/finance/landed-costs${suffix}`);
  }

  createLandedCostVoucher(
    input: UpsertLandedCostVoucherInput,
  ): Promise<LandedCostVoucher> {
    return this.request("/finance/landed-costs", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  getLandedCostVoucher(id: string): Promise<LandedCostVoucherWithLines> {
    return this.request(`/finance/landed-costs/${encodeURIComponent(id)}`);
  }

  updateLandedCostVoucher(
    id: string,
    input: UpsertLandedCostVoucherInput,
  ): Promise<LandedCostVoucher> {
    return this.request(`/finance/landed-costs/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteLandedCostVoucher(id: string): Promise<void> {
    return this.request(`/finance/landed-costs/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  upsertLandedCostCharge(
    voucherId: string,
    input: UpsertLandedCostChargeInput,
  ): Promise<LandedCostCharge> {
    return this.request(
      `/finance/landed-costs/${encodeURIComponent(voucherId)}/charges`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      },
    );
  }

  deleteLandedCostCharge(voucherId: string, chargeId: string): Promise<void> {
    return this.request(
      `/finance/landed-costs/${encodeURIComponent(voucherId)}/charges/${encodeURIComponent(chargeId)}`,
      { method: "DELETE" },
    );
  }

  upsertLandedCostTarget(
    voucherId: string,
    input: UpsertLandedCostTargetInput,
  ): Promise<LandedCostTarget> {
    return this.request(
      `/finance/landed-costs/${encodeURIComponent(voucherId)}/targets`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      },
    );
  }

  deleteLandedCostTarget(voucherId: string, targetId: string): Promise<void> {
    return this.request(
      `/finance/landed-costs/${encodeURIComponent(voucherId)}/targets/${encodeURIComponent(targetId)}`,
      { method: "DELETE" },
    );
  }

  allocateLandedCostVoucher(id: string): Promise<LandedCostTarget[]> {
    return this.request(
      `/finance/landed-costs/${encodeURIComponent(id)}/allocate`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      },
    );
  }

  postLandedCostVoucher(id: string): Promise<LandedCostPostResult> {
    return this.request(
      `/finance/landed-costs/${encodeURIComponent(id)}/post`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({}),
      },
    );
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

  // --- Phase N5: budgets ------------------------------------------------

  listBudgets(): Promise<Budget[]> {
    return this.request(`/finance/budgets`);
  }

  getBudget(id: string): Promise<Budget> {
    return this.request(`/finance/budgets/${encodeURIComponent(id)}`);
  }

  createBudget(input: CreateBudgetInput): Promise<Budget> {
    return this.request(`/finance/budgets`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateBudget(id: string, input: UpdateBudgetInput): Promise<Budget> {
    return this.request(`/finance/budgets/${encodeURIComponent(id)}`, {
      method: "PUT",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  deleteBudget(id: string): Promise<void> {
    return this.request(`/finance/budgets/${encodeURIComponent(id)}`, {
      method: "DELETE",
    });
  }

  listBudgetLines(budgetId: string): Promise<BudgetLine[]> {
    return this.request(
      `/finance/budgets/${encodeURIComponent(budgetId)}/lines`
    );
  }

  upsertBudgetLine(
    budgetId: string,
    input: BudgetLineInput
  ): Promise<BudgetLine> {
    return this.request(
      `/finance/budgets/${encodeURIComponent(budgetId)}/lines`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }

  deleteBudgetLine(budgetId: string, lineId: string): Promise<void> {
    return this.request(
      `/finance/budgets/${encodeURIComponent(budgetId)}/lines/${encodeURIComponent(lineId)}`,
      { method: "DELETE" }
    );
  }

  budgetVariance(
    budgetId: string,
    params?: { from?: string; to?: string }
  ): Promise<BudgetVarianceReport> {
    const qs = new URLSearchParams();
    if (params?.from) qs.set("from", params.from);
    if (params?.to) qs.set("to", params.to);
    const suffix = qs.toString() ? `?${qs.toString()}` : "";
    return this.request(
      `/finance/budgets/${encodeURIComponent(budgetId)}/variance${suffix}`
    );
  }

  // --- Phase N9d: cycle counts ------------------------------------------

  /** List cycle-count sessions, optionally filtered by status. */
  listCycleCountSessions(filter?: {
    status?: string;
    warehouse_id?: string;
  }): Promise<CycleCountSession[]> {
    const qs = new URLSearchParams();
    if (filter?.status) qs.set("status", filter.status);
    if (filter?.warehouse_id) qs.set("warehouse_id", filter.warehouse_id);
    const query = qs.toString();
    return this.request(
      `/inventory/cycle-counts${query ? `?${query}` : ""}`
    );
  }

  /** Open a new draft cycle-count session. */
  createCycleCountSession(input: {
    code: string;
    description?: string;
    warehouse_id: string;
  }): Promise<CycleCountSession> {
    return this.request("/inventory/cycle-counts", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  /** Fetch a session with its lines. */
  getCycleCountSession(id: string): Promise<CycleCountSessionWithLines> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(id)}`
    );
  }

  /** Patch metadata + advance status. */
  updateCycleCountSession(
    id: string,
    input: {
      code: string;
      description?: string;
      warehouse_id: string;
      status?: string;
    }
  ): Promise<CycleCountSession> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(id)}`,
      {
        method: "PUT",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }

  /** Delete a draft cycle-count session. */
  deleteCycleCountSession(id: string): Promise<void> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(id)}`,
      { method: "DELETE" }
    );
  }

  /** Seed (or refresh) expected_qty from the stock_levels view. */
  seedCycleCountSession(id: string): Promise<CycleCountLine[]> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(id)}/seed`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  /** Insert or update a count line. */
  upsertCycleCountLine(
    sessionId: string,
    input: {
      id?: string;
      item_id: string;
      expected_qty: string;
      counted_qty: string;
      notes?: string;
    }
  ): Promise<CycleCountLine> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(sessionId)}/lines`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(input),
      }
    );
  }

  /** Remove a count line. */
  deleteCycleCountLine(sessionId: string, lineId: string): Promise<void> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(
        sessionId
      )}/lines/${encodeURIComponent(lineId)}`,
      { method: "DELETE" }
    );
  }

  /** Post the session: writes variance inventory_moves. Idempotent. */
  postCycleCountSession(id: string): Promise<CycleCountSession> {
    return this.request(
      `/inventory/cycle-counts/${encodeURIComponent(id)}/post`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  // --- Phase 2a B5 — marketplace tenant surface ---------------------
  // These wrap the routes mounted under /api/v1/marketplace in
  // services/api/routes.go (tenantChain). The wire shapes mirror
  // listExtensionsResponse / installResponse / upgradeResponse in
  // services/api/marketplace_handlers.go and are kept in lock-step
  // with the Go DTOs — when a field is added there, add it here.

  listMarketplaceExtensions(
    opts: MarketplaceListExtensionsOptions = {}
  ): Promise<MarketplaceListExtensionsResponse> {
    const params = new URLSearchParams();
    if (opts.publisher) params.set("publisher", opts.publisher);
    if (opts.q) params.set("q", opts.q);
    // `!= null` covers both undefined (param omitted) and null
    // (explicit null). Critically, this does NOT short-circuit
    // on limit=0 the way `if (opts.limit)` would. Today the
    // server clamps limit<=0 to 500 server-side, but the wire
    // contract is "if the caller sent a number, forward it" —
    // a future server change to treat 0 as "no rows" must not
    // be silently subverted by a falsy check on the client.
    if (opts.limit != null) params.set("limit", String(opts.limit));
    const qs = params.toString();
    return this.request(`/marketplace/extensions${qs ? `?${qs}` : ""}`);
  }

  getMarketplaceExtension(
    extId: string
  ): Promise<MarketplaceGetExtensionResponse> {
    return this.request(
      `/marketplace/extensions/${encodeURIComponent(extId)}`
    );
  }

  listMarketplaceVersions(
    extId: string
  ): Promise<MarketplaceListVersionsResponse> {
    return this.request(
      `/marketplace/extensions/${encodeURIComponent(extId)}/versions`
    );
  }

  listMarketplaceInstallations(): Promise<MarketplaceListInstallationsResponse> {
    return this.request(`/marketplace/installations`);
  }

  getMarketplaceInstallation(installId: string): Promise<MarketplaceInstallation> {
    return this.request(
      `/marketplace/installations/${encodeURIComponent(installId)}`
    );
  }

  installMarketplaceExtension(
    input: InstallMarketplaceExtensionInput
  ): Promise<InstallMarketplaceExtensionResponse> {
    return this.request(`/marketplace/installations`, {
      method: "POST",
      // Idempotency-Key matches IdempotencyMiddleware wired on the
      // install/upgrade/uninstall/updateSettings group — a retried
      // POST after a network glitch must NOT double-dispatch the
      // pre_install hook or create a second install row.
      headers: { "Idempotency-Key": crypto.randomUUID() },
      body: JSON.stringify(input),
    });
  }

  updateMarketplaceInstallationSettings(
    installId: string,
    settings: Record<string, unknown>
  ): Promise<MarketplaceUpdateSettingsResponse> {
    return this.request(
      `/marketplace/installations/${encodeURIComponent(installId)}/settings`,
      {
        method: "PATCH",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({ settings }),
      }
    );
  }

  upgradeMarketplaceInstallation(
    installId: string,
    input: UpgradeMarketplaceInstallationInput
  ): Promise<UpgradeMarketplaceInstallationResponse> {
    // settings is the optional caller-supplied migrated document.
    // When the caller wants to preserve the existing document, pass
    // `keep_settings: true` (or omit settings entirely) — the engine
    // reads the row under FOR UPDATE and writes the same document
    // back. The wire contract is verbatim from upgradeRequestBody in
    // marketplace_handlers.go.
    const body: Record<string, unknown> = {
      from_version_id: input.from_version_id,
      to_version_id: input.to_version_id,
    };
    if (input.keep_settings) body.keep_settings = true;
    if (input.settings !== undefined) body.settings = input.settings;
    return this.request(
      `/marketplace/installations/${encodeURIComponent(installId)}/upgrade`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify(body),
      }
    );
  }

  uninstallMarketplaceExtension(installId: string): Promise<void> {
    return this.request(
      `/marketplace/installations/${encodeURIComponent(installId)}`,
      {
        method: "DELETE",
        headers: { "Idempotency-Key": crypto.randomUUID() },
      }
    );
  }

  getMarketplacePublisher(slug: string): Promise<MarketplacePublisherPublic> {
    return this.request(`/marketplace/publishers/${encodeURIComponent(slug)}`);
  }
}

// --- Phase N9d: cycle counts -----------------------------------------

export interface CycleCountSession {
  tenant_id: string;
  id: string;
  code: string;
  description?: string;
  warehouse_id: string;
  status: "draft" | "counting" | "reconciled" | "posted";
  created_by: string;
  created_at: string;
  updated_at: string;
  posted_at?: string | null;
}

export interface CycleCountLine {
  tenant_id: string;
  id: string;
  session_id: string;
  item_id: string;
  expected_qty: string;
  counted_qty: string;
  variance: string;
  notes?: string;
  created_at: string;
  updated_at: string;
}

export interface CycleCountSessionWithLines {
  session: CycleCountSession;
  lines: CycleCountLine[];
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

export interface BOM {
  tenant_id: string;
  id: string;
  item_id: string;
  version: string;
  status: "draft" | "active" | "obsolete";
  output_qty: string;
  uom: string;
  notes?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
  components?: BOMComponent[];
}

export interface BOMComponent {
  bom_id: string;
  component_item_id: string;
  qty: string;
  uom: string;
  scrap_percent?: string | null;
  sort_order: number;
}

export interface CreateBOMInput {
  item_id: string;
  version: string;
  output_qty: string;
  uom: string;
  notes?: string;
  // Component ordering is implicit in array position — the server
  // assigns sort_order = (index + 1) on insert, so this shape
  // intentionally does NOT accept a sort_order field on the request
  // (it would be silently ignored). The response shape `BOMComponent`
  // still exposes sort_order for callers that need to render the
  // server-assigned order.
  components: Array<{
    component_item_id: string;
    qty: string;
    uom: string;
    scrap_percent?: string;
  }>;
  activate?: boolean;
}

export interface WorkOrder {
  tenant_id: string;
  id: string;
  item_id: string;
  bom_id?: string | null;
  warehouse_id: string;
  planned_qty: string;
  actual_qty?: string | null;
  status: "draft" | "released" | "in_progress" | "completed" | "closed" | "cancelled";
  scheduled_start?: string | null;
  scheduled_end?: string | null;
  started_at?: string | null;
  completed_at?: string | null;
  notes?: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface CreateWorkOrderInput {
  item_id: string;
  warehouse_id: string;
  planned_qty: string;
  scheduled_start?: string;
  scheduled_end?: string;
  notes?: string;
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

// --- Phase N5: budgets -----------------------------------------------

export interface Budget {
  tenant_id: string;
  id: string;
  name: string;
  fiscal_year: number;
  status: "draft" | "active" | "closed";
  cost_center?: string;
  notes?: string;
  variance_threshold?: string | null;
  created_by?: string | null;
  created_at: string;
  updated_at: string;
}

export interface CreateBudgetInput {
  name: string;
  fiscal_year: number;
  status?: "draft" | "active" | "closed";
  cost_center?: string;
  notes?: string;
  variance_threshold?: string;
}

export interface UpdateBudgetInput {
  name: string;
  status: "draft" | "active" | "closed";
  cost_center?: string;
  notes?: string;
  variance_threshold?: string;
}

export interface BudgetLine {
  tenant_id: string;
  id: string;
  budget_id: string;
  account_code: string;
  cost_center?: string;
  // 12-element array, January..December in fiscal-month order.
  months: string[];
  annual_total: string;
  created_at: string;
  updated_at: string;
}

export interface BudgetLineInput {
  id?: string;
  account_code: string;
  cost_center?: string;
  // 12-element array, January..December in fiscal-month order.
  months: string[];
}

/**
 * Chart-of-accounts classification of the variance row's account.
 * Mirrors the `account_type` column on `accounts` and the backend's
 * `VarianceRow.account_type` JSON field. Renderers use this to pick
 * "exceeded plan = good / bad" colour semantics:
 *
 *   - debit-normal (asset / expense): positive variance means
 *     actual exceeded plan, which is typically *bad* (overspend).
 *   - credit-normal (liability / equity / revenue): positive
 *     variance means actual exceeded plan, which is typically *good*
 *     (over-earning revenue, over-collecting AP/equity).
 *
 * The backend has already sign-normalised the Actual / Variance
 * amounts so that positive = exceeded plan for every account type;
 * the client uses `account_type` only to decide colour, not to
 * re-derive the sign.
 */
export type BudgetVarianceAccountType =
  | "asset"
  | "liability"
  | "equity"
  | "revenue"
  | "expense"
  | "";

export interface BudgetVarianceRow {
  budget_id: string;
  account_code: string;
  /** Optional — the backend joins the chart of accounts so the
   * row can render "4000 — Sales Revenue" instead of an opaque
   * code. Empty when the account_code is unknown. */
  account_name?: string;
  /** Optional — only emitted by the backend when the account
   * resolves to a known account_type at report time. */
  account_type?: BudgetVarianceAccountType;
  cost_center?: string;
  period: string;
  budgeted: string;
  actual: string;
  variance: string;
  variance_pct: string;
  /** Better-than-plan flag the backend stamps per row.
   *  Revenue over-perform and expense under-spend are
   *  favourable; expense over-spend and revenue under-perform
   *  are unfavourable. The footer rollups
   *  total_favourable_variance / total_unfavourable_variance
   *  bucket the gross variance using this flag. */
  favourable: boolean;
  /** Unplanned activity: budgeted is zero but actual is not.
   *  The backend forces variance_pct=0 for these rows to avoid
   *  div-by-zero, so renderers should display "—" (rather than
   *  "0%") and the variance alerter ALWAYS notifies on these
   *  rows regardless of the configured threshold. Common cause:
   *  spend booked against an account that has no plan in this
   *  budget, or revenue recognised on a previously-unplanned
   *  line. */
  unplanned: boolean;
}

export interface BudgetVarianceReport {
  tenant_id: string;
  budget_id: string;
  budget_name: string;
  fiscal_year: number;
  from: string;
  to: string;
  rows: BudgetVarianceRow[];
  total_budgeted: string;
  total_actual: string;
  total_variance: string;
  /** Sum of |variance| across rows where favourable=true.
   *  Always non-negative. Surface this on the footer instead of
   *  total_variance for at-a-glance red/green colouring. */
  total_favourable_variance: string;
  /** Sum of |variance| across rows where favourable=false.
   *  Always non-negative. */
  total_unfavourable_variance: string;
}

// --- Phase N9c: landed cost vouchers ---------------------------------------

/**
 * LandedCostVoucher is the header row for a landed-cost voucher.
 * Mirrors internal/finance.LandedCostVoucher; the lifecycle is
 * draft → allocated → posted.
 */
export interface LandedCostVoucher {
  tenant_id: string;
  id: string;
  voucher_number: string;
  description?: string;
  status: "draft" | "allocated" | "posted";
  allocation_method: "by_qty" | "by_amount" | "by_weight";
  posted_at?: string | null;
  je_id?: string | null;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface LandedCostCharge {
  tenant_id: string;
  id: string;
  voucher_id: string;
  description: string;
  amount: string;
  account_code?: string;
  created_at: string;
  updated_at: string;
}

export interface LandedCostTarget {
  tenant_id: string;
  id: string;
  voucher_id: string;
  source_ktype: string;
  source_id: string;
  item_id: string;
  warehouse_id: string;
  qty: string;
  unit_cost: string;
  amount: string;
  weight: string;
  allocated_amount: string;
  applied: boolean;
  created_at: string;
  updated_at: string;
}

export interface LandedCostVoucherWithLines {
  voucher: LandedCostVoucher;
  charges: LandedCostCharge[];
  targets: LandedCostTarget[];
}

export interface UpsertLandedCostVoucherInput {
  voucher_number: string;
  description?: string;
  allocation_method?: "by_qty" | "by_amount" | "by_weight";
}

export interface UpsertLandedCostChargeInput {
  id?: string;
  description: string;
  amount: string | number;
  account_code?: string;
}

export interface UpsertLandedCostTargetInput {
  id?: string;
  source_ktype?: string;
  source_id: string;
  item_id: string;
  warehouse_id: string;
  qty: string | number;
  unit_cost: string | number;
  weight?: string | number;
}

export interface LandedCostPostResult {
  voucher: LandedCostVoucher;
  journal_entry: {
    id: string;
    posted_at: string;
  };
}

// --- Phase 2a B5 — marketplace tenant types -------------------------
// Wire shapes that mirror the Go marketplace package + the handler
// DTOs in services/api/marketplace_handlers.go. Kept flat (no nested
// `signature` field) for the same reason the Go-side ExtensionVersion
// struct keeps the three signature columns at the top level — JSON
// consumers iterate fields by name.

export type ExtensionStatus =
  | "unpublished"
  | "listed"
  | "deprecated"
  | "removed";

export type InstallStatus =
  | "pending"
  | "installing"
  | "active"
  | "disabled"
  | "failed"
  | "uninstalled";

export interface MarketplaceExtension {
  id: string;
  name: string;
  publisher: string;
  slug: string;
  display_name: string;
  description: string;
  author: string;
  license: string;
  homepage?: string;
  support_email?: string;
  icon_url?: string;
  status: ExtensionStatus;
  listed_version?: string;
  created_at: string;
  updated_at: string;
}

export interface MarketplaceExtensionVersion {
  id: string;
  extension_id: string;
  version: string;
  bundle_hash: string;
  bundle_size_bytes: number;
  bundle_url: string;
  min_kapp_version: string;
  max_kapp_version?: string;
  features_required: string[];
  permissions_required: string[];
  ktypes_count: number;
  workflows_count: number;
  agent_tools_count: number;
  ui_extensions_count: number;
  webhooks_count: number;
  yanked: boolean;
  yanked_reason?: string;
  published_at: string;
  bundle_signature?: string;
  bundle_signature_key_id?: string;
  signed_at?: string;
}

// MarketplaceInstallation mirrors services/api/marketplace_handlers.go
// installationView. Settings is the parsed object (never raw bytes).
export interface MarketplaceInstallation {
  id: string;
  tenant_id: string;
  extension_id: string;
  extension_version_id: string;
  status: InstallStatus;
  settings: Record<string, unknown>;
  webhook_base: string;
  installed_by?: string;
  installed_at: string;
  updated_at: string;
  last_health_check_at?: string;
  last_health_check_status?: string;
  failure_reason?: string;
}

export interface MarketplacePublisherPublic {
  id: string;
  slug: string;
  display_name: string;
  verified: boolean;
  has_keys: boolean;
}

export interface MarketplaceListExtensionsOptions {
  publisher?: string;
  q?: string;
  limit?: number;
}

export interface MarketplaceListExtensionsResponse {
  items: MarketplaceExtension[];
}

export interface MarketplaceGetExtensionResponse {
  extension: MarketplaceExtension;
  versions: MarketplaceExtensionVersion[];
}

export interface MarketplaceListVersionsResponse {
  items: MarketplaceExtensionVersion[];
}

export interface MarketplaceListInstallationsResponse {
  items: MarketplaceInstallation[];
}

export interface InstallMarketplaceExtensionInput {
  extension_id: string;
  version_id: string;
  webhook_base: string;
  settings?: Record<string, unknown>;
}

export interface InstallMarketplaceExtensionResponse {
  installation: MarketplaceInstallation;
  signing_secret: string;
}

export interface UpgradeMarketplaceInstallationInput {
  from_version_id: string;
  to_version_id: string;
  // Mutually exclusive with keep_settings — when present, replaces
  // the persisted document with this caller-migrated value.
  settings?: Record<string, unknown>;
  // Explicit "preserve existing" signal. Equivalent to omitting both
  // settings and keep_settings; surfaced so a caller can be
  // unambiguous about intent vs. "settings: {}" (which WIPES).
  keep_settings?: boolean;
}

export interface UpgradeMarketplaceInstallationResponse {
  installation: MarketplaceInstallation;
  from_version_id: string;
}

export interface MarketplaceUpdateSettingsResponse {
  installation: MarketplaceInstallation;
}
