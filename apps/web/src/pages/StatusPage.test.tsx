import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { StatusPage } from "./StatusPage";

const OPERATIONAL = {
  status: "operational" as const,
  component_availability_percent: 100,
  components: [
    // The public API emits generic, technology-agnostic names
    // (database, cache, …) rather than raw probe names (postgres,
    // redis, …) so a public scrape cannot fingerprint the stack.
    { name: "database", status: "operational" as const, latency_ms: 1.2 },
    { name: "cache", status: "operational" as const, latency_ms: 0.4 },
  ],
  incidents: [
    { summary: "Platform capacity increased to absorb load", at: "2026-06-01T12:00:00Z" },
  ],
  checked_at: "2026-06-03T00:00:00Z",
};

const DEGRADED = {
  status: "degraded" as const,
  component_availability_percent: 50,
  components: [
    { name: "database", status: "operational" as const, latency_ms: 1.0 },
    { name: "event_delivery", status: "degraded" as const, latency_ms: 2.0 },
  ],
  incidents: [],
  checked_at: "2026-06-03T00:00:00Z",
};

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/status"]}>
        <StatusPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("StatusPage", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("renders operational banner, components (with friendly labels) and incidents", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(OPERATIONAL), { status: 200 }),
    );
    renderPage();

    expect(await screen.findByText("All systems operational")).toBeInTheDocument();
    expect(screen.getByText("100% of components operational")).toBeInTheDocument();
    // Machine names mapped to human labels.
    expect(screen.getByText("Database")).toBeInTheDocument();
    expect(screen.getByText("Cache")).toBeInTheDocument();
    expect(
      screen.getByText("Platform capacity increased to absorb load"),
    ).toBeInTheDocument();
  });

  it("calls the public endpoint without auth headers", async () => {
    const spy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify(OPERATIONAL), { status: 200 }));
    renderPage();
    await screen.findByText("All systems operational");
    expect(spy).toHaveBeenCalledWith("/api/v1/health");
  });

  it("shows the degraded banner when a component is degraded", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(DEGRADED), { status: 200 }),
    );
    renderPage();
    expect(await screen.findByText("Some systems degraded")).toBeInTheDocument();
  });

  it("renders an error state when the request fails", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("nope", { status: 500 }));
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByText(/Unable to load platform status/i),
      ).toBeInTheDocument(),
    );
  });
});
