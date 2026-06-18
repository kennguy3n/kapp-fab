import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listRecords = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
  },
}));

import { OrgChartPage } from "./OrgChartPage";
import { renderWithProviders, makeKRecord } from "../test-utils";

// A small org with two reporting lines under one root, plus a second
// root branch, so search behaviour can be checked for matched roots,
// matched managers, and unrelated branches.
//   Casey Root (CEO)
//     └ Morgan Lead (VP)
//         └ Riley Report (IC)
//   Dana Other (root)
//     └ Sam Side (IC)
const EMPLOYEES = [
  makeKRecord({ id: "ceo", ktype: "hr.employee", data: { name: "Casey Root", designation: "CEO" } }),
  makeKRecord({ id: "vp", ktype: "hr.employee", data: { name: "Morgan Lead", designation: "VP", reporting_to: "ceo" } }),
  makeKRecord({ id: "ic", ktype: "hr.employee", data: { name: "Riley Report", designation: "Engineer", reporting_to: "vp" } }),
  makeKRecord({ id: "other", ktype: "hr.employee", data: { name: "Dana Other", designation: "COO" } }),
  makeKRecord({ id: "side", ktype: "hr.employee", data: { name: "Sam Side", designation: "Analyst", reporting_to: "other" } }),
];

describe("OrgChartPage search", () => {
  beforeEach(() => {
    listRecords.mockReset();
    listRecords.mockResolvedValue(EMPLOYEES);
  });

  it("keeps a matched root's reports visible (root has no ancestors)", async () => {
    const user = userEvent.setup();
    renderWithProviders(<OrgChartPage />);
    await screen.findByText("Casey Root");

    await user.type(screen.getByPlaceholderText(/Search name, role, team/i), "Casey");

    // The matched root itself must stay expanded so its direct report shows.
    expect(await screen.findByText("Morgan Lead")).toBeInTheDocument();
  });

  it("keeps a matched manager's reports visible", async () => {
    const user = userEvent.setup();
    renderWithProviders(<OrgChartPage />);
    await screen.findByText("Casey Root");

    await user.type(screen.getByPlaceholderText(/Search name, role, team/i), "Morgan");

    expect(await screen.findByText("Riley Report")).toBeInTheDocument();
  });

  it("collapses branches that don't lead to a match", async () => {
    const user = userEvent.setup();
    renderWithProviders(<OrgChartPage />);
    await screen.findByText("Casey Root");

    await user.type(screen.getByPlaceholderText(/Search name, role, team/i), "Riley");

    // Path to the match stays open...
    expect(await screen.findByText("Riley Report")).toBeInTheDocument();
    // ...while the unrelated second branch stays collapsed.
    expect(screen.queryByText("Sam Side")).not.toBeInTheDocument();
  });
});
