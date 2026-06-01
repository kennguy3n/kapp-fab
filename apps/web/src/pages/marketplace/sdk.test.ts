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

  it("upgradeMarketplaceInstallation forwards keep_settings:false on the wire instead of dropping it on a falsy check (ANALYSIS_0004)", async () => {
    // ANALYSIS_0004 (round 3): the SDK previously had
    // `if (input.keep_settings) body.keep_settings = true;`,
    // which (a) hard-coded `true` regardless of the caller's
    // value and (b) silently dropped `false`. Both behaviours
    // violate the "forward what the caller sent" wire-contract
    // rule we already enforce for `limit` (ANALYSIS_0005,
    // round 2). The fix uses `!= null`, mirroring the limit
    // pattern. The server's upgradeRequestBody treats omission
    // and `keep_settings:false` as identical (both fall through
    // to the default keep-existing branch), so this is a
    // wire-contract honesty fix \u2014 it doesn't change server
    // semantics but it ensures the next person who copies the
    // pattern into a field where false IS semantically distinct
    // doesn't inherit the bug.
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
      keep_settings: false,
    });
    const init = fetchSpy.mock.calls[0][1];
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    // Key MUST be present and MUST be false (not coerced to true).
    expect("keep_settings" in body).toBe(true);
    expect(body.keep_settings).toBe(false);
  });

  it("upgradeMarketplaceInstallation omits keep_settings when undefined (ANALYSIS_0004 negative case)", async () => {
    // Negative case: caller didn't supply keep_settings at all
    // (the common case where they want the engine's default
    // keep-existing branch). The SDK MUST NOT render an
    // unsolicited `keep_settings: false` in the body \u2014 the
    // wire shape stays minimal.
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
    });
    const init = fetchSpy.mock.calls[0][1];
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect("keep_settings" in body).toBe(false);
  });

  it("upgradeMarketplaceInstallation omits settings when explicitly null (round-5 ANALYSIS_0005)", async () => {
    // Round-5 ANALYSIS_0005: the SDK's two optional upgrade
    // fields used divergent inclusion checks \u2014 keep_settings
    // used `!= null` (covers both undefined and null) while
    // settings used `!== undefined` (only covers undefined). TS
    // types both as optional, so a TypeScript-correct caller
    // can only ever pass `undefined`. But untyped JS callers
    // (third-party scripts, codegen, runtime config) CAN pass
    // `null` \u2014 and the pre-fix divergence meant the SDK would
    // faithfully forward `settings: null` on the wire while
    // silently dropping `keep_settings: null`. That asymmetry
    // is the copy-paste hazard Devin Review flagged.
    //
    // The fix unifies both checks under `!= null`, which is the
    // strictly more defensive of the two: a null on EITHER
    // field is now dropped from the wire body. This test pins
    // the unified contract by feeding both fields explicit
    // nulls (cast through unknown to bypass the optional-only
    // type) and asserting both are absent from the request body.
    const fetchSpy = vi.fn().mockResolvedValue(
      mockJSON({
        installation: {
          id: "install-1",
          tenant_id: "tnt-1",
          extension_id: "ext-1",
          extension_version_id: "v1",
          status: "active",
          settings: {},
          webhook_base: "https://t.example.com",
          installed_at: "2025-03-01T00:00:00Z",
          updated_at: "2025-03-01T00:00:00Z",
        },
        from_version_id: "v0",
      }),
    );
    const api = newClient(fetchSpy);
    await api.upgradeMarketplaceInstallation("install-1", {
      from_version_id: "v0",
      to_version_id: "v1",
      // The cast-to-null is the whole point of this test \u2014 an
      // untyped JS caller might send these as actual null values,
      // and the SDK must defend equally against both.
      keep_settings: null as unknown as boolean,
      settings: null as unknown as Record<string, unknown>,
    });
    const init = fetchSpy.mock.calls[0][1];
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect("keep_settings" in body).toBe(false);
    expect("settings" in body).toBe(false);
  });

  it("upgradeMarketplaceInstallation forwards settings={} when supplied (round-5 ANALYSIS_0005 positive case)", async () => {
    // Companion to the previous test: an explicitly-supplied
    // settings document, even one that's the empty object {},
    // MUST round-trip onto the wire. The `!= null` check must
    // NOT short-circuit on truthy-falsy values \u2014 an empty
    // object is the canonical "wipe my settings to defaults"
    // signal, distinct from omission (which preserves the
    // current document).
    const fetchSpy = vi.fn().mockResolvedValue(
      mockJSON({
        installation: {
          id: "install-1",
          tenant_id: "tnt-1",
          extension_id: "ext-1",
          extension_version_id: "v1",
          status: "active",
          settings: {},
          webhook_base: "https://t.example.com",
          installed_at: "2025-03-01T00:00:00Z",
          updated_at: "2025-03-01T00:00:00Z",
        },
        from_version_id: "v0",
      }),
    );
    const api = newClient(fetchSpy);
    await api.upgradeMarketplaceInstallation("install-1", {
      from_version_id: "v0",
      to_version_id: "v1",
      settings: {},
    });
    const init = fetchSpy.mock.calls[0][1];
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect("settings" in body).toBe(true);
    expect(body.settings).toEqual({});
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
