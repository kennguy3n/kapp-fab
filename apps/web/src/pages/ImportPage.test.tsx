import { describe, it, expect, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { http, HttpResponse } from "msw";
import { server } from "../test/msw/server";
import { ImportPage } from "./ImportPage";

// ImportPage talks to /api/v1/imports through the raw fetch API (the
// Phase F surface is not on the generated client yet), so it is driven
// through MSW rather than a module mock. The wizard routes resume from
// /imports/:id, so the harness mounts all three route shapes and lets
// the real router navigate between them on create.
function renderImports(initialEntry: string) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <Routes>
          <Route path="/imports" element={<ImportPage />} />
          {/* /imports/new is captured by the :id param (id === "new"),
              matching the real route table in App.tsx. */}
          <Route path="/imports/:id" element={<ImportPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function job(overrides: Record<string, unknown> = {}) {
  return {
    id: "job-1",
    tenant_id: "t1",
    source_type: "csv",
    status: "mapping",
    config: {},
    mapping: {},
    progress: { source: { entities: [{ name: "Customer", target_ktype: "crm.account", row_count: 5 }] } },
    errors: null,
    reconciliation: {},
    created_by: "u1",
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-02T00:00:00Z",
    ...overrides,
  };
}

describe("ImportPage", () => {
  beforeEach(() => {
    localStorage.setItem("kapp.tenant", "acme");
  });

  it("lists existing import jobs in the index", async () => {
    server.use(
      http.get("/api/v1/imports", () =>
        HttpResponse.json([job({ id: "abcdef12-0000", source_type: "frappe", status: "completed" })]),
      ),
    );
    renderImports("/imports");
    expect(await screen.findByText("frappe")).toBeInTheDocument();
    expect(screen.getByText("completed")).toBeInTheDocument();
    // "New import" is a navigation control, so it renders as a Button
    // styled link (`<Button asChild><Link>`) — a single anchor with
    // role="link", replacing main's invalid <button>-inside-<a> nest.
    expect(
      screen.getByRole("link", { name: "New import" }),
    ).toBeInTheDocument();
  });

  it("shows the empty state when there are no jobs", async () => {
    server.use(http.get("/api/v1/imports", () => HttpResponse.json([])));
    renderImports("/imports");
    expect(await screen.findByText("No imports yet.")).toBeInTheDocument();
  });

  it("surfaces a load error in the index", async () => {
    server.use(
      http.get("/api/v1/imports", () => new HttpResponse(null, { status: 500, statusText: "Server Error" })),
    );
    renderImports("/imports");
    expect(await screen.findByText(/Failed to load jobs:/i)).toBeInTheDocument();
  });

  it("creates a CSV job from step 1 and advances to the mapping step", async () => {
    let posted: Record<string, unknown> | null = null;
    server.use(
      http.post("/api/v1/imports", async ({ request }) => {
        posted = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(job({ status: "mapping" }));
      }),
      http.get("/api/v1/imports/job-1", () => HttpResponse.json(job({ status: "mapping" }))),
      http.get("/api/v1/imports/job-1/errors", () => HttpResponse.json([])),
    );
    const user = userEvent.setup();
    renderImports("/imports/new");

    await user.type(screen.getByLabelText(/Entity \(source/i), "customers");
    await user.type(screen.getByLabelText(/Target KType/i), "crm.account");
    await user.type(screen.getByLabelText(/Payload/i), "name\nAcme");
    await user.click(screen.getByRole("button", { name: /Create job/i }));

    // Router navigates to /imports/job-1 and the wizard resumes on the
    // mapping step, listing the discovered source entity.
    expect(await screen.findByRole("heading", { name: /Step 2\. Mapping/i })).toBeInTheDocument();
    expect(screen.getByText("Customer")).toBeInTheDocument();
    expect(posted).toMatchObject({ source_type: "csv" });
  });

  it("reveals Frappe-specific fields when that source type is chosen", async () => {
    const user = userEvent.setup();
    renderImports("/imports/new");
    await user.selectOptions(screen.getByLabelText(/Source type/i), "frappe");
    expect(screen.getByLabelText(/Frappe base URL/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/DocTypes/i)).toBeInTheDocument();
    // The CSV-only payload box is gone in Frappe mode.
    expect(screen.queryByLabelText(/Payload/i)).not.toBeInTheDocument();
  });

  it("renders the validation step with the per-row error report", async () => {
    server.use(
      http.get("/api/v1/imports/job-1", () => HttpResponse.json(job({ status: "validating" }))),
      http.get("/api/v1/imports/job-1/errors", () =>
        HttpResponse.json([
          {
            id: 1,
            job_id: "job-1",
            source_type: "csv",
            source_id: "row-7",
            target_ktype: "crm.account",
            data: {},
            validation_errors: [{ field: "email", code: "required", message: "email is required" }],
            status: "invalid",
          },
        ]),
      ),
    );
    renderImports("/imports/job-1");
    expect(await screen.findByRole("heading", { name: /Step 3\. Validate/i })).toBeInTheDocument();
    expect(screen.getByText("1 invalid rows")).toBeInTheDocument();
    expect(screen.getByText(/email is required/)).toBeInTheDocument();
  });

  it("renders the review step and accepts the cutover", async () => {
    let accepted = false;
    server.use(
      http.get("/api/v1/imports/job-1", () =>
        HttpResponse.json(
          job({ status: "reconciling", reconciliation: { source_count: 10, staged_count: 10, valid_count: 9, invalid_count: 1 } }),
        ),
      ),
      http.get("/api/v1/imports/job-1/errors", () => HttpResponse.json([])),
      http.post("/api/v1/imports/job-1/accept", () => {
        accepted = true;
        return HttpResponse.json({ job: job({ status: "completed" }), imported: 9 });
      }),
    );
    const user = userEvent.setup();
    renderImports("/imports/job-1");

    expect(await screen.findByRole("heading", { name: /Step 4\. Review/i })).toBeInTheDocument();
    expect(screen.getByText("9")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Accept & cutover/i }));
    await waitFor(() => expect(accepted).toBe(true));
  });

  it("renders the completion summary for a finished job", async () => {
    server.use(
      http.get("/api/v1/imports/job-1", () =>
        HttpResponse.json(job({ status: "completed", progress: { imported: 42 }, completed_at: "2024-02-01T00:00:00Z" })),
      ),
      http.get("/api/v1/imports/job-1/errors", () => HttpResponse.json([])),
    );
    renderImports("/imports/job-1");
    expect(await screen.findByRole("heading", { name: /Step 5\. Complete/i })).toBeInTheDocument();
    expect(screen.getByText("42")).toBeInTheDocument();
  });
});
