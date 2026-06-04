import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { server } from "../test/msw/server";

const listKTypes = vi.fn();
vi.mock("../lib/api", () => ({
  api: { listKTypes: (...a: unknown[]) => listKTypes(...a) },
}));

import { ImportMappingPage } from "./ImportMappingPage";
import { renderWithProviders } from "../test-utils";

// ImportMappingPage mixes transports: the import job is read via raw
// fetch (MSW) while the KType registry comes from the generated client
// (module mock). Tests exercise both paths together.
const KTYPES = [
  {
    name: "crm.account",
    version: 1,
    schema: { name: "crm.account", version: 1, fields: [{ name: "name", type: "string" }, { name: "email", type: "string" }] },
  },
];

function importJob(overrides: Record<string, unknown> = {}) {
  return {
    id: "job-1",
    status: "mapping",
    source_type: "csv",
    progress: { source: { entities: [{ name: "Customer", row_count: 5, fields: ["cust_name", "email_addr"], target_ktype: "crm.account" }] } },
    mapping: {},
    ...overrides,
  };
}

function render(route = "/imports/job-1/mapping") {
  return renderWithProviders(<ImportMappingPage />, {
    route,
    path: "/imports/:id/mapping",
    tenant: "acme",
  });
}

describe("ImportMappingPage", () => {
  beforeEach(() => {
    listKTypes.mockReset();
    listKTypes.mockResolvedValue(KTYPES);
  });

  it("renders an editor row per discovered source field", async () => {
    server.use(http.get("/api/v1/imports/job-1", () => HttpResponse.json(importJob())));
    render();

    expect(await screen.findByRole("heading", { name: "Customer" })).toBeInTheDocument();
    expect(screen.getByText("5 rows")).toBeInTheDocument();
    // Each source field gets its own mapping row.
    expect(screen.getByText("cust_name")).toBeInTheDocument();
    expect(screen.getByText("email_addr")).toBeInTheDocument();
  });

  it("shows the empty state when discovery found no entities", async () => {
    server.use(
      http.get("/api/v1/imports/job-1", () =>
        HttpResponse.json(importJob({ progress: { source: { entities: [] } } })),
      ),
    );
    render();
    expect(
      await screen.findByText(/No discovered entities yet/i),
    ).toBeInTheDocument();
  });

  it("saves a field-level mapping back to the import job", async () => {
    type MapBody = {
      mapping?: { entities?: Record<string, { fields?: Record<string, string> }> };
    };
    // Hold the captured body on an object so its type isn't narrowed to
    // `null` across the MSW callback boundary (TS control-flow analysis
    // ignores reassignments that only happen inside closures).
    const captured: { body: MapBody | null } = { body: null };
    server.use(
      http.get("/api/v1/imports/job-1", () => HttpResponse.json(importJob())),
      http.post("/api/v1/imports/job-1/map", async ({ request }) => {
        captured.body = (await request.json()) as MapBody;
        return HttpResponse.json(importJob());
      }),
    );
    const user = userEvent.setup();
    render();

    // Wait for the field rows, then map the source "email_addr" column
    // onto the target KType's "email" field.
    await screen.findByText("email_addr");
    const targetSelects = screen.getAllByRole("combobox");
    // selects: [0] = entity target KType, [1..] = per source field. The
    // last two are the field selects for cust_name / email_addr.
    const emailSelect = targetSelects[targetSelects.length - 1];
    await user.selectOptions(emailSelect, "email");

    await user.click(screen.getByRole("button", { name: /Save mapping/i }));
    await waitFor(() => expect(captured.body).not.toBeNull());
    expect(captured.body?.mapping?.entities?.Customer?.fields?.email_addr).toBe("email");
  });
});
