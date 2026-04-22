import { ApiClient, type KRecord, type KType } from "@kapp/client";

const tenantId = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

const token = (): string | null => localStorage.getItem("kapp.token");

export const api = new ApiClient({
  baseUrl: "/api/v1",
  headers: () => {
    const h: Record<string, string> = {
      "X-Tenant-ID": tenantId(),
    };
    const t = token();
    if (t) h.Authorization = `Bearer ${t}`;
    return h;
  },
});

export type { KRecord, KType };
