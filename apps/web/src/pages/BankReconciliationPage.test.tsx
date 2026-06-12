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

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
    updateRecord: (...a: unknown[]) => updateRecord(...a),
    createRecord: (...a: unknown[]) => createRecord(...a),
    listBankFeedSuggestions: (...a: unknown[]) => listBankFeedSuggestions(...a),
    listBankFeedRules: (...a: unknown[]) => listBankFeedRules(...a),
    acceptBankFeedSuggestion: (...a: unknown[]) => acceptBankFeedSuggestion(...a),
    rejectBankFeedSuggestion: (...a: unknown[]) => rejectBankFeedSuggestion(...a),
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
    routeListRecords();
    listBankFeedSuggestions.mockResolvedValue([]);
    listBankFeedRules.mockResolvedValue([]);
    acceptBankFeedSuggestion.mockResolvedValue(undefined);
    rejectBankFeedSuggestion.mockResolvedValue(undefined);
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
});
