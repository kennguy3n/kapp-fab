import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const listRecords = vi.fn();
const generatePayslips = vi.fn();
const postPayRun = vi.fn();
const listPayRunPayslips = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
    generatePayslips: (...a: unknown[]) => generatePayslips(...a),
    postPayRun: (...a: unknown[]) => postPayRun(...a),
    listPayRunPayslips: (...a: unknown[]) => listPayRunPayslips(...a),
  },
}));

const navigate = vi.fn();
vi.mock("react-router-dom", async (importOriginal) => {
  const actual = await importOriginal<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => navigate };
});

import { PayrollPage } from "./PayrollPage";
import { renderWithProviders, makeKRecord } from "../test-utils";

const COMPONENT = makeKRecord({
  id: "comp-1",
  ktype: "hr.salary_component",
  data: { code: "BASIC", name: "Basic Pay", type: "earning", amount: 5000, currency: "USD", active: true },
});
const STRUCTURE = makeKRecord({
  id: "struct-1",
  ktype: "hr.salary_structure",
  data: { employee_id: "emp-1", effective_from: "2024-01-01", base_salary: 6000, currency: "USD", payment_frequency: "monthly", status: "active" },
});
const RUN = makeKRecord({
  id: "run-1",
  ktype: "hr.pay_run",
  data: { name: "Jan 2024", pay_period_start: "2024-01-01", pay_period_end: "2024-01-31", department: "Eng", payslip_count: 2, total_gross: 12000, total_net: 9000, currency: "USD", status: "draft" },
});
// Employee records back the humanized name columns (structures + payslips
// resolve employee_id → display name via the shared recordLabel helper).
const EMPLOYEES = [
  makeKRecord({ id: "emp-1", ktype: "hr.employee", data: { name: "Ada Lovelace", designation: "Engineer" } }),
  makeKRecord({ id: "emp-9", ktype: "hr.employee", data: { name: "Grace Hopper", designation: "Engineer" } }),
];

// Route listRecords by the ktype argument so each tab's query gets the
// matching fixture set.
function routeListRecords() {
  listRecords.mockImplementation((ktype: string) => {
    if (ktype === "hr.salary_component") return Promise.resolve([COMPONENT]);
    if (ktype === "hr.salary_structure") return Promise.resolve([STRUCTURE]);
    if (ktype === "hr.pay_run") return Promise.resolve([RUN]);
    if (ktype === "hr.employee") return Promise.resolve(EMPLOYEES);
    return Promise.resolve([]);
  });
}

describe("PayrollPage", () => {
  beforeEach(() => {
    listRecords.mockReset();
    generatePayslips.mockReset();
    postPayRun.mockReset();
    listPayRunPayslips.mockReset();
    navigate.mockReset();
    routeListRecords();
  });

  it("renders the components tab by default with its rows", async () => {
    renderWithProviders(<PayrollPage />);
    expect(screen.getByRole("heading", { name: "Payroll" })).toBeInTheDocument();
    expect(await screen.findByText("Basic Pay")).toBeInTheDocument();
    expect(screen.getByText("BASIC")).toBeInTheDocument();
    // Components tab reflects the active state.
    expect(screen.getByRole("tab", { name: "Components" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
  });

  it("switches to the structures tab and lists salary structures", async () => {
    const user = userEvent.setup();
    renderWithProviders(<PayrollPage />);
    await user.click(screen.getByRole("tab", { name: "Structures" }));
    expect(await screen.findByText("Ada Lovelace")).toBeInTheDocument();
    expect(screen.getByText("Monthly")).toBeInTheDocument();
  });

  it("routes the New button to the create form for the active tab", async () => {
    const user = userEvent.setup();
    renderWithProviders(<PayrollPage />);
    await user.click(screen.getByRole("button", { name: /New component/i }));
    expect(navigate).toHaveBeenCalledWith("/records/hr.salary_component/new");
  });

  it("generates payslips for a draft run and shows the summary", async () => {
    generatePayslips.mockResolvedValue({
      created_count: 3,
      skipped_existing: 1,
      skipped_no_structure: 2,
    });
    const user = userEvent.setup();
    renderWithProviders(<PayrollPage />);
    await user.click(screen.getByRole("tab", { name: "Pay Runs" }));
    await screen.findByText("Jan 2024");

    await user.click(screen.getByRole("button", { name: "Generate" }));
    expect(generatePayslips).toHaveBeenCalledWith("run-1");
    expect(
      await screen.findByText(/Created 3 slip\(s\); skipped 1 existing, 2 without/i),
    ).toBeInTheDocument();
  });

  it("surfaces a post failure inline", async () => {
    postPayRun.mockRejectedValue(new Error("period locked"));
    const user = userEvent.setup();
    renderWithProviders(<PayrollPage />);
    await user.click(screen.getByRole("tab", { name: "Pay Runs" }));
    await screen.findByText("Jan 2024");

    // Posting is irreversible, so it's gated behind a confirmation dialog.
    await user.click(screen.getByRole("button", { name: "Post" }));
    await user.click(screen.getByRole("button", { name: "Post pay run" }));
    expect(await screen.findByText(/Post failed:.*period locked/i)).toBeInTheDocument();
  });

  it("loads the run's payslips via the dedicated endpoint when expanded", async () => {
    listPayRunPayslips.mockResolvedValue([
      makeKRecord({
        id: "slip-1",
        ktype: "hr.payslip",
        data: { employee_id: "emp-9", gross_pay: 4000, total_deductions: 500, net_pay: 3500, currency: "USD", status: "draft" },
      }),
    ]);
    const user = userEvent.setup();
    renderWithProviders(<PayrollPage />);
    await user.click(screen.getByRole("tab", { name: "Pay Runs" }));
    await screen.findByText("Jan 2024");

    await user.click(screen.getByRole("button", { name: "View slips" }));
    expect(listPayRunPayslips).toHaveBeenCalledWith("run-1");
    const slipsSection = (await screen.findByRole("heading", { name: "Payslips" }))
      .closest("section") as HTMLElement;
    expect(within(slipsSection).getByText("Grace Hopper")).toBeInTheDocument();
    // Toggling again hides the panel.
    await user.click(screen.getByRole("button", { name: "Hide slips" }));
    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Payslips" })).not.toBeInTheDocument(),
    );
  });

  it("shows the error state when the pay-run list query fails", async () => {
    listRecords.mockImplementation((ktype: string) =>
      ktype === "hr.pay_run"
        ? Promise.reject(new Error("boom"))
        : Promise.resolve([]),
    );
    const user = userEvent.setup();
    renderWithProviders(<PayrollPage />);
    await user.click(screen.getByRole("tab", { name: "Pay Runs" }));
    expect(await screen.findByText(/We couldn't load pay runs/i)).toBeInTheDocument();
  });
});
