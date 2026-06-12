import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listRecords = vi.fn();
const updateRecord = vi.fn();
const createRecord = vi.fn();
const listBankFeedSuggestions = vi.fn();
const listBankFeedRules = vi.fn();
const acceptBankFeedSuggestion = vi.fn();
const rejectBankFeedSuggestion = vi.fn();
const listExchangeRates = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
    updateRecord: (...a: unknown[]) => updateRecord(...a),
    createRecord: (...a: unknown[]) => createRecord(...a),
    listBankFeedSuggestions: (...a: unknown[]) => listBankFeedSuggestions(...a),
    listBankFeedRules: (...a: unknown[]) => listBankFeedRules(...a),
    acceptBankFeedSuggestion: (...a: unknown[]) => acceptBankFeedSuggestion(...a),
    rejectBankFeedSuggestion: (...a: unknown[]) => rejectBankFeedSuggestion(...a),
    listExchangeRates: (...a: unknown[]) => listExchangeRates(...a),
  },
}));

import { BankReconciliationPage } from "./BankReconciliationPage";
import { renderWithProviders, makeKRecord } from "../test-utils";

// jsdom's File/Blob does not implement async text(), which the CSV
// uploader relies on. Build a File whose text() resolves to the given
// content so the parser runs exactly as it does in the browser.
function csvFile(name: string, content: string): File {
  const file = new File([content], name, { type: "text/csv" });
  Object.defineProperty(file, "text", { value: () => Promise.resolve(content) });
  return file;
}

const ACCOUNT = makeKRecord({
  id: "acct-1",
  ktype: "finance.bank_account",
  data: { name: "Operating USD", currency: "USD", account_number: "****1234" },
});
const ACCOUNT_2 = makeKRecord({
  id: "acct-2",
  ktype: "finance.bank_account",
  data: { name: "Savings USD", currency: "USD", account_number: "****9999" },
});
const TXN_UNRECONCILED = makeKRecord({
  id: "txn-1",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-1", value_date: "2024-02-01", description: "ACME invoice", amount: 250, currency: "USD", status: "unreconciled" },
});
const TXN_OTHER_ACCOUNT = makeKRecord({
  id: "txn-2",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-zz", value_date: "2024-02-02", description: "Other acct", amount: 10, currency: "USD", status: "unreconciled" },
});
const TXN_TRANSFER_OUT = makeKRecord({
  id: "txn-out",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-1", value_date: "2024-02-03", description: "Transfer to savings", amount: -500, currency: "USD", status: "transfer" },
});
const TXN_TRANSFER_IN = makeKRecord({
  id: "txn-in",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-2", value_date: "2024-02-03", description: "Transfer from operating", amount: 500, currency: "USD", status: "transfer" },
});

const SUGGESTION_BEST = {
  id: "sug-best",
  tenant_id: "t",
  transaction_id: "txn-1",
  journal_entry_id: "je-aaaa1111-0000-0000-0000-000000000000",
  confidence: 0.95,
  match_reason: "exact amount, same-day",
  status: "suggested",
  created_at: "",
};
const SUGGESTION_ALT = {
  id: "sug-alt",
  tenant_id: "t",
  transaction_id: "txn-1",
  journal_entry_id: "je-bbbb2222-0000-0000-0000-000000000000",
  confidence: 0.55,
  match_reason: "amount within tolerance",
  status: "suggested",
  created_at: "",
};

const RULE = {
  id: "rule-1",
  priority: 10,
  condition_type: "description_contains",
  condition_value: "ACME",
  target_account_code: "1020",
  target_cost_center: "",
  auto_approve: true,
  bank_account_id: null,
  enabled: true,
  created_at: "",
  updated_at: "",
};

