import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  PORTAL_TOKEN_KEY,
  portalApi,
  type PortalAuthResponse,
} from "./portalApi";

// portalApi is the customer-portal HTTP surface. Two contracts are
// security-relevant and pinned here:
//   1. It authenticates with the portal-scoped token under its own
//      localStorage key — NEVER the AppShell's kapp.token.
//   2. It must NOT send the X-Tenant-ID header (the portal JWT carries
//      the tenant claim; sending a client-controlled tenant id would
//      be a cross-tenant escalation vector for 5000 SME tenants).
// fetch is stubbed (bypassing MSW) so each call's URL, method, headers
// and body can be asserted exactly.

interface FetchCall {
  url: string;
  init: RequestInit;
}

let calls: FetchCall[];

function stubFetch(response: {
  ok?: boolean;
  status?: number;
  json?: unknown;
  text?: string;
}) {
  const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init: init ?? {} });
    return {
      ok: response.ok ?? true,
      status: response.status ?? 200,
      json: async () => response.json ?? {},
      text: async () => response.text ?? "",
    } as Response;
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function headersOf(init: RequestInit): Record<string, string> {
  return (init.headers as Record<string, string>) ?? {};
}

beforeEach(() => {
  calls = [];
  window.localStorage.clear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("portalApi authentication headers", () => {
  it("attaches the portal-scoped bearer token from its own storage key", async () => {
    window.localStorage.setItem(PORTAL_TOKEN_KEY, "portal-jwt-123");
    stubFetch({ json: { tickets: [] } });
    await portalApi.listTickets();
    const headers = headersOf(calls[0].init);
    expect(headers.Authorization).toBe("Bearer portal-jwt-123");
  });

  it("omits Authorization when no portal token is stored", async () => {
    stubFetch({});
    await portalApi.requestLink("acme", "user@example.com");
    expect(headersOf(calls[0].init).Authorization).toBeUndefined();
  });

  it("never sends an X-Tenant-ID header (tenant is in the portal JWT)", async () => {
    window.localStorage.setItem(PORTAL_TOKEN_KEY, "portal-jwt-123");
    // Even if the AppShell tenant key is set, the portal surface must
    // not leak it into the request.
    window.localStorage.setItem("kapp.tenant", "some-other-tenant");
    stubFetch({ json: { tickets: [] } });
    await portalApi.listTickets();
    const headers = headersOf(calls[0].init);
    expect(headers["X-Tenant-ID"]).toBeUndefined();
    expect(headers["X-Tenant-Id"]).toBeUndefined();
  });
});

describe("portalApi request shapes", () => {
  it("posts the magic-link request to the auth endpoint", async () => {
    stubFetch({});
    await portalApi.requestLink("acme", "user@example.com");
    expect(calls[0].url).toBe("/api/v1/portal/auth/request");
    expect(calls[0].init.method).toBe("POST");
    expect(JSON.parse(calls[0].init.body as string)).toEqual({
      tenant_slug: "acme",
      email: "user@example.com",
    });
  });

  it("returns the parsed auth payload from verify", async () => {
    const payload: PortalAuthResponse = {
      token: "t",
      expires_at: 123,
      user: {
        id: "u1",
        tenant_id: "tn1",
        email: "u@e.com",
        display_name: "U",
      },
    };
    stubFetch({ json: payload });
    const res = await portalApi.verifyLink("acme", "u@e.com", "code");
    expect(res).toEqual(payload);
  });

  it("URL-encodes path parameters to avoid path injection", async () => {
    window.localStorage.setItem(PORTAL_TOKEN_KEY, "t");
    stubFetch({ json: {} });
    await portalApi.getTicket("a/b ?x");
    expect(calls[0].url).toBe("/api/v1/portal/tickets/a%2Fb%20%3Fx");
  });
});

describe("portalApi error / empty-body handling", () => {
  it("throws with status + body text on a non-2xx response", async () => {
    stubFetch({ ok: false, status: 403, text: "forbidden" });
    await expect(portalApi.listTickets()).rejects.toThrow("403: forbidden");
  });

  it("resolves to undefined on a 204 No Content reply", async () => {
    stubFetch({ ok: true, status: 204 });
    await expect(portalApi.requestLink("acme", "u@e.com")).resolves.toBeUndefined();
  });
});
