import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { ApiClient } from "@kapp/client";

// SDK smoke-tests for the marketplace surface added in B5. These
// pin the wire shape (URL paths, methods, request bodies, idempotency
// header presence) so accidental refactors in the client surface a
// real test failure instead of a silent contract drift.

function newClient(fetchSpy: ReturnType<typeof vi.fn>): ApiClient {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (globalThis as any).fetch = fetchSpy;
  return new ApiClient({
    baseUrl: "/api/v1",
    headers: () => ({ "X-Tenant-ID": "tnt-1" }),
  });
}

function mockJSON(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("Marketplace SDK", () => {
  let originalFetch: typeof globalThis.fetch;
  beforeEach(() => {
    originalFetch = globalThis.fetch;
  });
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it("listMarketplaceExtensions builds the canonical URL + forwards q/publisher", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(mockJSON({ items: [] }));
    const api = newClient(fetchSpy);
    await api.listMarketplaceExtensions({ q: "inv", publisher: "acme" });
    const url = fetchSpy.mock.calls[0][0] as string;
    expect(url).toContain("/api/v1/marketplace/extensions?");
    expect(url).toContain("publisher=acme");
    expect(url).toContain("q=inv");
  });

  it("listMarketplaceExtensions forwards limit=0 instead of dropping it on a falsy check (ANALYSIS_0005)", async () => {
    // The SDK previously used `if (opts.limit)`, which is falsy
    // for 0 — so a caller asking for "limit=0" silently sent no
    // limit param. The server today clamps limit<=0 to 500, so
    // the misbehaviour is observable only at the wire layer
    // (URL doesn't include `limit=0`). Pin the contract: if
    // the caller passed a number, the SDK forwards it.
    const fetchSpy = vi.fn().mockResolvedValue(mockJSON({ items: [] }));
    const api = newClient(fetchSpy);
    await api.listMarketplaceExtensions({ limit: 0 });
    const url = fetchSpy.mock.calls[0][0] as string;
    expect(url).toContain("limit=0");
  });

  it("listMarketplaceExtensions omits limit param when undefined (ANALYSIS_0005 negative case)", async () => {
    // Negative case: `limit: undefined` (or simply not passed)
    // MUST NOT render a `limit=` query param. The fix uses
    // `!= null`, which treats undefined + null as "not sent".
    const fetchSpy = vi.fn().mockResolvedValue(mockJSON({ items: [] }));
    const api = newClient(fetchSpy);
    await api.listMarketplaceExtensions({});
    const url = fetchSpy.mock.calls[0][0] as string;
    expect(url).not.toContain("limit=");
  });

  it("installMarketplaceExtension POSTs with body + Idempotency-Key header", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(
      mockJSON({
        installation: {
          id: "i1",
          tenant_id: "t",
          extension_id: "e",
          extension_version_id: "v",
          status: "active",
          settings: {},
          webhook_base: "https://x",
          installed_at: "2025-01-01T00:00:00Z",
          updated_at: "2025-01-01T00:00:00Z",
        },
        signing_secret: "sec",
      }),
    );
    const api = newClient(fetchSpy);
    await api.installMarketplaceExtension({
      extension_id: "e",
      version_id: "v",
      webhook_base: "https://x",
      settings: { api_key: "k" },
    });
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe("/api/v1/marketplace/installations");
    expect(init.method).toBe("POST");
    const headers = init.headers as Record<string, string>;
    expect(headers["Idempotency-Key"]).toBeTruthy();
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body.extension_id).toBe("e");
    expect(body.version_id).toBe("v");
    expect(body.settings).toEqual({ api_key: "k" });
  });

  it("updateMarketplaceInstallationSettings PATCHes settings + Idempotency-Key", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(
      mockJSON({
        installation: {
          id: "i1",
          tenant_id: "t",
          extension_id: "e",
          extension_version_id: "v",
          status: "active",
          settings: { api_key: "new" },
          webhook_base: "https://x",
          installed_at: "2025-01-01T00:00:00Z",
          updated_at: "2025-01-01T00:00:00Z",
        },
      }),
    );
    const api = newClient(fetchSpy);
    await api.updateMarketplaceInstallationSettings("install-1", {
      api_key: "new",
    });
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe("/api/v1/marketplace/installations/install-1/settings");
    expect(init.method).toBe("PATCH");
    expect(
      (init.headers as Record<string, string>)["Idempotency-Key"],
    ).toBeTruthy();
    const body = JSON.parse(init.body as string) as { settings: unknown };
    expect(body.settings).toEqual({ api_key: "new" });
  });

  it("upgradeMarketplaceInstallation forwards from/to + keep_settings", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(
      mockJSON({
        installation: {
          id: "i1",
          tenant_id: "t",
          extension_id: "e",
          extension_version_id: "v",
          status: "active",
          settings: {},
          webhook_base: "https://x",
          installed_at: "2025-01-01T00:00:00Z",
          updated_at: "2025-01-01T00:00:00Z",
        },
        from_version_id: "v0",
      }),
    );
    const api = newClient(fetchSpy);
    await api.upgradeMarketplaceInstallation("install-1", {
      from_version_id: "v0",
      to_version_id: "v1",
      keep_settings: true,
    });
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe("/api/v1/marketplace/installations/install-1/upgrade");
    expect(init.method).toBe("POST");
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body.from_version_id).toBe("v0");
    expect(body.to_version_id).toBe("v1");
    expect(body.keep_settings).toBe(true);
    // settings is omitted when caller used keep_settings.
    expect("settings" in body).toBe(false);
  });

  it("uninstallMarketplaceExtension DELETEs with Idempotency-Key", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    const api = newClient(fetchSpy);
    await api.uninstallMarketplaceExtension("install-1");
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe("/api/v1/marketplace/installations/install-1");
    expect(init.method).toBe("DELETE");
    expect(
      (init.headers as Record<string, string>)["Idempotency-Key"],
    ).toBeTruthy();
  });

  it("listMarketplaceVersions GETs /extensions/{ext_id}/versions", async () => {
    const fetchSpy = vi.fn().mockResolvedValue(mockJSON({ items: [] }));
    const api = newClient(fetchSpy);
    await api.listMarketplaceVersions("ext-1");
    const url = fetchSpy.mock.calls[0][0];
    expect(url).toBe("/api/v1/marketplace/extensions/ext-1/versions");
  });
});
