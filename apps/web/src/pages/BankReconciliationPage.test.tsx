import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listRecords = vi.fn();
const updateRecord = vi.fn();
const createRecord = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
    updateRecord: (...a: unknown[]) => updateRecord(...a),
    createRecord: (...a: unknown[]) => createRecord(...a),
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

function routeListRecords(txns = [TXN_UNRECONCILED, TXN_OTHER_ACCOUNT]) {
  listRecords.mockImplementation((ktype: string) => {
    if (ktype === "finance.bank_account") return Promise.resolve([ACCOUNT]);
    if (ktype === "finance.bank_transaction") return Promise.resolve(txns);
    return Promise.resolve([]);
  });
}

describe("BankReconciliationPage", () => {
  beforeEach(() => {
    listRecords.mockReset();
    updateRecord.mockReset();
    createRecord.mockReset();
    routeListRecords();
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

    expect(await screen.findByText("ACME invoice")).toBeInTheDocument();
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
});
