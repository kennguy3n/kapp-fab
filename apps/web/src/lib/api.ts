import { ApiClient, type KRecord, type KType } from "@kapp/client";
import { installDemoLocalStorage, installPortalDemoFetch, mockApi } from "./mock-api";

const tenantId = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

const token = (): string | null => localStorage.getItem("kapp.token");

// VITE_DEMO_MODE swaps the live ApiClient for an in-memory fixture
// shim so the UI can render populated screens without a backend
// (used by the screenshot capture script under scripts/).
const demoMode =
  typeof import.meta !== "undefined" &&
  (import.meta as { env?: Record<string, string | undefined> }).env
    ?.VITE_DEMO_MODE === "true";

if (demoMode) {
  installDemoLocalStorage();
  installPortalDemoFetch();
}

const realApi = new ApiClient({
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

export const api: ApiClient = demoMode ? mockApi : realApi;

export type { KRecord, KType };
