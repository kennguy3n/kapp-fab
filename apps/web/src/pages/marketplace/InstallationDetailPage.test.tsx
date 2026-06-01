import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const getMarketplaceInstallation = vi.fn();
const getMarketplaceExtension = vi.fn();
const listMarketplaceVersions = vi.fn();
const updateMarketplaceInstallationSettings = vi.fn();
const upgradeMarketplaceInstallation = vi.fn();
const uninstallMarketplaceExtension = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    getMarketplaceInstallation: (...a: unknown[]) =>
      getMarketplaceInstallation(...a),
    getMarketplaceExtension: (...a: unknown[]) => getMarketplaceExtension(...a),
    listMarketplaceVersions: (...a: unknown[]) => listMarketplaceVersions(...a),
    updateMarketplaceInstallationSettings: (...a: unknown[]) =>
      updateMarketplaceInstallationSettings(...a),
    upgradeMarketplaceInstallation: (...a: unknown[]) =>
      upgradeMarketplaceInstallation(...a),
    uninstallMarketplaceExtension: (...a: unknown[]) =>
      uninstallMarketplaceExtension(...a),
  },
}));

import { InstallationDetailPage } from "./InstallationDetailPage";

const ROW = {
  id: "install-1",
  tenant_id: "tnt-1",
  extension_id: "ext-1",
  extension_version_id: "ver-0",
  status: "active",
  settings: { api_key: "secret" },
  webhook_base: "https://acme.example.com",
  installed_at: "2025-03-01T00:00:00Z",
  updated_at: "2025-03-01T00:00:00Z",
};

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

const VERSIONS_RESP = {
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
      permissions_required: ["records.write"],
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
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/marketplace/installed/install-1"]}>
        <Routes>
          <Route
            path="/marketplace/installed/:installId"
            element={<InstallationDetailPage />}
          />
          <Route
            path="/marketplace/installed"
            element={<div>installed-list</div>}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("InstallationDetailPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    getMarketplaceInstallation.mockResolvedValue(ROW);
    getMarketplaceExtension.mockResolvedValue({
      extension: EXT,
      versions: VERSIONS_RESP.items,
    });
    listMarketplaceVersions.mockResolvedValue(VERSIONS_RESP);
  });

  it("renders the install header + status + version", async () => {
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /Inventory Sync/i }),
      ).toBeInTheDocument(),
    );
    expect(screen.getByText(/v1\.1\.0/)).toBeInTheDocument();
    expect(screen.getByText(/Active/)).toBeInTheDocument();
  });

  it("offers the upgrade panel because v1.2.0 is newer than the installed v1.1.0", async () => {
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
      ).toBeInTheDocument(),
    );
  });

  it("fires upgradeMarketplaceInstallation with the right from/to + keep_settings on confirmation", async () => {
    upgradeMarketplaceInstallation.mockResolvedValueOnce({
      installation: { ...ROW, extension_version_id: "ver-1" },
      from_version_id: "ver-0",
    });
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
      ).toBeInTheDocument(),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Upgrade to v1\.2\.0/i }),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Upgrade & keep settings/i }),
    );
    await waitFor(() =>
      expect(upgradeMarketplaceInstallation).toHaveBeenCalledWith(
        "install-1",
        {
          from_version_id: "ver-0",
          to_version_id: "ver-1",
          keep_settings: true,
        },
      ),
    );
  });

  it("collapses the upgrade panel when installed version is the most-recent publish (no downgrade offers)", async () => {
    // Tenant is on ver-1 (1.2.0, published 2025-02-01). The
    // older ver-0 (1.1.0, published 2025-01-15) is in the
    // catalogue but MUST NOT appear in the upgrade panel,
    // otherwise users would be silently offered to downgrade
    // — a real risk for settings-schema-incompatible reverts.
    const rowOnLatest = { ...ROW, extension_version_id: "ver-1" };
    getMarketplaceInstallation.mockReset();
    getMarketplaceInstallation.mockResolvedValue(rowOnLatest);
    renderPage();
    // Wait for the page to load — v1.2.0 appears in the header
    // version line and (post-fix) in the "already on latest"
    // empty-state copy, so we don't pin on getByText here.
    await waitFor(() =>
      expect(screen.getAllByText(/v1\.2\.0/).length).toBeGreaterThan(0),
    );
    // The upgrade panel renders an "Upgrade to vX" button when
    // any newer version is available. Confirm NONE exist —
    // including no button offering to "upgrade" back to v1.1.0.
    expect(
      screen.queryByRole("button", { name: /Upgrade to v1\.1\.0/i }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Upgrade to v/i }),
    ).not.toBeInTheDocument();
    // Affirmative signal that the panel ackownledges already-latest.
    expect(
      screen.getByText(/already on the latest approved version/i),
    ).toBeInTheDocument();
  });

  it("invalidates the installation query in onSettled so error-path stale rollbacks get refetched", async () => {
    // Settings update fails; onMutate has already optimistically
    // staged the new value into cache, onError rolls it back to
    // the pre-mutate snapshot. That snapshot may itself be
    // stale (another tab edited concurrently), so the mutation
    // MUST trigger a background refetch via onSettled. We pin
    // that by observing getMarketplaceInstallation is called a
    // second time after the failed PATCH settles.
    updateMarketplaceInstallationSettings.mockRejectedValueOnce(
      new Error("boom"),
    );
    renderPage();
    await waitFor(() =>
      expect(getMarketplaceInstallation).toHaveBeenCalledTimes(1),
    );
    // Touch the settings field then click Save. The schema is
    // currently null in this page (see comment in
    // onSaveSettings) so SettingsForm renders the free-form
    // JSON textarea — we target it via its placeholder rather
    // than a label (the textarea is intentionally label-less
    // because the surrounding Card heading is the label).
    const textarea = await screen.findByPlaceholderText(/api_key/i);
    await userEvent.clear(textarea);
    await userEvent.type(textarea, '{{"api_key":"new"}');
    await userEvent.click(
      screen.getByRole("button", { name: /Save settings/i }),
    );
    // After the mutation settles (success or error) the
    // installation query MUST be invalidated, triggering a
    // background refetch — observable as a second call to
    // getMarketplaceInstallation.
    await waitFor(() =>
      expect(getMarketplaceInstallation.mock.calls.length).toBeGreaterThanOrEqual(2),
    );
  });

  it("confirms before uninstalling and posts the DELETE", async () => {
    uninstallMarketplaceExtension.mockResolvedValueOnce(undefined);
    renderPage();
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Uninstall extension/i }),
      ).toBeInTheDocument(),
    );
    await userEvent.click(
      screen.getByRole("button", { name: /Uninstall extension/i }),
    );
    // Modal confirmation button.
    const confirm = await screen.findByRole("button", {
      name: /^Uninstall$/i,
    });
    await userEvent.click(confirm);
    await waitFor(() =>
      expect(uninstallMarketplaceExtension).toHaveBeenCalledWith("install-1"),
    );
  });
});