// A EUR line sitting in the USD operating account — exercises the
// foreign-currency display and the cross-currency match guard.
const TXN_FOREIGN = makeKRecord({
  id: "txn-eur",
  ktype: "finance.bank_transaction",
  data: {
    bank_account_id: "acct-1",
    value_date: "2024-02-04",
    description: "Berlin supplier",
    amount: 100,
    currency: "EUR",
    status: "unreconciled",
  },
});
// An already-reconciled line — exercises the unmatch / correction path.
const TXN_MATCHED = makeKRecord({
  id: "txn-matched",
  ktype: "finance.bank_transaction",
  data: {
    bank_account_id: "acct-1",
    value_date: "2024-02-06",
    description: "Cleared rent",
    amount: 900,
    currency: "USD",
    status: "matched",
    matched_entry_id: "je-cleared-0000",
  },
});
// A duplicate pair (same date/desc/amount) and an equal-and-opposite
// reversal pair — exercise the anomaly badges.
const TXN_DUP_A = makeKRecord({
  id: "txn-dup-a",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-1", value_date: "2024-02-07", description: "Stripe payout", amount: 500, currency: "USD", status: "unreconciled" },
});
const TXN_DUP_B = makeKRecord({
  id: "txn-dup-b",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-1", value_date: "2024-02-07", description: "Stripe payout", amount: 500, currency: "USD", status: "unreconciled" },
});
const TXN_REV_POS = makeKRecord({
  id: "txn-rev-pos",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-1", value_date: "2024-02-08", description: "Bounced cheque", amount: 320, currency: "USD", status: "unreconciled" },
});
const TXN_REV_NEG = makeKRecord({
  id: "txn-rev-neg",
  ktype: "finance.bank_transaction",
  data: { bank_account_id: "acct-1", value_date: "2024-02-09", description: "Bounced cheque", amount: -320, currency: "USD", status: "unreconciled" },
});

const EUR_RATE = {
  tenant_id: "t",
  from_currency: "EUR",
  to_currency: "USD",
  rate_date: "2024-02-04",
  rate: "1.10",
  created_at: "",
  updated_at: "",
};

function routeListRecords(txns = [TXN_UNRECONCILED, TXN_OTHER_ACCOUNT]) {
  listRecords.mockImplementation((ktype: string) => {
    if (ktype === "finance.bank_account") return Promise.resolve([ACCOUNT, ACCOUNT_2]);
    if (ktype === "finance.bank_transaction") return Promise.resolve(txns);
    return Promise.resolve([]);
  });
}

