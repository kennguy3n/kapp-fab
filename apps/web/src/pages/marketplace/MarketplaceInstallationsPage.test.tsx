import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

const listMarketplaceInstallations = vi.fn();
const getMarketplaceExtension = vi.fn();
const listMarketplaceVersions = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    listMarketplaceInstallations: (...a: unknown[]) =>
      listMarketplaceInstallations(...a),
    getMarketplaceExtension: (...a: unknown[]) => getMarketplaceExtension(...a),
    listMarketplaceVersions: (...a: unknown[]) => listMarketplaceVersions(...a),
  },
}));

import { MarketplaceInstallationsPage } from "./MarketplaceInstallationsPage";

const EXT = {
  id: "ext-1",
  name: "acme.inventory-sync",
  publisher: "acme",
  slug: "inventory-sync",
  display_name: "Inventory Sync",
  description: "",
  author: "Acme",
  license: "MIT",
  status: "listed" as const,
  listed_version: "1.2.0",
  created_at: "2025-01-01T00:00:00Z",
  updated_at: "2025-02-01T00:00:00Z",
};

const VERSIONS = {
  items: [
    {
      id: "ver-1",
      extension_id: "ext-1",
      version: "1.2.0",
      bundle_hash: "a".repeat(64),
      bundle_size_bytes: 100,
      bundle_url: "",
      min_kapp_version: "1.0.0",
      features_required: [],
      permissions_required: [],
      ktypes_count: 0,
      workflows_count: 0,
      agent_tools_count: 0,
      ui_extensions_count: 0,
      webhooks_count: 0,
      yanked: false,
      published_at: "2025-02-01T00:00:00Z",
    },
    {
      id: "ver-0",
      extension_id: "ext-1",
      version: "1.1.0",
      bundle_hash: "b".repeat(64),
      bundle_size_bytes: 100,
      bundle_url: "",
      min_kapp_version: "1.0.0",
      features_required: [],
      permissions_required: [],
      ktypes_count: 0,
      workflows_count: 0,
      agent_tools_count: 0,
      ui_extensions_count: 0,
      webhooks_count: 0,
      yanked: false,
      published_at: "2025-01-15T00:00:00Z",
    },
  ],
};

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/marketplace/installed"]}>
        <MarketplaceInstallationsPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("MarketplaceInstallationsPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders the empty-state when no installs exist", async () => {
    listMarketplaceInstallations.mockResolvedValueOnce({ items: [] });
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByText(/No extensions are installed yet/i),
      ).toBeInTheDocument(),
    );
  });

  it("renders each install row with status + extension display name", async () => {
    listMarketplaceInstallations.mockResolvedValueOnce({
      items: [
        {
          id: "install-1",
          tenant_id: "tnt-1",
          extension_id: "ext-1",
          extension_version_id: "ver-0", // on the older version
          status: "active",
          settings: {},
          webhook_base: "https://acme.example.com",
          installed_at: "2025-03-01T00:00:00Z",
          updated_at: "2025-03-01T00:00:00Z",
        },
      ],
    });
    getMarketplaceExtension.mockResolvedValueOnce({
      extension: EXT,
      versions: VERSIONS.items,
    });
    listMarketplaceVersions.mockResolvedValueOnce(VERSIONS);

    renderPage();
    await waitFor(() =>
      expect(screen.getByText("Inventory Sync")).toBeInTheDocument(),
    );
    expect(screen.getByText(/Active/)).toBeInTheDocument();
    // Behind the default → shows the Update available badge.
    await waitFor(() =>
      expect(screen.getByText(/Update available/i)).toBeInTheDocument(),
    );
    // Version row resolves to v1.1.0 (the installed version, not the default).
    expect(screen.getByText(/v1\.1\.0/)).toBeInTheDocument();
  });

  it("does NOT fan out a per-row listMarketplaceVersions call (versions reused from getMarketplaceExtension to avoid N+1)", async () => {
    // Regression for the N+1 round-trip pattern: each row used
    // to fire its own useQuery against listMarketplaceVersions,
    // duplicating data already present in the parent's
    // getMarketplaceExtension response. After the fix the page
    // should NEVER call listMarketplaceVersions — versions are
    // sourced from the extQueries[i].data.versions array and
    // passed down as a prop.
    listMarketplaceInstallations.mockResolvedValueOnce({
      items: [
        {
          id: "install-1",
          tenant_id: "tnt-1",
          extension_id: "ext-1",
          extension_version_id: "ver-0",
          status: "active",
          settings: {},
          webhook_base: "https://acme.example.com",
          installed_at: "2025-03-01T00:00:00Z",
          updated_at: "2025-03-01T00:00:00Z",
        },
        {
          id: "install-2",
          tenant_id: "tnt-1",
          extension_id: "ext-1", // same extension, different install
          extension_version_id: "ver-1",
          status: "disabled",
          settings: {},
          webhook_base: "",
          installed_at: "2025-03-02T00:00:00Z",
          updated_at: "2025-03-02T00:00:00Z",
        },
      ],
    });
    getMarketplaceExtension.mockResolvedValue({
      extension: EXT,
      versions: VERSIONS.items,
    });
    renderPage();
    // Wait for the rows to actually render so we know the
    // useQueries cycle has settled.
    await waitFor(() =>
      expect(screen.getAllByText(/Inventory Sync/i).length).toBeGreaterThan(0),
    );
    // The extension lookup query MUST fire (once per unique
    // extension id) — but listMarketplaceVersions MUST NOT.
    expect(getMarketplaceExtension).toHaveBeenCalledWith("ext-1");
    expect(listMarketplaceVersions).not.toHaveBeenCalled();
    // Both rows resolve their version labels — proving that
    // versions did flow through the shared lookup.
    expect(screen.getByText(/v1\.1\.0/)).toBeInTheDocument();
    expect(screen.getByText(/v1\.2\.0/)).toBeInTheDocument();
  });

  it("surfaces failure_reason inline for failed installs", async () => {
    listMarketplaceInstallations.mockResolvedValueOnce({
      items: [
        {
          id: "install-2",
          tenant_id: "tnt-1",
          extension_id: "ext-1",
          extension_version_id: "ver-1",
          status: "failed",
          settings: {},
          webhook_base: "",
          installed_at: "2025-03-01T00:00:00Z",
          updated_at: "2025-03-01T00:00:00Z",
          failure_reason: "settings validation failed: field 'api_key' required",
        },
      ],
    });
    getMarketplaceExtension.mockResolvedValueOnce({
      extension: EXT,
      versions: VERSIONS.items,
    });
    listMarketplaceVersions.mockResolvedValueOnce(VERSIONS);
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/Failed/)).toBeInTheDocument(),
    );
    expect(
      screen.getByText(/api_key.*required/i),
    ).toBeInTheDocument();
  });
});
