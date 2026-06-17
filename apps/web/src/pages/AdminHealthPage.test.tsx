import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { LocaleProvider } from "../lib/i18n";
import { AdminHealthPage } from "./AdminHealthPage";

const FIXTURE = {
  system: {
    status: "degraded" as const,
    components: [
      {
        name: "postgres",
        status: "operational" as const,
        latency_ms: 1.5,
        detail: { replica: false },
      },
      {
        name: "outbox",
        status: "degraded" as const,
        latency_ms: 3.2,
        detail: { undelivered_events: 4200, oldest_event_age_seconds: 90 },
      },
    ],
    checked_at: "2026-06-03T00:00:00Z",
  },
  cells: [
    {
      id: "default",
      region: "local",
      max_tenants: 1000,
      tenant_count: 250,
      cpu_pct: 30,
      mem_pct: 40,
      conn_saturation_pct: 20,
      utilization_pct: 25,
    },
  ],
  pool: {
    max_conns: 20,
    total_conns: 8,
    acquired_conns: 3,
    idle_conns: 5,
    saturation_percent: 40,
  },
  top_tenants: [
    { tenant_id: "t-1", name: "Acme", api_calls: 5000 },
    { tenant_id: "t-2", name: "Globex", api_calls: 1200 },
  ],
};

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <LocaleProvider>
        <MemoryRouter initialEntries={["/admin/health"]}>
          <AdminHealthPage />
        </MemoryRouter>
      </LocaleProvider>
    </QueryClientProvider>,
  );
}

describe("AdminHealthPage", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("renders components, pool saturation, outbox backlog, cells and top tenants", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(FIXTURE), { status: 200 }),
    );
    renderPage();

    // Component rows (machine names are humanized into Title Case);
    // wait on a data-only row so we're past the loading skeleton.
    expect(await screen.findByText("Postgres")).toBeInTheDocument();
    expect(screen.getByText("System health")).toBeInTheDocument();
    expect(screen.getByText("Outbox")).toBeInTheDocument();
    // Outbox backlog surfaced from component detail, locale-formatted.
    expect(screen.getByText("4,200")).toBeInTheDocument();
    // Pool saturation.
    expect(
      screen.getByText(/8 \/ 20 connections \(3 in use, 5 idle\)/),
    ).toBeInTheDocument();
    // Cell + tenant leaderboard (counts are locale-formatted).
    expect(screen.getByText(/250 \/ 1,000 tenants/)).toBeInTheDocument();
    expect(screen.getByText("Acme")).toBeInTheDocument();
    expect(screen.getByText("Globex")).toBeInTheDocument();
  });

  it("forwards the bearer token to the admin endpoint", async () => {
    localStorage.setItem("kapp.token", "test-jwt");
    localStorage.setItem("kapp.tenant", "tenant-xyz");
    const spy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify(FIXTURE), { status: 200 }));
    renderPage();
    await screen.findByText("System health");

    expect(spy).toHaveBeenCalledWith(
      "/api/v1/admin/health/detailed",
      expect.objectContaining({
        headers: expect.objectContaining({
          Authorization: "Bearer test-jwt",
          "X-Tenant-ID": "tenant-xyz",
        }),
      }),
    );
  });

  it("renders an error state on failure", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("no", { status: 403 }));
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByText(/Couldn't load system health/i),
      ).toBeInTheDocument(),
    );
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });
});