describe("BankReconciliationPage", () => {
  beforeEach(() => {
    listRecords.mockReset();
    updateRecord.mockReset();
    createRecord.mockReset();
    listBankFeedSuggestions.mockReset();
    listBankFeedRules.mockReset();
    acceptBankFeedSuggestion.mockReset();
    rejectBankFeedSuggestion.mockReset();
    listExchangeRates.mockReset();
    routeListRecords();
    listBankFeedSuggestions.mockResolvedValue([]);
    listBankFeedRules.mockResolvedValue([]);
    acceptBankFeedSuggestion.mockResolvedValue(undefined);
    rejectBankFeedSuggestion.mockResolvedValue(undefined);
    listExchangeRates.mockResolvedValue({ rates: [] });
  });

  it("lists accounts and prompts to pick one", async () => {
    renderWithProviders(<BankReconciliationPage />);
    expect(screen.getByRole("heading", { name: "Bank Reconciliation" })).toBeInTheDocument();
    expect(await screen.findByText("Operating USD")).toBeInTheDocument();
    expect(screen.getByText("Select a bank account.")).toBeInTheDocument();
  });

  it("shows only the selected account's transactions", async () => {
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    // The selected account's line shows up (it appears in both the
    // side-by-side workspace and the transaction table).
    expect((await screen.findAllByText("ACME invoice")).length).toBeGreaterThan(0);
    // A transaction belonging to a different account is filtered out.
    expect(screen.queryByText("Other acct")).not.toBeInTheDocument();
  });

  it("renders the empty state when the account has no transactions", async () => {
    routeListRecords([TXN_OTHER_ACCOUNT]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));
    expect(await screen.findByText("No transactions yet.")).toBeInTheDocument();
  });

  it("marks an unreconciled line ignored via updateRecord", async () => {
    updateRecord.mockResolvedValue(TXN_UNRECONCILED);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));
    await user.click(await screen.findByRole("button", { name: "Mark ignored" }));

    expect(updateRecord).toHaveBeenCalledWith(
      "finance.bank_transaction",
      "txn-1",
      expect.objectContaining({ status: "ignored" }),
    );
  });

  it("imports statement lines from a CSV file", async () => {
    createRecord.mockResolvedValue(makeKRecord({ ktype: "finance.bank_transaction" }));
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const csv = "value_date,description,amount,currency\n2024-03-01,Deposit,500,USD\n2024-03-02,Fee,-12,USD\n";
    const file = csvFile("statement.csv", csv);
    const header = screen.getByRole("heading", { name: "Transactions" }).closest("header") as HTMLElement;
    const input = header.querySelector('input[type="file"]') as HTMLInputElement;
    await user.upload(input, file);

    await waitFor(() => expect(createRecord).toHaveBeenCalledTimes(2));
    expect(createRecord).toHaveBeenCalledWith(
      "finance.bank_transaction",
      expect.objectContaining({ bank_account_id: "acct-1", description: "Deposit", amount: 500 }),
    );
  });

  it("surfaces a CSV parse error for a malformed header", async () => {
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const badCsv = "foo,bar\n1,2\n";
    const file = csvFile("bad.csv", badCsv);
    const header = screen.getByRole("heading", { name: "Transactions" }).closest("header") as HTMLElement;
    const input = header.querySelector('input[type="file"]') as HTMLInputElement;
    await user.upload(input, file);

    expect(
      await screen.findByText(/CSV must have value_date, description, amount columns/i),
    ).toBeInTheDocument();
    expect(createRecord).not.toHaveBeenCalled();
  });

  it("surfaces the best suggestion with confidence and reasons, and accepts it", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST, SUGGESTION_ALT]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const queue = await screen.findByRole("region", { name: "Match review queue" });
    expect(within(queue).getByText("95%")).toBeInTheDocument();
    expect(within(queue).getByText("exact amount")).toBeInTheDocument();
    expect(within(queue).getByText("same-day")).toBeInTheDocument();

    await user.click(within(queue).getByRole("button", { name: "Accept" }));
    expect(acceptBankFeedSuggestion).toHaveBeenCalledWith("sug-best");
  });

  it("reveals alternative candidates behind the find-alternative toggle", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST, SUGGESTION_ALT]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const queue = await screen.findByRole("region", { name: "Match review queue" });
    // The alternative's reason is hidden until the toggle is opened.
    expect(within(queue).queryByText("amount within tolerance")).not.toBeInTheDocument();
    await user.click(within(queue).getByRole("button", { name: /Find alternative \(1\)/ }));
    expect(within(queue).getByText("amount within tolerance")).toBeInTheDocument();
  });

  it("rejects a suggestion via rejectBankFeedSuggestion", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const queue = await screen.findByRole("region", { name: "Match review queue" });
    await user.click(within(queue).getByRole("button", { name: "Reject" }));
    expect(rejectBankFeedSuggestion).toHaveBeenCalledWith("sug-best");
  });

  it("accepts only high-confidence suggestions in the bulk action", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST, SUGGESTION_ALT]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const queue = await screen.findByRole("region", { name: "Match review queue" });
    await user.click(
      within(queue).getByRole("button", { name: /Accept all high-confidence \(1\)/ }),
    );
    await waitFor(() => expect(acceptBankFeedSuggestion).toHaveBeenCalledTimes(1));
    expect(acceptBankFeedSuggestion).toHaveBeenCalledWith("sug-best");
  });

  it("shows candidate ledger entries side-by-side when a line is selected", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const sidebyside = await screen.findByRole("region", {
      name: "Side-by-side reconciliation",
    });
    expect(
      within(sidebyside).getByText("Select a bank line to see candidate ledger entries."),
    ).toBeInTheDocument();

    await user.click(within(sidebyside).getByRole("button", { name: /ACME invoice/ }));
    expect(
      await within(sidebyside).findByRole("button", { name: "Match" }),
    ).toBeInTheDocument();
  });

  it("surfaces auto-detected transfer pairs", async () => {
    routeListRecords([TXN_TRANSFER_OUT, TXN_TRANSFER_IN]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const transfers = await screen.findByRole("region", { name: "Detected transfers" });
    expect(within(transfers).getByText(/Transfer to savings/)).toBeInTheDocument();
    expect(within(transfers).getByText("Operating USD")).toBeInTheDocument();
    expect(within(transfers).getByText("Savings USD")).toBeInTheDocument();
  });

  it("lists reconciliation rules", async () => {
    listBankFeedRules.mockResolvedValue([RULE]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const rules = await screen.findByRole("region", { name: "Reconciliation rules" });
    expect(within(rules).getByText("description_contains: ACME")).toBeInTheDocument();
    expect(within(rules).getByText("Enabled")).toBeInTheDocument();
  });

  // --- Batch-3: multi-currency -----------------------------------------
  it("shows the base-currency equivalent for a foreign line", async () => {
    routeListRecords([TXN_FOREIGN]);
    listExchangeRates.mockResolvedValue({ rates: [EUR_RATE] });
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const sidebyside = await screen.findByRole("region", {
      name: "Side-by-side reconciliation",
    });
    // 100 EUR × 1.10 = 110.00 USD base equivalent surfaces on the line.
    expect(
      await within(sidebyside).findByText(/110\.00 USD/),
    ).toBeInTheDocument();
    expect(
      within(sidebyside).getAllByText(/Foreign currency/i).length,
    ).toBeGreaterThan(0);
  });

  it("guards a cross-currency match with an explicit warning and 'Match anyway'", async () => {
    routeListRecords([TXN_FOREIGN]);
    listBankFeedSuggestions.mockResolvedValue([
      { ...SUGGESTION_BEST, transaction_id: "txn-eur" },
    ]);
    listExchangeRates.mockResolvedValue({ rates: [EUR_RATE] });
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const sidebyside = await screen.findByRole("region", {
      name: "Side-by-side reconciliation",
    });
    await user.click(
      within(sidebyside).getByRole("button", { name: /Berlin supplier/ }),
    );
    expect(await within(sidebyside).findByRole("alert")).toHaveTextContent(
      /EUR/,
    );
    // The action is relabelled so the operator can't silently mis-match.
    expect(
      within(sidebyside).getByRole("button", { name: /Match anyway/i }),
    ).toBeInTheDocument();
    expect(
      within(sidebyside).queryByRole("button", { name: /^Match$/ }),
    ).not.toBeInTheDocument();
  });

  // --- Batch-3: undo / correction --------------------------------------
  it("unmatches a reconciled line, clearing the ledger reference", async () => {
    routeListRecords([TXN_MATCHED]);
    updateRecord.mockResolvedValue(TXN_MATCHED);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    await user.click(await screen.findByRole("button", { name: "Unmatch" }));
    expect(updateRecord).toHaveBeenCalledWith(
      "finance.bank_transaction",
      "txn-matched",
      expect.objectContaining({ status: "unreconciled" }),
    );
    const payload = updateRecord.mock.calls[0][2] as Record<string, unknown>;
    expect(payload).not.toHaveProperty("matched_entry_id");
  });

  it("offers undo after a bulk accept and reverts each line", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST]);
    updateRecord.mockResolvedValue(TXN_UNRECONCILED);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const queue = await screen.findByRole("region", { name: "Match review queue" });
    await user.click(
      within(queue).getByRole("button", { name: /Accept all high-confidence \(1\)/ }),
    );
    await waitFor(() =>
      expect(acceptBankFeedSuggestion).toHaveBeenCalledWith("sug-best"),
    );

    const undo = await screen.findByRole("button", { name: "Undo" });
    await user.click(undo);
    await waitFor(() =>
      expect(updateRecord).toHaveBeenCalledWith(
        "finance.bank_transaction",
        "txn-1",
        expect.objectContaining({ status: "unreconciled" }),
      ),
    );
  });

  // --- Batch-3: edge states --------------------------------------------
  it("flags duplicate and reversed lines in the transaction table", async () => {
    routeListRecords([TXN_DUP_A, TXN_DUP_B, TXN_REV_POS, TXN_REV_NEG]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    expect((await screen.findAllByText("Duplicate")).length).toBe(2);
    expect(screen.getAllByText("Reversal").length).toBe(2);
  });

  it("shows a retry affordance when suggestions fail to load", async () => {
    listBankFeedSuggestions.mockRejectedValueOnce(new Error("boom"));
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    expect(
      await screen.findByText("Could not load suggestions."),
    ).toBeInTheDocument();
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST]);
    await user.click(screen.getByRole("button", { name: "Retry" }));
    const queue = await screen.findByRole("region", { name: "Match review queue" });
    expect(await within(queue).findByText("95%")).toBeInTheDocument();
  });

  // --- Batch-3: keyboard & throughput ----------------------------------
  it("filters unmatched lines by the search box", async () => {
    routeListRecords([TXN_UNRECONCILED, TXN_DUP_A]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const sidebyside = await screen.findByRole("region", {
      name: "Side-by-side reconciliation",
    });
    // Both lines are listed before filtering.
    expect(
      within(sidebyside).getByRole("button", { name: /ACME invoice/ }),
    ).toBeInTheDocument();
    expect(
      within(sidebyside).getByRole("button", { name: /Stripe payout/ }),
    ).toBeInTheDocument();

    await user.type(
      screen.getByPlaceholderText(/Filter by payee/i),
      "stripe",
    );
    expect(
      within(sidebyside).queryByRole("button", { name: /ACME invoice/ }),
    ).not.toBeInTheDocument();
    expect(
      within(sidebyside).getByRole("button", { name: /Stripe payout/ }),
    ).toBeInTheDocument();
  });

  it("accepts the active line's best candidate from the keyboard", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const queue = await screen.findByRole("region", { name: "Match review queue" });
    const listbox = within(queue).getByRole("listbox");
    listbox.focus();
    await user.keyboard("a");
    await waitFor(() =>
      expect(acceptBankFeedSuggestion).toHaveBeenCalledWith("sug-best"),
    );
  });

  it("supports a split match across multiple ledger entries", async () => {
    listBankFeedSuggestions.mockResolvedValue([SUGGESTION_BEST, SUGGESTION_ALT]);
    const user = userEvent.setup();
    renderWithProviders(<BankReconciliationPage />);
    await user.click(await screen.findByText("Operating USD"));

    const sidebyside = await screen.findByRole("region", {
      name: "Side-by-side reconciliation",
    });
    await user.click(
      within(sidebyside).getByRole("button", { name: /ACME invoice/ }),
    );
    await user.click(
      await within(sidebyside).findByRole("button", { name: /Split across entries/i }),
    );

    // Allocate the full 250 across the two candidates (125 + 125).
    const selects = within(sidebyside).getAllByLabelText("Ledger entry");
    const amounts = within(sidebyside).getAllByLabelText(/allocated/i);
    await user.type(amounts[0], "125");
    await user.click(within(sidebyside).getByRole("button", { name: /Add entry/i }));
    const selects2 = within(sidebyside).getAllByLabelText("Ledger entry");
    await user.selectOptions(selects2[1], "sug-alt");
    const amounts2 = within(sidebyside).getAllByLabelText(/allocated/i);
    await user.type(amounts2[1], "125");

    await user.click(
      within(sidebyside).getByRole("button", { name: /Reconcile split/i }),
    );
    await waitFor(() => {
      expect(acceptBankFeedSuggestion).toHaveBeenCalledWith("sug-best");
      expect(acceptBankFeedSuggestion).toHaveBeenCalledWith("sug-alt");
    });
    void selects;
  });
});
