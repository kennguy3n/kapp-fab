import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";

const listRecords = vi.fn();
const updateRecord = vi.fn();

vi.mock("../lib/api", () => ({
  api: {
    listRecords: (...a: unknown[]) => listRecords(...a),
    updateRecord: (...a: unknown[]) => updateRecord(...a),
  },
}));

import { OnboardingChecklistPage } from "./OnboardingChecklistPage";

function checklistRecord(
  steps: { key: string; label: string; done: boolean; link?: string }[],
  extra: Record<string, unknown> = {},
) {
  return {
    id: "task-1",
    tenant_id: "t-1",
    ktype: "tasks.task",
    ktype_version: 1,
    status: "active",
    version: 1,
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
    data: {
      title: "Getting Started",
      onboarding: "checklist",
      steps,
      ...extra,
    },
  };
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/onboarding"]}>
        <Routes>
          <Route path="/onboarding" element={<OnboardingChecklistPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("OnboardingChecklistPage", () => {
  beforeEach(() => {
    listRecords.mockReset();
    updateRecord.mockReset();
    updateRecord.mockResolvedValue({});
  });

  it("renders the checklist steps and progress", async () => {
    listRecords.mockResolvedValue([
      checklistRecord([
        { key: "create_contact", label: "Create your first contact", done: true },
        { key: "send_invoice", label: "Send your first invoice", done: false },
      ]),
    ]);
    renderPage();
    expect(await screen.findByText(/Create your first contact/i)).toBeInTheDocument();
    expect(screen.getByText(/1 \/ 2 done/i)).toBeInTheDocument();
  });

  it("toggles a step via PATCH", async () => {
    listRecords.mockResolvedValue([
      checklistRecord([
        { key: "send_invoice", label: "Send your first invoice", done: false },
      ]),
    ]);
    renderPage();
    const checkbox = await screen.findByRole("checkbox", {
      name: /Send your first invoice/i,
    });
    await userEvent.click(checkbox);
    await waitFor(() => expect(updateRecord).toHaveBeenCalledTimes(1));
    const [ktype, id, data] = updateRecord.mock.calls[0]!;
    expect(ktype).toBe("tasks.task");
    expect(id).toBe("task-1");
    expect((data as { steps: { done: boolean }[] }).steps[0]!.done).toBe(true);
  });

  it("only enables Dismiss once every step is done", async () => {
    listRecords.mockResolvedValue([
      checklistRecord([
        { key: "a", label: "Step A", done: true },
        { key: "b", label: "Step B", done: true },
      ]),
    ]);
    renderPage();
    const dismiss = await screen.findByRole("button", {
      name: /Dismiss checklist/i,
    });
    expect(dismiss).not.toBeDisabled();
    await userEvent.click(dismiss);
    await waitFor(() => expect(updateRecord).toHaveBeenCalledTimes(1));
    expect(
      (updateRecord.mock.calls[0]![2] as { dismissed: boolean }).dismissed,
    ).toBe(true);
  });

  it("shows the empty message when no checklist exists", async () => {
    listRecords.mockResolvedValue([]);
    renderPage();
    expect(
      await screen.findByText(/No onboarding checklist found/i),
    ).toBeInTheDocument();
  });
});
