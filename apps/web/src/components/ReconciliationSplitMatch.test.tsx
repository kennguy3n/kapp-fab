import { describe, it, expect, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { BankFeedSuggestion } from "@kapp/client";
import { renderWithProviders, makeKRecord } from "../test-utils";
import {
  ReconciliationSplitMatch,
  type SplitAllocation,
} from "./ReconciliationSplitMatch";

function suggestion(id: string, je: string): BankFeedSuggestion {
  return {
    id,
    tenant_id: "t",
    transaction_id: "txn-1",
    journal_entry_id: je,
    confidence: 0.8,
    match_reason: "amount",
    status: "suggested",
    created_at: "",
  };
}

const TXN = makeKRecord({
  id: "txn-1",
  ktype: "finance.bank_transaction",
  data: {
    bank_account_id: "acct-1",
    value_date: "2024-02-01",
    description: "Split me",
    amount: 100,
    currency: "USD",
    status: "unreconciled",
  },
});

const SUGGESTIONS = [
  suggestion("s1", "je-1111"),
  suggestion("s2", "je-2222"),
];

function setup(onReconcile = vi.fn<(a: SplitAllocation[]) => void>()) {
  renderWithProviders(
    <ReconciliationSplitMatch
      txn={TXN}
      suggestions={SUGGESTIONS}
      currency="USD"
      pending={false}
      onReconcile={onReconcile}
    />,
  );
  return { onReconcile };
}

describe("ReconciliationSplitMatch", () => {
  it("keeps reconcile disabled until the split nets to zero", async () => {
    const user = userEvent.setup();
    setup();
    const reconcile = screen.getByRole("button", {
      name: /reconcile split/i,
    });
    expect(reconcile).toBeDisabled();

    const amounts = screen.getAllByLabelText(/allocated/i);
    await user.clear(amounts[0]);
    await user.type(amounts[0], "60");
    // 60 of 100 — still unbalanced.
    expect(reconcile).toBeDisabled();
  });

  it("enables reconcile when allocations balance and emits the chosen suggestions", async () => {
    const user = userEvent.setup();
    const { onReconcile } = setup();

    // First row already targets s1; allocate 60 to it.
    const firstAmount = screen.getAllByLabelText(/allocated/i)[0];
    await user.clear(firstAmount);
    await user.type(firstAmount, "60");

    // Add a second row, point it at s2, allocate the remaining 40.
    await user.click(screen.getByRole("button", { name: /add entry/i }));
    const selects = screen.getAllByLabelText("Ledger entry");
    await user.selectOptions(selects[1], "s2");
    const amounts = screen.getAllByLabelText(/allocated/i);
    await user.type(amounts[1], "40");

    const reconcile = screen.getByRole("button", { name: /reconcile split/i });
    expect(reconcile).toBeEnabled();

    await user.click(reconcile);
    expect(onReconcile).toHaveBeenCalledTimes(1);
    const allocations = onReconcile.mock.calls[0][0];
    expect(allocations.map((a) => a.suggestion.id)).toEqual(["s1", "s2"]);
    expect(allocations.map((a) => a.amount)).toEqual([60, 40]);
  });

  it("does not allow reconciling two rows against the same ledger entry", async () => {
    const user = userEvent.setup();
    setup();
    // Both rows point at s1 and together net to zero, but they collide.
    const firstAmount = screen.getAllByLabelText(/allocated/i)[0];
    await user.type(firstAmount, "60");
    await user.click(screen.getByRole("button", { name: /add entry/i }));
    const selects = screen.getAllByLabelText("Ledger entry");
    await user.selectOptions(selects[1], "s1");
    const amounts = screen.getAllByLabelText(/allocated/i);
    await user.type(amounts[1], "40");

    expect(
      screen.getByRole("button", { name: /reconcile split/i }),
    ).toBeDisabled();
    // Balanced-but-colliding must explain why it can't reconcile rather than
    // showing the misleading "ready to reconcile" success badge.
    expect(screen.getByText(/can only be used once/i)).toBeInTheDocument();
    expect(
      screen.queryByText(/ready to reconcile/i),
    ).not.toBeInTheDocument();
  });

  it("shows the running remaining amount", async () => {
    const user = userEvent.setup();
    setup();
    const firstAmount = screen.getAllByLabelText(/allocated/i)[0];
    await user.type(firstAmount, "25");
    // Remaining 75 should appear in the totals region.
    const remaining = screen.getByText(/remaining/i).closest("div");
    expect(remaining && within(remaining).getByText(/75/)).toBeTruthy();
  });
});
