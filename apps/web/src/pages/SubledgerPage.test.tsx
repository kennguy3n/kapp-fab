import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders, makeKRecord } from "../test-utils";

const listRecords = vi.fn();
const postInvoice = vi.fn();
const postBill = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...args: unknown[]) => listRecords(...args),
    postInvoice: (...args: unknown[]) => postInvoice(...args),
    postBill: (...args: unknown[]) => postBill(...args),
  },
}));

import { SubledgerPage } from "./SubledgerPage";

const orgs = [
  makeKRecord({
    id: "org1",
    ktype: "crm.organization",
    data: { name: "Acme Corp" },
  }),
];

const invoices = [
  makeKRecord({
    id: "inv1",
    ktype: "finance.ar_invoice",
    data: {
      invoice_number: "INV-001",
      customer_id: "org1",
      due_date: "2025-02-01T00:00:00Z",
      total: "5000.00",
      currency: "USD",
      status: "posted",
    },
  }),
  makeKRecord({
    id: "inv2",
    ktype: "finance.ar_invoice",
    data: {
      invoice_number: "INV-002",
      customer_id: "org1",
      due_date: "2025-03-01T00:00:00Z",
      total: "2500.00",
      currency: "USD",
      status: "draft",
    },
  }),
];

function mockData() {
  listRecords.mockImplementation((ktype: string) =>
    ktype === "crm.organization"
      ? Promise.resolve(orgs)
      : Promise.resolve(invoices),
  );
}

describe("SubledgerPage (AR)", () => {
  beforeEach(() => {
    listRecords.mockReset();
    postInvoice.mockReset();
    postBill.mockReset();
    postInvoice.mockResolvedValue(undefined);
  });

  it("resolves counterparties to names and formats outstanding totals", async () => {
    mockData();
    renderWithProviders(<SubledgerPage variant="ar" />);

    expect(await screen.findByText("INV-001")).toBeInTheDocument();
    expect(screen.getByText("INV-002")).toBeInTheDocument();

    // Counterparty UUIDs never leak — resolved to the org name.
    expect(screen.getAllByText("Acme Corp").length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText("org1")).toBeNull();

    // Amounts are currency-formatted; lifecycle tokens become badges.
    expect(screen.getAllByText("$5,000.00").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText("$2,500.00")).toBeInTheDocument();
    expect(screen.getAllByText("Posted").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Draft").length).toBeGreaterThanOrEqual(1);
  });

  it("posts a draft document to the ledger", async () => {
    mockData();
    renderWithProviders(<SubledgerPage variant="ar" />);
    await screen.findByText("INV-002");

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Post" }));

    expect(postInvoice).toHaveBeenCalledWith("inv2");
  });

  it("renders an error surface with retry when the records query fails", async () => {
    listRecords.mockImplementation((ktype: string) =>
      ktype === "crm.organization"
        ? Promise.resolve([])
        : Promise.reject(new Error("subledger down")),
    );
    renderWithProviders(<SubledgerPage variant="ar" />);

    expect(
      await screen.findByText(/Couldn't load the ar subledger/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: /try again/i }),
    ).toBeInTheDocument();
  });

  it("teaches the empty state when there are no invoices", async () => {
    listRecords.mockResolvedValue([]);
    renderWithProviders(<SubledgerPage variant="ar" />);

    expect(await screen.findByText(/No invoices yet/i)).toBeInTheDocument();
  });
});
