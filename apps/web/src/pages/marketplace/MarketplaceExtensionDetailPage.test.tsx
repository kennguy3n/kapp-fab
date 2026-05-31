import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getMarketplaceExtension = vi.fn();
const listMarketplaceInstallations = vi.fn();
const installMarketplaceExtension = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    getMarketplaceExtension: (...a: unknown[]) => getMarketplaceExtension(...a),
    listMarketplaceInstallations: (...a: unknown[]) =>
      listMarketplaceInstallations(...a),
    installMarketplaceExtension: (...a: unknown[]) =>
      installMarketplaceExtension(...a),
  },
}));

import { MarketplaceExtensionDetailPage } from "./MarketplaceExtensionDetailPage";

const EXT_FIXTURE = {
  extension: {
    id: "ext-1",
    name: "acme.inventory-sync",
    publisher: "acme",
    slug: "inventory-sync",
    display_name: "Inventory Sync",
    description: "Syncs stock levels with external WMS feeds.",
    author: "Acme Corp",
    license: "MIT",
    status: "listed" as const,
    listed_version: "1.2.0",
    created_at: "2025-01-01T00:00:00Z",
    updated_at: "2025-02-01T00:00:00Z",
  },
  versions: [
    {
      id: "ver-1",
      extension_id: "ext-1",
      version: "1.2.0",
      bundle_hash: "abc123def4567890" + "0".repeat(48),
      bundle_size_bytes: 102400,
      bundle_url: "https://cdn.example.com/abc123",
      min_kapp_version: "1.0.0",
      features_required: ["inventory"],
      permissions_required: ["records.write"],
      ktypes_count: 1,
      workflows_count: 0,
      agent_tools_count: 0,
      ui_extensions_count: 0,
      webhooks_count: 2,
      yanked: false,
      published_at: "2025-02-01T00:00:00Z",
      bundle_signature: "sig-bytes",
      bundle_signature_key_id: "key-1",
      signed_at: "2025-02-01T00:00:00Z",
    },
    {
      id: "ver-0",
      extension_id: "ext-1",
      version: "1.1.0",
      bundle_hash: "old111" + "0".repeat(58),
      bundle_size_bytes: 90000,
      bundle_url: "https://cdn.example.com/old",
      min_kapp_version: "1.0.0",
      features_required: [],
      permissions_required: [],
      ktypes_count: 1,
      workflows_count: 0,
      agent_tools_count: 0,
      ui_extensions_count: 0,
      webhooks_count: 1,
      yanked: false,
      published_at: "2025-01-15T00:00:00Z",
    },
  ],
};

function renderPage(extId = "ext-1") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[`/marketplace/extensions/${extId}`]}>
        <Routes>
          <Route
            path="/marketplace/extensions/:extId"
            element={<MarketplaceExtensionDetailPage />}
          />
          <Route
            path="/marketplace/installed/:installId"
            element={<div>install-detail</div>}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("MarketplaceExtensionDetailPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    listMarketplaceInstallations.mockResolvedValue({ items: [] });
  });

  it("renders the extension header + Listed status badge", async () => {
    getMarketplaceExtension.mockResolvedValueOnce(EXT_FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /Inventory Sync/i }),
      ).toBeInTheDocument(),
    );
    expect(screen.getAllByText(/Listed/i).length).toBeGreaterThan(0);
  });

  it("disables Install when the user already has an install for this extension", async () => {
    getMarketplaceExtension.mockResolvedValueOnce(EXT_FIXTURE);
    listMarketplaceInstallations.mockResolvedValueOnce({
      items: [
        {
          id: "install-1",
          tenant_id: "tnt-1",
          extension_id: "ext-1",
          extension_version_id: "ver-1",
          status: "active",
          settings: {},
          webhook_base: "https://acme.example.com",
          installed_at: "2025-03-01T00:00:00Z",
          updated_at: "2025-03-01T00:00:00Z",
        },
      ],
    });
    renderPage();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Manage install/i }))
        .toBeInTheDocument(),
    );
    // Install button should NOT be present when already installed.
    expect(
      screen.queryByRole("button", { name: /^Install /i }),
    ).not.toBeInTheDocument();
  });

  it("renders the Versions tab with both rows and surfaces the default", async () => {
    getMarketplaceExtension.mockResolvedValueOnce(EXT_FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: /Versions/i })).toBeInTheDocument(),
    );
    await userEvent.click(screen.getByRole("tab", { name: /Versions/i }));
    // Both versions appear in the table (use getAllBy because the
    // default badge has the version too).
    await waitFor(() => {
      expect(screen.getAllByText("v1.2.0").length).toBeGreaterThan(0);
    });
    expect(screen.getAllByText("v1.1.0").length).toBeGreaterThan(0);
    expect(screen.getByText("DEFAULT")).toBeInTheDocument();
  });

  it("renders the Permissions tab with the listed-version requirements", async () => {
    getMarketplaceExtension.mockResolvedValueOnce(EXT_FIXTURE);
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("tab", { name: /Permissions/i }),
      ).toBeInTheDocument(),
    );
    await userEvent.click(screen.getByRole("tab", { name: /Permissions/i }));
    await waitFor(() =>
      expect(screen.getByText("inventory")).toBeInTheDocument(),
    );
    expect(screen.getByText("records.write")).toBeInTheDocument();
  });
});
