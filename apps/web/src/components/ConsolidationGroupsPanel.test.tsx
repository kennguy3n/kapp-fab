import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test-utils";
import { ConsolidationGroupsPanel } from "./ConsolidationGroupsPanel";
import type { ConsolidationGroup } from "./ConsolidationApi";

const createGroup = vi.fn();

vi.mock("./ConsolidationApi", () => ({
  consolidationApi: {
    createGroup: (...args: unknown[]) => createGroup(...args),
  },
}));

const groupFixture: ConsolidationGroup = {
  id: "grp-1",
  name: "EMEA Roll-up",
  presentation_currency: "EUR",
  member_tenant_ids: ["t1", "t2"],
};

function setup(overrides: Partial<Parameters<typeof ConsolidationGroupsPanel>[0]> = {}) {
  const props = {
    groups: [] as ConsolidationGroup[],
    activeGroupId: null,
    onSelect: vi.fn(),
    onForget: vi.fn(),
    onCreated: vi.fn(),
    onTrack: vi.fn(),
    ...overrides,
  };
  renderWithProviders(<ConsolidationGroupsPanel {...props} />);
  return props;
}

describe("ConsolidationGroupsPanel", () => {
  beforeEach(() => {
    createGroup.mockReset();
  });

  it("shows the empty state when no groups are tracked", () => {
    setup();
    expect(
      screen.getByText(/No groups tracked in this browser/i),
    ).toBeInTheDocument();
  });

  it("creates a group with parsed members and calls onCreated", async () => {
    createGroup.mockResolvedValueOnce(groupFixture);
    const props = setup();
    const user = userEvent.setup();

    await user.type(screen.getByLabelText(/^Name$/i), "EMEA Roll-up");
    const members = screen.getByLabelText(/Member tenant IDs/i);
    await user.type(members, "t1, t2");

    await user.click(screen.getByRole("button", { name: /^Create group$/i }));

    await waitFor(() => expect(createGroup).toHaveBeenCalledTimes(1));
    const arg = createGroup.mock.calls[0]![0] as Record<string, unknown>;
    expect(arg.member_tenant_ids).toEqual(["t1", "t2"]);
    expect(arg.name).toBe("EMEA Roll-up");
    await waitFor(() => expect(props.onCreated).toHaveBeenCalledWith(groupFixture));
  });

  it("includes elimination pairs that are fully filled in", async () => {
    createGroup.mockResolvedValueOnce(groupFixture);
    setup();
    const user = userEvent.setup();

    await user.type(screen.getByLabelText(/^Name$/i), "G");
    await user.click(screen.getByRole("button", { name: /Add pair/i }));
    await user.type(screen.getByPlaceholderText("From tenant"), "t1");
    await user.type(screen.getByPlaceholderText("To tenant"), "t2");
    await user.type(screen.getByPlaceholderText("Account code"), "1500");

    await user.click(screen.getByRole("button", { name: /^Create group$/i }));

    await waitFor(() => expect(createGroup).toHaveBeenCalledTimes(1));
    const arg = createGroup.mock.calls[0]![0] as {
      elimination_pairs?: Array<Record<string, string>>;
    };
    expect(arg.elimination_pairs).toEqual([
      { from_tenant: "t1", to_tenant: "t2", account_code: "1500" },
    ]);
  });

  it("renders tracked groups and fires select / forget", async () => {
    const props = setup({ groups: [groupFixture], activeGroupId: "grp-1" });
    const user = userEvent.setup();
    expect(screen.getByText("EMEA Roll-up")).toBeInTheDocument();
    expect(screen.getByText("Selected")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /^Select$/i }));
    expect(props.onSelect).toHaveBeenCalledWith("grp-1");

    await user.click(screen.getByRole("button", { name: /^Forget$/i }));
    expect(props.onForget).toHaveBeenCalledWith("grp-1");
  });

  it("tracks an existing group by id", async () => {
    const props = setup();
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(/Track existing group by ID/i), "grp-9");
    await user.click(screen.getByRole("button", { name: /^Track$/i }));
    expect(props.onTrack).toHaveBeenCalledWith("grp-9");
  });
});
