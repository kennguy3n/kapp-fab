import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const installMarketplaceExtension = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    installMarketplaceExtension: (...a: unknown[]) =>
      installMarketplaceExtension(...a),
  },
}));

import { InstallExtensionDialog } from "./InstallExtensionDialog";

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

const VER = {
  id: "ver-1",
  extension_id: "ext-1",
  version: "1.2.0",
  bundle_hash: "abc" + "0".repeat(61),
  bundle_size_bytes: 102400,
  bundle_url: "",
  min_kapp_version: "1.0.0",
  features_required: ["inventory"],
  permissions_required: ["records.write"],
  ktypes_count: 1,
  workflows_count: 0,
  agent_tools_count: 0,
  ui_extensions_count: 0,
  webhooks_count: 1,
  yanked: false,
  published_at: "2025-02-01T00:00:00Z",
};

function renderDialog({
  onInstalled = vi.fn(),
  onClose = vi.fn(),
}: {
  onInstalled?: ReturnType<typeof vi.fn>;
  onClose?: ReturnType<typeof vi.fn>;
} = {}) {
  const qc = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  });
  return {
    onInstalled,
    onClose,
    ...render(
      <QueryClientProvider client={qc}>
        <InstallExtensionDialog
          extension={EXT}
          version={VER}
          onClose={onClose}
          onInstalled={onInstalled}
        />
      </QueryClientProvider>,
    ),
  };
}

describe("InstallExtensionDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows version + permission requirements + webhook base", () => {
    renderDialog();
    expect(screen.getByText(/Install Inventory Sync v1\.2\.0/)).toBeInTheDocument();
    expect(screen.getByText("inventory")).toBeInTheDocument();
    expect(screen.getByText("records.write")).toBeInTheDocument();
    expect(screen.getByLabelText(/Webhook base URL/i)).toBeInTheDocument();
  });

  it("validates the webhook base URL before posting", async () => {
    renderDialog();
    const input = screen.getByLabelText(/Webhook base URL/i);
    await userEvent.clear(input);
    await userEvent.type(input, "not-a-url");
    await userEvent.click(
      screen.getByRole("button", { name: /Install extension/i }),
    );
    await waitFor(() =>
      expect(screen.getByText(/valid URL|http\(s\)/i)).toBeInTheDocument(),
    );
    expect(installMarketplaceExtension).not.toHaveBeenCalled();
  });

  it("posts the install + invokes onInstalled with the API response", async () => {
    const onInstalled = vi.fn();
    installMarketplaceExtension.mockResolvedValueOnce({
      installation: {
        id: "install-1",
        tenant_id: "tnt-1",
        extension_id: "ext-1",
        extension_version_id: "ver-1",
        status: "active",
        settings: {},
        webhook_base: "https://t.example.com",
        installed_at: "2025-03-01T00:00:00Z",
        updated_at: "2025-03-01T00:00:00Z",
      },
      signing_secret: "sec",
    });
    renderDialog({ onInstalled });
    const input = screen.getByLabelText(/Webhook base URL/i);
    await userEvent.clear(input);
    await userEvent.type(input, "https://t.example.com");
    await userEvent.click(
      screen.getByRole("button", { name: /Install extension/i }),
    );
    await waitFor(() => expect(installMarketplaceExtension).toHaveBeenCalled());
    const args = installMarketplaceExtension.mock.calls[0][0];
    expect(args).toMatchObject({
      extension_id: "ext-1",
      version_id: "ver-1",
      webhook_base: "https://t.example.com",
    });
    await waitFor(() => expect(onInstalled).toHaveBeenCalled());
  });

  it("surfaces a server error inside the dialog", async () => {
    installMarketplaceExtension.mockRejectedValueOnce(
      new Error("409 install already exists"),
    );
    renderDialog();
    const input = screen.getByLabelText(/Webhook base URL/i);
    await userEvent.clear(input);
    await userEvent.type(input, "https://t.example.com");
    await userEvent.click(
      screen.getByRole("button", { name: /Install extension/i }),
    );
    await waitFor(() =>
      expect(screen.getByText(/409 install already exists/)).toBeInTheDocument(),
    );
  });
});
