// Default MSW request handlers for the apps/web unit suite.
//
// These are the "happy path" responses for the handful of endpoints
// that components hit through the raw `fetch` API (NotificationBell,
// LoginPage SSO) plus a couple of ApiClient routes used by the
// MSW-backed page tests (TenantListPage, DashboardPage). Anything not
// covered here is rejected by the server's `onUnhandledRequest: "error"`
// setting (see server.ts) so a test that triggers an unexpected request
// fails loudly instead of silently hitting the network.
//
// A test that needs a specific shape or an error response overrides the
// relevant route per-test with `server.use(...)`; the afterEach
// `server.resetHandlers()` in setup.ts restores these defaults between
// tests.

import { http, HttpResponse } from "msw";
import { makeTenant } from "../factories";

// All API routes are served under the same /api/v1 prefix the Vite dev
// proxy and the production ApiClient (baseUrl "/api/v1") use.
const API = "/api/v1";

export const handlers = [
  // --- Notifications (NotificationBell, raw fetch) --------------------
  http.get(`${API}/notifications`, () => HttpResponse.json([])),
  // 204 No Content carries no body, so use a bare HttpResponse rather
  // than HttpResponse.json(null, ...) (which would attach a JSON body).
  http.post(`${API}/notifications/:id/read`, () =>
    new HttpResponse(null, { status: 204 }),
  ),
  http.post(`${API}/notifications/read-all`, () =>
    new HttpResponse(null, { status: 204 }),
  ),

  // --- Auth (LoginPage SSO code exchange, raw fetch) ------------------
  http.post(`${API}/auth/sso`, () =>
    HttpResponse.json({
      access_token: "test-access-token",
      refresh_token: "test-refresh-token",
      tenant_id: "00000000-0000-4000-8000-000000000001",
      expires_in: 3600,
    }),
  ),

  // --- Tenant control plane (ApiClient.listTenants) -------------------
  http.get(`${API}/tenants`, () => HttpResponse.json([makeTenant()])),
];
