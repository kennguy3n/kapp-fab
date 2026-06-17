import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { LocaleProvider } from "../lib/i18n";
import { makeKType, makeKRecord, makeWorkflowRun } from "../test/factories";

const getWorkflowRun = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    getWorkflowRun: (...a: unknown[]) => getWorkflowRun(...a),
  },
}));

import { RightPane } from "./RightPane";

const KTYPE = makeKType({ name: "crm.deal" });

function renderPane(
  props: Partial<React.ComponentProps<typeof RightPane>> = {},
) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const onClose = props.onClose ?? vi.fn();
  const utils = render(
    <LocaleProvider>
      <QueryClientProvider client={qc}>
        <RightPane
          ktype={KTYPE}
          record={makeKRecord({
            id: "rec-1",
            data: { title: "Acme deal", stage: "open", value: 5000 },
          })}
          onClose={onClose}
          {...props}
        />
      </QueryClientProvider>
    </LocaleProvider>,
  );
  return { ...utils, onClose };
}

describe("RightPane", () => {
  beforeEach(() => {
    getWorkflowRun.mockReset();
    getWorkflowRun.mockResolvedValue(null);
  });

  it("renders nothing when no record is selected", () => {
    const { container } = renderPane({ record: null });
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the record's field values on the details tab", async () => {
    renderPane();
    // Header prefers the record's `name`/`title`.
    expect(
      await screen.findByRole("heading", { name: /Acme deal/i }),
    ).toBeInTheDocument();
    // Field labels are humanized to Title Case; numbers are locale-formatted
    // (no `currency` sibling on this record, so it renders as a plain number).
    expect(screen.getByText("Title")).toBeInTheDocument();
    expect(screen.getByText("5,000")).toBeInTheDocument();
  });

  it("derives the workflow state heuristically when no run exists", async () => {
    getWorkflowRun.mockResolvedValue(null);
    renderPane();
    // "stage" === "open" is the initial state per the factory workflow.
    await waitFor(() =>
      expect(screen.getByText("Workflow state")).toBeInTheDocument(),
    );
    // The state token renders as a humanized Badge ("Open").
    expect(screen.getAllByText("Open").length).toBeGreaterThan(0);
  });

  it("renders the legal next actions and invokes onAction when clicked", async () => {
    const onAction = vi.fn();
    const user = userEvent.setup();
    renderPane({ onAction });

    // From "open" the workflow allows win -> won and lose -> lost.
    const winBtn = await screen.findByRole("button", { name: /win → won/i });
    await user.click(winBtn);
    expect(onAction).toHaveBeenCalledWith("win");
  });

  it("renders the workflow run history on the timeline tab", async () => {
    getWorkflowRun.mockResolvedValue(
      makeWorkflowRun({
        state: "won",
        history: [
          {
            from_state: "open",
            to_state: "won",
            action: "win",
            actor_id: "user-9",
            timestamp: "2024-01-02T00:00:00Z",
          },
        ],
      }),
    );
    const user = userEvent.setup();
    renderPane();

    // Tabs use the ARIA tablist pattern (role="tab" + aria-selected).
    await user.click(await screen.findByRole("tab", { name: /Timeline/i }));
    expect(await screen.findByText(/open → won/i)).toBeInTheDocument();
    expect(screen.getByText(/by user-9/i)).toBeInTheDocument();
  });

  it("closes when Escape is pressed", async () => {
    const user = userEvent.setup();
    const { onClose } = renderPane();
    await screen.findByRole("heading", { name: /Acme deal/i });
    await user.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("closes via the header close button", async () => {
    const user = userEvent.setup();
    const { onClose } = renderPane();
    await user.click(await screen.findByRole("button", { name: /Close/i }));
    expect(onClose).toHaveBeenCalled();
  });
});
