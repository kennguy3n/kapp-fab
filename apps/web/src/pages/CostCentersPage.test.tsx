import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders, makeKRecord } from "../test-utils";

const listRecords = vi.fn();
const createRecord = vi.fn();
const updateRecord = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...args: unknown[]) => listRecords(...args),
    createRecord: (...args: unknown[]) => createRecord(...args),
    updateRecord: (...args: unknown[]) => updateRecord(...args),
  },
}));

import { CostCentersPage } from "./CostCentersPage";

const centres = [
  makeKRecord({
    id: "cc1",
    ktype: "finance.cost_center",
    data: { code: "SALES", name: "Sales team", active: true },
  }),
  makeKRecord({
    id: "cc2",
    ktype: "finance.cost_center",
    data: { code: "OPS", name: "Operations", active: false },
  }),
];

describe("CostCentersPage", () => {
  beforeEach(() => {
    listRecords.mockReset();
    createRecord.mockReset();
    updateRecord.mockReset();
    createRecord.mockResolvedValue(centres[0]);
  });

  it("renders the cost-centre tree with status badges", async () => {
    listRecords.mockResolvedValue(centres);
    renderWithProviders(<CostCentersPage />);

    expect(await screen.findByText("Sales team")).toBeInTheDocument();
    expect(screen.getByText("Operations")).toBeInTheDocument();
    expect(screen.getByText("SALES")).toBeInTheDocument();
    expect(screen.getByText("OPS")).toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(screen.getByText("Inactive")).toBeInTheDocument();
  });

  it("creates a new cost centre from the form", async () => {
    listRecords.mockResolvedValue([]);
    renderWithProviders(<CostCentersPage />);
    await screen.findByText(/No cost centres yet/i);

    const user = userEvent.setup();
    await user.type(screen.getByPlaceholderText("SALES"), "MKT");
    await user.type(screen.getByPlaceholderText("Sales team"), "Marketing");
    await user.click(screen.getByRole("button", { name: /add cost centre/i }));

    expect(createRecord).toHaveBeenCalledWith("finance.cost_center", {
      code: "MKT",
      name: "Marketing",
      parent_code: undefined,
      active: true,
    });
  });

  it("blocks submission and shows inline validation when required fields are empty", async () => {
    listRecords.mockResolvedValue([]);
    renderWithProviders(<CostCentersPage />);
    await screen.findByText(/No cost centres yet/i);

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /add cost centre/i }));

    expect(screen.getByText("A code is required.")).toBeInTheDocument();
    expect(screen.getByText("A name is required.")).toBeInTheDocument();
    expect(createRecord).not.toHaveBeenCalled();
  });

  it("renders an error surface with retry when the query fails", async () => {
    listRecords.mockRejectedValue(new Error("centres down"));
    renderWithProviders(<CostCentersPage />);

    expect(
      await screen.findByText(/Couldn't load cost centres/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });
});
