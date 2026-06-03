// Node-side MSW server shared by every Vitest file.
//
// `setupServer` patches the global `fetch` (via @mswjs/interceptors) so
// any component or ApiClient call that goes through real `fetch` is
// intercepted and answered by the handlers in handlers.ts. The
// lifecycle hooks (listen / resetHandlers / close) are wired once in
// src/test/setup.ts so individual test files only need to import
// `server` when they want to install a per-test override via
// `server.use(...)`.
//
// Tests that stub `fetch` outright (vi.stubGlobal("fetch", ...)) or
// mock the `../lib/api` module bypass MSW entirely — the server only
// answers requests that reach the real fetch, so the two styles
// coexist without interference.

import { setupServer } from "msw/node";
import { handlers } from "./handlers";

export const server = setupServer(...handlers);
