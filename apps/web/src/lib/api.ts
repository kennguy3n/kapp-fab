import { ApiClient, type KRecord, type KType } from "@kapp/client";

const tenantId = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

const token = (): string | null => localStorage.getItem("kapp.token");

// VITE_DEMO_MODE swaps the live ApiClient for an in-memory fixture
// shim so the UI can render populated screens without a backend
// (used by the screenshot capture script under scripts/).
//
// Vite statically replaces `import.meta.env.VITE_DEMO_MODE` at build
// time, so when it's unset the constant below becomes literal `false`
// and the `if (demoMode)` branch (plus the dynamic `import("./mock-api")`
// inside it) becomes unreachable. Rollup then drops the mock-api/
// mock-data chunks entirely from production bundles.
const demoMode = import.meta.env.VITE_DEMO_MODE === "true";

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

// Demo mode primes localStorage synchronously here (so React render
// passes the tenant guard immediately) and then asynchronously swaps
// in the mock client + portal fetch interceptor once the dynamic
// import resolves. Inlining the localStorage seed avoids needing
// top-level await — the `mock-api` module evaluation can land a tick
// later because the screenshot script also waits for network idle
// before asserting.
let resolvedApi: ApiClient = realApi;
let apiReady: Promise<ApiClient> = Promise.resolve(realApi);

if (demoMode && typeof window !== "undefined") {
  // Demo tenant id — kept in sync with mock-data.ts DEMO_TENANT_ID.
  if (!localStorage.getItem("kapp.tenant")) {
    localStorage.setItem("kapp.tenant", "00000000-0000-0000-0000-000000000001");
  }
  if (!localStorage.getItem("kapp.token")) {
    localStorage.setItem(
      "kapp.token",
      // Decoy JWT — three base64 segments so any code that splits
      // on "." gets three pieces. Not a real signed token.
      "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJkZW1vQGFjbWUuZXhhbXBsZSIsInRlbmFudF9pZCI6Ijk5OTk5OTk5LTk5OTktOTk5OS05OTk5LTk5OTk5OTk5OTk5OSJ9.demo-signature",
    );
  }

  apiReady = import("./mock-api").then((mod) => {
    mod.installPortalDemoFetch();
    resolvedApi = mod.mockApi;
    return mod.mockApi;
  });
}

// Lazy proxy: until the dynamic import resolves, calls fall through
// to a Promise.then chain that awaits the mock client. After resolve
// (or in non-demo builds), method access is direct.
//
// "then"/"catch"/"finally" are filtered out so this proxy never
// masquerades as a thenable — otherwise any code that resolves a
// Promise with `api` would call our stub `.then(resolve, reject)` and
// hang forever waiting for it to fulfil the resolution.
const API_THENABLE_GUARD = new Set(["then", "catch", "finally"]);

export const api: ApiClient = new Proxy({} as ApiClient, {
  get(_target, prop: string | symbol) {
    if (typeof prop !== "string" && typeof prop !== "number") return undefined;
    const key = String(prop);
    if (API_THENABLE_GUARD.has(key)) return undefined;
    if (resolvedApi !== realApi || !demoMode) {
      return (resolvedApi as unknown as Record<string, unknown>)[key];
    }
    return (...args: unknown[]) =>
      apiReady.then((a) =>
        (a as unknown as Record<string, (...a: unknown[]) => unknown>)[key](
          ...args,
        ),
      );
  },
});

export type { KRecord, KType };
