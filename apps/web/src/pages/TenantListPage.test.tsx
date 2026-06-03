import { describe, it, expect } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import { server } from "../test/msw/server";
import { makeTenant } from "../test/factories";
import { TenantListPage } from "./TenantListPage";

// TenantListPage reads api.listTenants(), which in the non-demo test
// build flows through the real ApiClient (fetch GET /api/v1/tenants).
// This exercises the full MSW <-> ApiClient path with factory data
// rather than a module mock.

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <TenantListPage />
    </QueryClientProvider>,
  );
}

describe("TenantListPage", () => {
  it("renders a row per tenant returned by the API", async () => {
    server.use(
      http.get("/api/v1/tenants", () =>
        HttpResponse.json([
          makeTenant({ slug: "acme", name: "Acme Inc", plan: "pro", status: "active" }),
          makeTenant({ slug: "globex", name: "Globex", plan: "free", status: "suspended" }),
        ]),
      ),
    );
    renderPage();

    expect(await screen.findByText("Acme Inc")).toBeInTheDocument();
    expect(screen.getByText("globex")).toBeInTheDocument();
    expect(screen.getByText("suspended")).toBeInTheDocument();
  });

  it("shows the empty state when no tenants exist", async () => {
    server.use(http.get("/api/v1/tenants", () => HttpResponse.json([])));
    renderPage();
    expect(
      await screen.findByText(/No tenants registered yet\./i),
    ).toBeInTheDocument();
  });

  it("shows an error message when the request fails", async () => {
    server.use(
      http.get("/api/v1/tenants", () =>
        HttpResponse.json({ error: "nope" }, { status: 500 }),
      ),
    );
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/Error loading tenants\./i)).toBeInTheDocument(),
    );
  });
});
