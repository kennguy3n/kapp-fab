import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { makeInsightsShare } from "../../test/factories";

const listInsightsQueryShares = vi.fn();
const shareInsightsQuery = vi.fn();
const deleteInsightsQueryShare = vi.fn();

vi.mock("../../lib/api", () => ({
  api: {
    listInsightsQueryShares: (...a: unknown[]) => listInsightsQueryShares(...a),
    shareInsightsQuery: (...a: unknown[]) => shareInsightsQuery(...a),
    deleteInsightsQueryShare: (...a: unknown[]) => deleteInsightsQueryShare(...a),
    // dashboard variants exist on the client but the "query" resource
    // path is the one under test here.
    listInsightsDashboardShares: vi.fn(),
    shareInsightsDashboard: vi.fn(),
    deleteInsightsDashboardShare: vi.fn(),
  },
}));

import { ShareModal } from "./ShareModal";

function renderModal(onClose = vi.fn()) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const utils = render(
    <QueryClientProvider client={qc}>
      <ShareModal
        resource="query"
        resourceId="q-1"
        resourceName="Pipeline by stage"
        onClose={onClose}
      />
    </QueryClientProvider>,
  );
  return { ...utils, onClose };
}

describe("ShareModal", () => {
  beforeEach(() => {
    listInsightsQueryShares.mockReset();
    shareInsightsQuery.mockReset();
    deleteInsightsQueryShare.mockReset();
  });

  it("lists existing shares for the resource", async () => {
    listInsightsQueryShares.mockResolvedValue({
      shares: [
        makeInsightsShare({ grantee_type: "role", grantee: "analyst", permission: "view" }),
        makeInsightsShare({ grantee_type: "user", grantee: "u-2", permission: "edit" }),
      ],
    });
    renderModal();

    // Each grantee row renders as "<type>: <grantee> (<permission>)"
    // across several text nodes, so assert on the dialog's normalized
    // text content rather than a single node.
    const dialog = screen.getByRole("dialog");
    await waitFor(() =>
      expect(dialog).toHaveTextContent(/role:\s*analyst\s*\(view\)/i),
    );
    expect(dialog).toHaveTextContent(/user:\s*u-2\s*\(edit\)/i);
  });

  it("shows the empty state when nothing is shared yet", async () => {
    listInsightsQueryShares.mockResolvedValue({ shares: [] });
    renderModal();
    expect(
      await screen.findByText(/Not shared with anyone yet\./i),
    ).toBeInTheDocument();
  });

  it("blocks submit and shows an inline error when the grantee is empty", async () => {
    listInsightsQueryShares.mockResolvedValue({ shares: [] });
    const user = userEvent.setup();
    renderModal();
    await screen.findByText(/Not shared with anyone yet\./i);

    await user.click(screen.getByRole("button", { name: /^Share$/i }));
    expect(await screen.findByText(/grantee required/i)).toBeInTheDocument();
    expect(shareInsightsQuery).not.toHaveBeenCalled();
  });

  it("creates a share via api.shareInsightsQuery with the selected fields", async () => {
    listInsightsQueryShares.mockResolvedValue({ shares: [] });
    shareInsightsQuery.mockResolvedValue(makeInsightsShare());
    const user = userEvent.setup();
    renderModal();
    await screen.findByText(/Not shared with anyone yet\./i);

    await user.type(screen.getByPlaceholderText(/user uuid/i), "user-123");
    await user.click(screen.getByRole("button", { name: /^Share$/i }));

    await waitFor(() => expect(shareInsightsQuery).toHaveBeenCalledTimes(1));
    const [id, input] = shareInsightsQuery.mock.calls[0]!;
    expect(id).toBe("q-1");
    expect(input).toMatchObject({
      grantee_type: "user",
      grantee: "user-123",
      permission: "view",
    });
  });

  it("revokes an existing share via api.deleteInsightsQueryShare", async () => {
    listInsightsQueryShares.mockResolvedValue({
      shares: [makeInsightsShare({ id: "share-9", grantee: "analyst", grantee_type: "role" })],
    });
    deleteInsightsQueryShare.mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderModal();

    await user.click(await screen.findByRole("button", { name: /Revoke/i }));
    await waitFor(() => expect(deleteInsightsQueryShare).toHaveBeenCalledTimes(1));
    const [id, shareId] = deleteInsightsQueryShare.mock.calls[0]!;
    expect(id).toBe("q-1");
    expect(shareId).toBe("share-9");
  });

  it("closes via the close button and via the backdrop", async () => {
    listInsightsQueryShares.mockResolvedValue({ shares: [] });
    const user = userEvent.setup();
    const { onClose } = renderModal();
    await screen.findByText(/Not shared with anyone yet\./i);

    await user.click(screen.getByRole("button", { name: /close/i }));
    expect(onClose).toHaveBeenCalledTimes(1);

    // Clicking the dialog backdrop also closes; clicking inside the
    // panel must NOT (stopPropagation).
    await user.click(screen.getByRole("heading", { name: /Share query/i }));
    expect(onClose).toHaveBeenCalledTimes(1);
    await user.click(screen.getByRole("dialog"));
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
