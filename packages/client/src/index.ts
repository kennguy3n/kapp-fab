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
