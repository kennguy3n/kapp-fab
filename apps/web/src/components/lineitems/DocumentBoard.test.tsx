import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders, makeKRecord } from "../../test-utils";
import { DocumentBoard, type DocumentBoardProps } from "./DocumentBoard";

const STAGES = ["draft", "confirmed", "fulfilled", "cancelled"];

function setup(overrides: Partial<DocumentBoardProps> = {}) {
  const props: DocumentBoardProps = {
    eyebrow: "Sales",
    title: "Sales Orders",
    description: "Track every order from draft to fulfilment.",
    newLabel: "New order",
    onNew: vi.fn(),
    stages: STAGES,
    records: [],
    statusOf: (r) => String((r.data as { status?: string }).status ?? "draft"),
    isLoading: false,
    isError: false,
    onRetry: vi.fn(),
    onMove: vi.fn(),
    onCardClick: vi.fn(),
    renderCard: (r) => <span>{String((r.data as { order_number?: string }).order_number)}</span>,
    emptyTitle: "No orders yet",
    emptyDescription: "Create your first order to get started.",
    ...overrides,
  };
  renderWithProviders(<DocumentBoard {...props} />);
  return props;
}

describe("DocumentBoard", () => {
  it("renders skeleton columns while loading", () => {
    setup({ isLoading: true });
    expect(screen.getByText("Sales Orders")).toBeInTheDocument();
    expect(document.body.querySelectorAll(".animate-pulse").length).toBeGreaterThan(0);
  });

  it("renders an error with a working retry", async () => {
    const user = userEvent.setup();
    const props = setup({ isError: true, error: new Error("boom") });
    expect(screen.getByRole("alert")).toHaveTextContent("boom");
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(props.onRetry).toHaveBeenCalledTimes(1);
  });

  it("renders a teaching empty state whose action creates a document", async () => {
    const user = userEvent.setup();
    const props = setup({ records: [] });
    expect(screen.getByText("No orders yet")).toBeInTheDocument();
    // Two "New order" actions exist (header + empty state); click the empty-state one.
    const actions = screen.getAllByRole("button", { name: "New order" });
    await user.click(actions[actions.length - 1]);
    expect(props.onNew).toHaveBeenCalled();
  });

  it("renders cards by stage and fires onCardClick", async () => {
    const user = userEvent.setup();
    const records = [
      makeKRecord({ id: "so-1", data: { status: "draft", order_number: "SO-1001" } }),
      makeKRecord({ id: "so-2", data: { status: "confirmed", order_number: "SO-1002" } }),
    ];
    const props = setup({ records });
    expect(screen.getByText("SO-1001")).toBeInTheDocument();
    expect(screen.getByText("SO-1002")).toBeInTheDocument();

    await user.click(screen.getByText("SO-1001"));
    expect(props.onCardClick).toHaveBeenCalledWith(records[0]);
  });

  it("surfaces records in unrecognized stages rather than hiding them", () => {
    const records = [makeKRecord({ id: "so-x", data: { status: "on_hold", order_number: "SO-9" } })];
    setup({ records });
    expect(screen.getByText(/unrecognized/i)).toBeInTheDocument();
    expect(screen.getByText("SO-9")).toBeInTheDocument();
  });
});
