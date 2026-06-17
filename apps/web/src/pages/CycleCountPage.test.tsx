import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listCycleCountSessions = vi.fn();
const getCycleCountSession = vi.fn();
const listInventoryItems = vi.fn();
const listInventoryWarehouses = vi.fn();
const createCycleCountSession = vi.fn();
const seedCycleCountSession = vi.fn();
const updateCycleCountSession = vi.fn();
const postCycleCountSession = vi.fn();
const upsertCycleCountLine = vi.fn();
const deleteCycleCountLine = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listCycleCountSessions: (...a: unknown[]) => listCycleCountSessions(...a),
    getCycleCountSession: (...a: unknown[]) => getCycleCountSession(...a),
    listInventoryItems: (...a: unknown[]) => listInventoryItems(...a),
    listInventoryWarehouses: (...a: unknown[]) => listInventoryWarehouses(...a),
    createCycleCountSession: (...a: unknown[]) => createCycleCountSession(...a),
    seedCycleCountSession: (...a: unknown[]) => seedCycleCountSession(...a),
    updateCycleCountSession: (...a: unknown[]) => updateCycleCountSession(...a),
    postCycleCountSession: (...a: unknown[]) => postCycleCountSession(...a),
    upsertCycleCountLine: (...a: unknown[]) => upsertCycleCountLine(...a),
    deleteCycleCountLine: (...a: unknown[]) => deleteCycleCountLine(...a),
  },
}));

import { CycleCountPage } from "./CycleCountPage";
import { renderWithProviders } from "../test-utils";

const WAREHOUSE = { id: "wh-00000001-aaaa", code: "MAIN", name: "Main DC" };
const ITEM = { id: "item-1", sku: "SKU-1", name: "Widget" };

function session(overrides: Record<string, unknown> = {}) {
  return {
    id: "ccs-1",
    code: "CC-2024-01",
    description: "Q1 spot check",
    status: "draft",
    warehouse_id: "wh-00000001-aaaa",
    ...overrides,
  };
}

describe("CycleCountPage", () => {
  beforeEach(() => {
    [
      listCycleCountSessions, getCycleCountSession, listInventoryItems,
      listInventoryWarehouses, createCycleCountSession, seedCycleCountSession,
      updateCycleCountSession, postCycleCountSession, upsertCycleCountLine,
      deleteCycleCountLine,
    ].forEach((m) => m.mockReset());
    listCycleCountSessions.mockResolvedValue([session()]);
    listInventoryItems.mockResolvedValue([ITEM]);
    listInventoryWarehouses.mockResolvedValue([WAREHOUSE]);
    getCycleCountSession.mockResolvedValue({ session: session(), lines: [] });
  });

  it("lists sessions and opens a draft session's detail panel", async () => {
    const user = userEvent.setup();
    renderWithProviders(<CycleCountPage />);

    await user.click(await screen.findByRole("button", { name: /CC-2024-01/ }));
    // Detail panel header + status line + the draft-only action.
    expect(await screen.findByRole("heading", { name: "CC-2024-01" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start counting" })).toBeInTheDocument();
    expect(getCycleCountSession).toHaveBeenCalledWith("ccs-1");
  });

  it("shows the empty state with no sessions", async () => {
    listCycleCountSessions.mockResolvedValue([]);
    renderWithProviders(<CycleCountPage />);
    expect(await screen.findByText(/No sessions yet/)).toBeInTheDocument();
  });

  it("surfaces a list load error", async () => {
    listCycleCountSessions.mockRejectedValue(new Error("boom"));
    renderWithProviders(<CycleCountPage />);
    expect(
      await screen.findByText(/Couldn't load sessions: boom/),
    ).toBeInTheDocument();
  });

  it("refetches with the chosen status filter", async () => {
    const user = userEvent.setup();
    renderWithProviders(<CycleCountPage />);
    await screen.findByRole("button", { name: /CC-2024-01/ });
    // The status filter is the first combobox (the warehouse picker in
    // the new-session builder is the second).
    await user.selectOptions(screen.getAllByRole("combobox")[0], "posted");
    await waitFor(() =>
      expect(listCycleCountSessions).toHaveBeenCalledWith({ status: "posted" }),
    );
  });

  it("creates a draft session from the builder", async () => {
    createCycleCountSession.mockResolvedValue(session({ id: "ccs-new" }));
    getCycleCountSession.mockResolvedValue({ session: session({ id: "ccs-new" }), lines: [] });
    const user = userEvent.setup();
    renderWithProviders(<CycleCountPage />);

    const create = await screen.findByRole("button", { name: "Create draft session" });
    expect(create).toBeDisabled();
    await user.type(screen.getByLabelText(/^Code/), "CC-NEW");
    await user.selectOptions(screen.getByLabelText(/^Warehouse/), "wh-00000001-aaaa");
    expect(create).toBeEnabled();
    await user.click(create);

    await waitFor(() =>
      expect(createCycleCountSession).toHaveBeenCalledWith(
        expect.objectContaining({ code: "CC-NEW", warehouse_id: "wh-00000001-aaaa" }),
      ),
    );
  });

  it("seeds expected quantities from stock on a draft session", async () => {
    seedCycleCountSession.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<CycleCountPage />);
    await user.click(await screen.findByRole("button", { name: /CC-2024-01/ }));
    await user.click(await screen.findByRole("button", { name: "Seed from stock" }));
    await waitFor(() => expect(seedCycleCountSession).toHaveBeenCalledWith("ccs-1"));
  });

  it("posts variance moves from a reconciled session after confirmation", async () => {
    getCycleCountSession.mockResolvedValue({ session: session({ status: "reconciled" }), lines: [] });
    postCycleCountSession.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<CycleCountPage />);

    await user.click(await screen.findByRole("button", { name: /CC-2024-01/ }));
    await user.click(await screen.findByRole("button", { name: "Post variance moves" }));
    // The action now routes through the ConfirmDialog rather than
    // window.confirm; confirm it in the modal.
    await user.click(await screen.findByRole("button", { name: "Post" }));
    await waitFor(() => expect(postCycleCountSession).toHaveBeenCalledWith("ccs-1"));
  });
});
