import type { Page, Route, Request } from "@playwright/test";

// Deterministic, in-memory mock of the /api/v1 surface the SPA talks
// to. Every E2E spec installs this so the suite never touches a real
// backend — responses are fixed fixtures and mutations are reflected
// in subsequent reads (so a created record shows up in the list).
//
// The SPA's ApiClient issues plain fetch() calls to `/api/v1/...`, so
// intercepting at the network layer with page.route exercises the
// real component + query-cache + routing stack end to end.

export interface MockState {
  deals: Array<Record<string, unknown>>;
}

const TENANT_ID = "00000000-0000-4000-8000-000000000001";

const DEAL_KTYPE = {
  tenant_id: TENANT_ID,
  name: "crm.deal",
  version: 1,
  schema: {
    fields: [
      { name: "title", type: "string", required: true },
      { name: "stage", type: "enum", values: ["open", "won", "lost"] },
      { name: "value", type: "number" },
    ],
  },
  workflow: null,
  created_at: "2024-01-01T00:00:00Z",
  updated_at: "2024-01-01T00:00:00Z",
};

const DASHBOARD_SUMMARY = {
  open_deals_count: 7,
  pipeline_value: 125000,
  outstanding_ar: 42000,
  outstanding_ap: 18000,
  low_stock_items_count: 3,
  pending_approvals: 2,
  open_tickets_count: 5,
  overdue_tickets_count: 1,
  present_today: 12,
  pending_reviews: 4,
  base_currency: "USD",
};

const RUN_RESULT = {
  result: {
    columns: ["stage", "count"],
    rows: [
      { stage: "open", count: 7 },
      { stage: "won", count: 4 },
      { stage: "lost", count: 2 },
    ],
  },
  cache_hit: false,
  query_hash: "hash-1",
  filter_hash: "filter-1",
  expires_at: null,
};

function makeDeal(
  id: string,
  data: Record<string, unknown>,
): Record<string, unknown> {
  return {
    id,
    tenant_id: TENANT_ID,
    ktype: "crm.deal",
    version: 1,
    data,
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
  };
}

export function initialState(): MockState {
  return {
    deals: [
      makeDeal("deal-1", { title: "Acme renewal", stage: "open", value: 50000 }),
      makeDeal("deal-2", { title: "Globex expansion", stage: "won", value: 75000 }),
    ],
  };
}

function json(route: Route, body: unknown, status = 200): Promise<void> {
  return route.fulfill({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
}

/**
 * Registers a single catch-all handler for /api/v1/**. Known routes
 * return purpose-built fixtures; anything unmatched returns an empty
 * 200 so an incidental background query (features, notifications, …)
 * never fails the page. The handler closes over `state` so writes are
 * observable by later reads within the same test.
 */
export async function installApiMock(
  page: Page,
  state: MockState = initialState(),
): Promise<MockState> {
  await page.route("**/api/v1/**", async (route: Route, request: Request) => {
    const url = new URL(request.url());
    const path = url.pathname.replace(/^.*\/api\/v1/, "");
    const method = request.method().toUpperCase();

    // --- auth -----------------------------------------------------
    if (path === "/auth/sso" && method === "POST") {
      return json(route, {
        access_token: "e2e-access-token",
        refresh_token: "e2e-refresh-token",
        tenant_id: TENANT_ID,
        expires_in: 3600,
      });
    }

    // --- shell background queries --------------------------------
    if (/^\/tenants\/[^/]+\/features$/.test(path) && method === "GET") {
      return json(route, { features: {} });
    }
    if (path === "/notifications" && method === "GET") {
      return json(route, []);
    }

    // --- dashboard ------------------------------------------------
    if (path === "/dashboard/summary" && method === "GET") {
      return json(route, DASHBOARD_SUMMARY);
    }

    // --- ktypes ---------------------------------------------------
    if (path === "/ktypes" && method === "GET") {
      return json(route, [DEAL_KTYPE]);
    }
    if (path === "/ktypes/crm.deal" && method === "GET") {
      return json(route, DEAL_KTYPE);
    }

    // --- saved views (record list) -------------------------------
    if (path.startsWith("/views") && method === "GET") {
      return json(route, []);
    }

    // --- records CRUD --------------------------------------------
    if (path === "/records/crm.deal" && method === "GET") {
      return json(route, state.deals);
    }
    if (path === "/records/crm.deal" && method === "POST") {
      // createRecord posts `{ data }`, not the bare field map.
      const { data } = request.postDataJSON() as {
        data: Record<string, unknown>;
      };
      const created = makeDeal(`deal-${state.deals.length + 1}`, data);
      state.deals.push(created);
      return json(route, created, 201);
    }
    const recMatch = path.match(/^\/records\/crm\.deal\/([^/]+)$/);
    if (recMatch) {
      const id = recMatch[1];
      const existing = state.deals.find((d) => d.id === id);
      if (method === "GET") {
        return existing
          ? json(route, existing)
          : json(route, { error: "not found" }, 404);
      }
      if (method === "PUT" || method === "PATCH") {
        // updateRecord PATCHes `{ data }` as well. Updating a record
        // that doesn't exist is a 404 — same as GET above and as the
        // real API behaves — rather than fabricating a phantom record
        // that the next GET wouldn't find (an internally inconsistent
        // mock).
        if (!existing) return json(route, { error: "not found" }, 404);
        const { data } = request.postDataJSON() as {
          data: Record<string, unknown>;
        };
        existing.data = data;
        return json(route, existing);
      }
    }

    // --- insights -------------------------------------------------
    if (path === "/insights/queries" && method === "GET") {
      return json(route, { queries: [] });
    }
    if (path === "/insights/queries" && method === "POST") {
      const payload = request.postDataJSON() as Record<string, unknown>;
      return json(route, {
        tenant_id: TENANT_ID,
        id: "query-1",
        name: (payload.name as string) ?? "Untitled",
        description: (payload.description as string) ?? "",
        definition: payload.definition ?? {},
        mode: payload.mode ?? "visual",
        created_at: "2024-01-01T00:00:00Z",
        updated_at: "2024-01-01T00:00:00Z",
      });
    }
    if (/^\/insights\/queries\/[^/]+\/run$/.test(path) && method === "POST") {
      return json(route, RUN_RESULT);
    }
    if (path === "/insights/dashboards" && method === "GET") {
      return json(route, { dashboards: [] });
    }

    // --- default: empty success ----------------------------------
    if (method === "GET") return json(route, []);
    return json(route, {});
  });

  return state;
}

/**
 * Seeds an authenticated session before any app script runs, so pages
 * behind the tenant shell render immediately. Mirrors what LoginPage
 * persists after a successful SSO exchange.
 */
export async function seedSession(page: Page): Promise<void> {
  await page.addInitScript((tenant) => {
    localStorage.setItem("kapp.token", "e2e-access-token");
    localStorage.setItem("kapp.tenant", tenant);
    localStorage.setItem(
      "kapp.expires_at",
      String(Date.now() + 3600 * 1000),
    );
  }, TENANT_ID);
}

export { TENANT_ID };
