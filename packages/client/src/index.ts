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
