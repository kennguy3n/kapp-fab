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

  it("disables Install + suppresses submit when the freeform JSON textarea has unparseable text (round-5 BUG_0001)", async () => {
    // Round-5 BUG_0001: the FreeformJsonEditor inside the dialog's
    // SettingsForm only fires onChange when its text buffer parses
    // cleanly. When the user types unparseable JSON the editor
    // surfaces a local error but the parent's `settings` state
    // retains the LAST valid value. Pre-fix the Install button
    // was only gated on install.isPending, so clicking it would
    // silently submit the stale-but-valid document instead of the
    // bytes on screen.
    //
    // The fix mirrors InstallationDetailPage's per-key validity
    // map: SettingsForm signals via onValidityChange(key, valid),
    // the dialog tracks settingsInvalidKeys (Set<string>), and
    // the Install button is disabled iff size > 0.
    //
    // We pin three behaviours:
    //   1. With a valid webhook + a valid (empty) settings doc,
    //      Install is enabled.
    //   2. After typing unparseable text into the settings
    //      textarea, the button transitions to disabled AND an
    //      inline warning surface appears (UX cue that the
    //      reason is the JSON, not the URL or anything else).
    //   3. Clicking the disabled button does NOT call the API
    //      \u2014 i.e. even if the click-handler was somehow reached
    //      (e.g. via keyboard or screen reader bypass), the
    //      install would still not fire because the SAVE GUARD
    //      is the button's disabled state, not a separate
    //      handler-side branch. We assert by attempting the
    //      click and verifying the mock was not called.
    renderDialog();
    const urlInput = screen.getByLabelText(/Webhook base URL/i);
    await userEvent.clear(urlInput);
    await userEvent.type(urlInput, "https://t.example.com");
    const installButton = screen.getByRole("button", {
      name: /Install extension/i,
    });
    // Initially enabled (valid URL, empty settings doc).
    expect(installButton).not.toBeDisabled();
    // Now corrupt the freeform JSON editor.
    const ta = screen.getByPlaceholderText(
      '{"api_key":"\u2026"}',
    ) as HTMLTextAreaElement;
    await userEvent.type(ta, '{{"unterminated');
    // Wait for the validity signal to propagate and the button
    // to reflect the invalid state.
    await waitFor(() => expect(installButton).toBeDisabled());
    // The inline warning surfaces so the user knows WHY Install
    // is greyed out (it might otherwise look like a bug \u2014 they
    // typed something, why can't they install?).
    expect(
      screen.getByText(/Resolve the JSON parse error/i),
    ).toBeInTheDocument();
    // Attempting the click is a no-op \u2014 disabled buttons don't
    // fire onClick from userEvent.click(), so the mock stays
    // untouched. The pre-fix code would have called the mock.
    await userEvent.click(installButton);
    expect(installMarketplaceExtension).not.toHaveBeenCalled();
  });

  it("re-enables Install once the JSON textarea parses cleanly again (round-5 BUG_0001)", async () => {
    // Companion to the previous test: once the user recovers
    // the document into a parseable shape, the Save button must
    // come back. The unmount-cleanup ref-pattern from round 4
    // (ANALYSIS_0004) handles the editor's tear-down, but
    // re-enabling on a buffer recovery is driven by the
    // validity-signal effect inside the editor \u2014 we pin that
    // round-trip works end-to-end (signal-invalid \u2192 disable \u2192
    // signal-valid \u2192 enable) without an unmount in between.
    renderDialog();
    const urlInput = screen.getByLabelText(/Webhook base URL/i);
    await userEvent.clear(urlInput);
    await userEvent.type(urlInput, "https://t.example.com");
    const installButton = screen.getByRole("button", {
      name: /Install extension/i,
    });
    const ta = screen.getByPlaceholderText(
      '{"api_key":"\u2026"}',
    ) as HTMLTextAreaElement;
    // Corrupt then recover.
    await userEvent.type(ta, '{{"unterminated');
    await waitFor(() => expect(installButton).toBeDisabled());
    await userEvent.clear(ta);
    await userEvent.type(ta, '{{"ok":1}');
    await waitFor(() => expect(installButton).not.toBeDisabled());
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
