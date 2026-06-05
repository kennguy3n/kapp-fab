import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listWorkOrders = vi.fn();
const listJobCards = vi.fn();
const startJobCard = vi.fn();
const completeJobCard = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listWorkOrders: (...a: unknown[]) => listWorkOrders(...a),
    listJobCards: (...a: unknown[]) => listJobCards(...a),
    startJobCard: (...a: unknown[]) => startJobCard(...a),
    completeJobCard: (...a: unknown[]) => completeJobCard(...a),
  },
}));

import { JobCardPage } from "./JobCardPage";
import { renderWithProviders } from "../test-utils";

const WO = {
  id: "wo-1111111122222222",
  item_id: "item-1",
  warehouse_id: "wh-1",
  status: "released",
  planned_qty: "10",
  actual_qty: "",
};

function card(overrides: Record<string, unknown> = {}) {
  return {
    id: "card-1",
    work_order_id: WO.id,
    routing_operation_seq: 1,
    work_center_id: "wc-1",
    status: "pending",
    qty_produced: "0",
    qty_rejected: "0",
    actual_start: null,
    actual_end: null,
    ...overrides,
  };
}

async function selectWorkOrder(user: ReturnType<typeof userEvent.setup>) {
  renderWithProviders(<JobCardPage />);
  // The selector is limited to released / in-progress orders; wait for
  // the work-order query to populate the option before choosing it.
  await screen.findByRole("option", { name: /released — qty 10/ });
  await user.selectOptions(screen.getByLabelText("Work order"), WO.id);
}

describe("JobCardPage", () => {
  beforeEach(() => {
    [listWorkOrders, listJobCards, startJobCard, completeJobCard].forEach((m) =>
      m.mockReset(),
    );
    listWorkOrders.mockImplementation((status: string) =>
      status === "released" ? Promise.resolve([WO]) : Promise.resolve([]),
    );
  });

  it("lists the job cards for the selected work order", async () => {
    listJobCards.mockResolvedValue([
      card({ id: "card-1", routing_operation_seq: 1 }),
      card({ id: "card-2", routing_operation_seq: 2 }),
    ]);
    const user = userEvent.setup();
    await selectWorkOrder(user);

    await waitFor(() => expect(listJobCards).toHaveBeenCalledWith(WO.id));
    expect(await screen.findAllByRole("button", { name: "Start" })).toHaveLength(
      2,
    );
  });

  it("disables only the clicked card's Start button while its mutation is in flight", async () => {
    listJobCards.mockResolvedValue([
      card({ id: "card-1", routing_operation_seq: 1 }),
      card({ id: "card-2", routing_operation_seq: 2 }),
    ]);
    // Never resolves, so the mutation stays pending and we can observe
    // which buttons are disabled mid-flight.
    startJobCard.mockReturnValue(new Promise(() => {}));

    const user = userEvent.setup();
    await selectWorkOrder(user);

    const startButtons = await screen.findAllByRole("button", { name: "Start" });
    expect(startButtons).toHaveLength(2);

    await user.click(startButtons[0]);

    // The clicked card's Start button is disabled; the other card's
    // Start button stays interactive (no whole-table freeze).
    await waitFor(() => expect(startButtons[0]).toBeDisabled());
    expect(startButtons[1]).toBeEnabled();
  });

  it("keeps the first card disabled when a second card's mutation is fired while the first is still in flight", async () => {
    listJobCards.mockResolvedValue([
      card({ id: "card-1", routing_operation_seq: 1 }),
      card({ id: "card-2", routing_operation_seq: 2 }),
    ]);
    // Both requests hang, so both mutations stay in flight at once.
    startJobCard.mockReturnValue(new Promise(() => {}));

    const user = userEvent.setup();
    await selectWorkOrder(user);

    const startButtons = await screen.findAllByRole("button", { name: "Start" });
    expect(startButtons).toHaveLength(2);

    await user.click(startButtons[0]);
    await waitFor(() => expect(startButtons[0]).toBeDisabled());

    // Firing the second card's mutation must NOT re-enable the first.
    // (A single shared useMutation would, since its `variables` would now
    // point at card-2; per-row mutations keep each card's state isolated.)
    await user.click(startButtons[1]);
    await waitFor(() => expect(startButtons[1]).toBeDisabled());
    expect(startButtons[0]).toBeDisabled();
    expect(startJobCard).toHaveBeenCalledTimes(2);
  });

  it("disables only the clicked card's Complete button while its mutation is in flight", async () => {
    listJobCards.mockResolvedValue([
      card({ id: "card-1", routing_operation_seq: 1, status: "in_progress" }),
      card({ id: "card-2", routing_operation_seq: 2, status: "in_progress" }),
    ]);
    completeJobCard.mockReturnValue(new Promise(() => {}));

    const user = userEvent.setup();
    await selectWorkOrder(user);

    const completeButtons = await screen.findAllByRole("button", {
      name: "Complete",
    });
    expect(completeButtons).toHaveLength(2);

    await user.click(completeButtons[0]);

    await waitFor(() => expect(completeButtons[0]).toBeDisabled());
    expect(completeButtons[1]).toBeEnabled();
  });

  it("shows both Start and Complete on a pending card (one-step completion is legal)", async () => {
    listJobCards.mockResolvedValue([card({ id: "card-1", status: "pending" })]);
    const user = userEvent.setup();
    await selectWorkOrder(user);

    const rows = await screen.findAllByRole("row");
    // header row + one card row
    const cardRow = rows[rows.length - 1];
    expect(within(cardRow).getByRole("button", { name: "Start" })).toBeInTheDocument();
    expect(
      within(cardRow).getByRole("button", { name: "Complete" }),
    ).toBeInTheDocument();
  });
});
