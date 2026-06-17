import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";

const capacityPlan = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    capacityPlan: (...a: unknown[]) => capacityPlan(...a),
  },
}));

import { CapacityPlanningPage } from "./CapacityPlanningPage";
import { renderWithProviders } from "../test-utils";

function day(
  date: string,
  scheduled: string,
  available: string,
  utilization: string,
  overloaded = false,
) {
  return {
    date,
    scheduled_minutes: scheduled,
    available_minutes: available,
    utilization_percent: utilization,
    overloaded,
  };
}

const PLAN = {
  start: "2026-06-16",
  end: "2026-06-17",
  rows: [
    {
      work_center_id: "wc-1",
      work_center_name: "CNC mill",
      status: "active",
      days: [
        day("2026-06-16", "240", "480", "50"),
        day("2026-06-17", "600", "480", "125", true),
      ],
    },
  ],
};

describe("CapacityPlanningPage", () => {
  beforeEach(() => {
    capacityPlan.mockReset();
    capacityPlan.mockResolvedValue(PLAN);
  });

  it("renders humanized day headers, utilisation and an overload callout", async () => {
    renderWithProviders(<CapacityPlanningPage />);
    expect(
      await screen.findByRole("cell", { name: "CNC mill" }),
    ).toBeInTheDocument();
    // Day header humanized (no raw YYYY-MM-DD).
    expect(screen.getAllByText("Jun 16").length).toBeGreaterThan(0);
    expect(screen.queryByText("2026-06-16")).not.toBeInTheDocument();
    // Utilisation rendered as a percentage.
    expect(screen.getAllByText("125%").length).toBeGreaterThan(0);
    // Overload surfaced as an actionable callout.
    expect(screen.getByText(/1 overloaded slot/i)).toBeInTheDocument();
  });

  it("shows an empty state when nothing is scheduled", async () => {
    capacityPlan.mockResolvedValue({
      start: "2026-06-16",
      end: "2026-06-17",
      rows: [],
    });
    renderWithProviders(<CapacityPlanningPage />);
    expect(await screen.findByText(/Nothing scheduled/i)).toBeInTheDocument();
  });

  it("surfaces a load error with retry", async () => {
    capacityPlan.mockRejectedValue(new Error("kaboom"));
    renderWithProviders(<CapacityPlanningPage />);
    expect(await screen.findByText(/kaboom/)).toBeInTheDocument();
  });
});
