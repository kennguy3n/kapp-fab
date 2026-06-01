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

  it("opens the install dialog from the Versions-tab per-row Install when listed_version is empty (regression: header CTA disabled but per-row should still work via picked-version lookup)", async () => {
    // Simulates a publisher who has approved versions in the
    // catalogue but hasn't promoted any of them as the
    // listed_version yet — e.g. a soft-launch state. The
    // header CTA is disabled (no listedVersion to anchor to),
    // but the Versions tab lists every approved version with
    // its own per-row Install button. Prior to the BUG_0001
    // fix the dialog gate required listedVersion to be
    // truthy, so clicking the per-row Install set
    // installVersionId but the dialog never rendered (silent
    // no-op for the user). The fix anchors the dialog on the
    // picked version via versions.find(), so the dialog
    // opens regardless of listedVersion state.
    getMarketplaceExtension.mockResolvedValueOnce({
      extension: { ...EXT_FIXTURE.extension, listed_version: "" },
      versions: EXT_FIXTURE.versions,
    });
    renderPage();
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: /Versions/i })).toBeInTheDocument(),
    );
    await userEvent.click(screen.getByRole("tab", { name: /Versions/i }));
    // The header CTA + the 2 per-row buttons all match
    // /^Install$/i; the header CTA is disabled with a
    // title="No installable version is available." tooltip
    // when listed_version is empty.
    const allInstalls = await screen.findAllByRole("button", {
      name: /^Install$/i,
    });
    expect(allInstalls.length).toBeGreaterThanOrEqual(2);
    const headerCta = allInstalls.find(
      (b) =>
        b.getAttribute("title") === "No installable version is available.",
    );
    expect(headerCta).toBeDefined();
    expect(headerCta).toBeDisabled();
    // The per-row Install buttons should NOT be disabled and
    // clicking one MUST open the install dialog.
    const perRow = allInstalls.find((b) => !b.hasAttribute("disabled"));
    expect(perRow).toBeDefined();
    await userEvent.click(perRow!);
    // The InstallExtensionDialog opens with a permission-summary
    // section; pin on that text so we know the dialog rendered
    // even though listedVersion is empty.
    await waitFor(() =>
      expect(
        screen.getByText(/Install Inventory Sync/i),
      ).toBeInTheDocument(),
    );
  });

  it("Versions-tab per-row Install button remains available when the listed default version is yanked (round-8 ANALYSIS_0004 distinct from header CTA)", async () => {
    // Round-8 ANALYSIS_0004: the header CTA's `installable`
    // and the VersionsTab's `canInstallVersions` prop look
    // similar but encode different invariants. The header
    // CTA installs the listed/default version, so its gate
    // bakes in `!!listedVersion && !listedVersion.yanked`.
    // The VersionsTab gate is a *prerequisite* (no install
    // row + ext.status==="listed") that enables per-row
    // Install buttons; each row applies its own `!v.yanked`
    // check.
    //
    // This test pins the case where the two gates produce
    // visibly different results: the listed default version
    // is yanked, so the header CTA must be disabled, BUT
    // the non-yanked older version's per-row Install button
    // must still be available. Pre-rename ("installable"
    // for both flags), a maintainer who collapsed them into
    // one variable would have silently hidden every per-row
    // Install button when the listed default happened to be
    // yanked.
    getMarketplaceExtension.mockResolvedValueOnce({
      extension: EXT_FIXTURE.extension,
      versions: [
        // listed default is yanked
        { ...EXT_FIXTURE.versions[0], yanked: true, yanked_reason: "CVE" },
        // older version is healthy
        EXT_FIXTURE.versions[1],
      ],
    });
    renderPage();
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: /Versions/i })).toBeInTheDocument(),
    );
    await userEvent.click(screen.getByRole("tab", { name: /Versions/i }));
    const allInstalls = await screen.findAllByRole("button", {
      name: /^Install$/i,
    });
    // Header CTA must be disabled with the yanked tooltip.
    const headerCta = allInstalls.find(
      (b) => b.getAttribute("title") === "Default version is yanked.",
    );
    expect(headerCta).toBeDefined();
    expect(headerCta).toBeDisabled();
    // At least one per-row Install button (for the healthy
    // older version) must still be enabled — confirms
    // `canInstallVersions` did NOT inherit the header CTA's
    // yanked check.
    const enabledPerRow = allInstalls.filter(
      (b) => !b.hasAttribute("disabled") && b !== headerCta,
    );
    expect(enabledPerRow.length).toBeGreaterThanOrEqual(1);
  });
});
