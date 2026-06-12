import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test-utils";
import { ConsolidationTrialBalance } from "./ConsolidationTrialBalance";
import type { ConsolidatedTrialBalance } from "./ConsolidationApi";

const fixture: ConsolidatedTrialBalance = {
  group_id: "g1",
  as_of: "2025-03-31T00:00:00Z",
  presentation_currency: "USD",
  total_debit: "3000",
  total_credit: "3000",
  residual: "0",
  cta: "120",
  rows: [
    {
      account_code: "1000",
      account_name: "Cash",
      type: "asset",
      debit: "2000",
      credit: "0",
      balance: "2000",
      contributions: [
        { tenant_id: "tenant-alpha", debit: "1200", credit: "0" },
        { tenant_id: "tenant-beta", debit: "800", credit: "0" },
      ],
    },
    {
      account_code: "3900",
      account_name: "Translation adjustment",
      type: "equity",
      debit: "0",
      credit: "120",
      balance: "-120",
      contributions: [],
    },
  ],
  eliminated: [
    { account_code: "1500", debit: "500", credit: "500", balance: "0" },
  ],
};

describe("ConsolidationTrialBalance", () => {
  it("renders CTA, residual and a balanced badge", () => {
    renderWithProviders(<ConsolidationTrialBalance result={fixture} />);
    expect(screen.getByText("Balanced")).toBeInTheDocument();
    // CTA stat value.
    expect(screen.getByText("120")).toBeInTheDocument();
  });

  it("renders one column header per contributing entity", () => {
    renderWithProviders(<ConsolidationTrialBalance result={fixture} />);
    // Tenant ids are truncated to 8 chars + ellipsis in the header.
    expect(screen.getByText("tenant-a…")).toBeInTheDocument();
    expect(screen.getByText("tenant-b…")).toBeInTheDocument();
  });

  it("flags the CTA row with a badge", () => {
    renderWithProviders(
      <ConsolidationTrialBalance result={fixture} ctaAccountCode="3900" />,
    );
    const row = screen.getByTestId("tb-row-3900");
    expect(within(row).getByText("CTA")).toBeInTheDocument();
  });

  it("drills down to per-entity contributions on click", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ConsolidationTrialBalance result={fixture} />);
    expect(screen.queryByTestId("tb-drill-1000")).not.toBeInTheDocument();

    await user.click(screen.getByTestId("tb-row-1000"));

    const drill = screen.getByTestId("tb-drill-1000");
    expect(
      within(drill).getByText("Per-entity contributions"),
    ).toBeInTheDocument();
    // Both contributing entities + their debit amounts appear.
    expect(within(drill).getByText("1200")).toBeInTheDocument();
    expect(within(drill).getByText("800")).toBeInTheDocument();
  });

  it("renders the eliminated intercompany section", () => {
    renderWithProviders(<ConsolidationTrialBalance result={fixture} />);
    expect(screen.getByText(/Eliminated \(intercompany\)/i)).toBeInTheDocument();
    expect(screen.getByText("1500")).toBeInTheDocument();
  });

  it("marks an unbalanced trial balance", () => {
    renderWithProviders(
      <ConsolidationTrialBalance
        result={{ ...fixture, residual: "5" }}
      />,
    );
    expect(screen.getByText("Unbalanced")).toBeInTheDocument();
  });
});
