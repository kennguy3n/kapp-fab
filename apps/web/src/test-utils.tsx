/**
 * Shared test utilities for the apps/web Vitest suite.
 *
 * Page and component tests in this app all need the same handful of
 * providers around the unit under test:
 *
 *   • QueryClientProvider — almost every page reads data through
 *     TanStack Query (`useQuery` / `useMutation`). Tests want a fresh
 *     client per render with retries disabled so a rejected mock
 *     surfaces the error state immediately instead of being retried.
 *   • MemoryRouter — pages use `<Link>` / `useNavigate` / `useParams`,
 *     all of which throw outside a Router. A MemoryRouter also lets a
 *     test drive the initial URL (and route params) without a browser.
 *   • LocaleProvider — pages call `useTranslation()` / `useFormatter()`
 *     which read the locale context. Using the real provider (rather
 *     than a per-test mock of `../lib/i18n`) keeps `t()` returning the
 *     real English strings the assertions match against, and exercises
 *     the i18n module itself.
 *   • TooltipProvider — `@kapp/ui` primitives that render a tooltip
 *     (sidebar items, icon buttons) read this context.
 *
 * `renderWithProviders` wraps the unit in all four. When a test needs
 * route params it passes `path` (the route pattern, e.g.
 * "/records/:ktype") alongside `route` (the concrete URL) so the
 * element mounts under a matching `<Route>`; otherwise the element is
 * rendered directly under the router.
 *
 * Auth/tenant state in this app is not React context — it lives in
 * localStorage (`kapp.tenant` / `kapp.token`, see src/lib/api.ts).
 * `seedTenant` / `seedAuth` (and the `tenant` / `token` render
 * options) prime that storage so a page's tenant-scoped query keys and
 * Authorization header behave as they do in the running app.
 */
import { type ReactElement, type ReactNode } from "react";
import { render, type RenderOptions, type RenderResult } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { TooltipProvider } from "@kapp/ui";
import { LocaleProvider } from "./lib/i18n";

// localStorage keys the app reads for tenant scoping + auth. Kept in
// sync with src/lib/api.ts (tenantId() / token()).
const TENANT_KEY = "kapp.tenant";
const TOKEN_KEY = "kapp.token";

/**
 * Build a QueryClient tuned for tests: no retries (so a rejected mock
 * resolves to the error branch on the first tick) and no
 * retry-on-mount, with a quiet logger so an intentionally-rejected
 * query doesn't spam the test console.
 */
export function createTestQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0, staleTime: 0 },
      mutations: { retry: false },
    },
  });
}

/** Seed the tenant id the app reads from localStorage for query
 *  scoping. Returns the id so callers can assert on derived keys. */
export function seedTenant(tenantId: string): string {
  window.localStorage.setItem(TENANT_KEY, tenantId);
  return tenantId;
}

/** Seed a bearer token so `api`'s header builder attaches
 *  Authorization, matching the signed-in app state. */
export function seedAuth(token: string): string {
  window.localStorage.setItem(TOKEN_KEY, token);
  return token;
}

export interface ProvidersOptions {
  /** Concrete initial URL the MemoryRouter starts at. Default "/". */
  route?: string;
  /** Route pattern to mount the element under (e.g. "/records/:ktype").
   *  Provide together with a matching `route` when the component reads
   *  params via useParams. When omitted the element renders directly
   *  under the router. */
  path?: string;
  /** Reuse an existing QueryClient (e.g. to inspect its cache); a
   *  fresh test client is created when omitted. */
  queryClient?: QueryClient;
  /** Seed localStorage `kapp.tenant` before rendering. */
  tenant?: string;
  /** Seed localStorage `kapp.token` before rendering. */
  token?: string;
  /** Extra RTL render options (container, baseElement, …). */
  renderOptions?: Omit<RenderOptions, "wrapper">;
}

export interface RenderWithProvidersResult extends RenderResult {
  /** The QueryClient backing the render — exposed so a test can read
   *  cache state or invalidate queries directly. */
  queryClient: QueryClient;
}

/**
 * Render `ui` wrapped in the app's standard provider stack. Returns
 * the usual RTL result plus the `queryClient` used, so a test can
 * inspect query/mutation cache state.
 */
export function renderWithProviders(
  ui: ReactElement,
  options: ProvidersOptions = {},
): RenderWithProvidersResult {
  const {
    route = "/",
    path,
    queryClient = createTestQueryClient(),
    tenant,
    token,
    renderOptions,
  } = options;

  if (tenant !== undefined) seedTenant(tenant);
  if (token !== undefined) seedAuth(token);

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <LocaleProvider>
        <TooltipProvider delayDuration={0}>
          <QueryClientProvider client={queryClient}>
            <MemoryRouter initialEntries={[route]}>
              {path ? (
                <Routes>
                  <Route path={path} element={children} />
                </Routes>
              ) : (
                children
              )}
            </MemoryRouter>
          </QueryClientProvider>
        </TooltipProvider>
      </LocaleProvider>
    );
  }

  const result = render(ui, { wrapper: Wrapper, ...renderOptions });
  return { ...result, queryClient };
}

// Re-export the data factories so a test can pull both the render
// helper and fixtures from a single import.
export * from "./test/factories";
