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

export interface KTypeSchema {
  name: string;
  version: number;
  fields: FieldSpec[];
  views?: {
    list?: { columns?: string[]; default_sort?: string };
    form?: { sections?: Array<{ title?: string; fields: string[] }> };
  };
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

  listKTypes(): Promise<KType[]> {
    return this.request("/ktypes");
  }

  getKType(name: string): Promise<KType> {
    return this.request(`/ktypes/${encodeURIComponent(name)}`);
  }

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
}
