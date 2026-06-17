import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { LocaleProvider } from "../lib/i18n";
import { makeKType, makeKRecord } from "../test/factories";

vi.mock("../lib/api", () => ({
  api: { listRecords: vi.fn().mockResolvedValue([]) },
}));

import { KTypeList } from "./KTypeList";

const KTYPE = makeKType({ name: "crm.deal" });
const ALPHA = makeKRecord({ id: "rec-alpha", data: { title: "Alpha", value: 200 } });
const BETA = makeKRecord({ id: "rec-beta", data: { title: "Beta", value: 100 } });

function renderList(columns: string[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (cols: string[]) => (
    <LocaleProvider>
      <QueryClientProvider client={qc}>
        <KTypeList
          ktype={KTYPE}
          records={[ALPHA, BETA]}
          columns={cols}
          onRowClick={vi.fn()}
        />
      </QueryClientProvider>
    </LocaleProvider>
  );
  const utils = render(tree(columns));
  return { ...utils, rerenderCols: (cols: string[]) => utils.rerender(tree(cols)) };
}

// The visible row order, read off the rendered table body.
function rowOrder(): string[] {
  const order: string[] = [];
  for (const row of screen.getAllByRole("row")) {
    const text = row.textContent ?? "";
    if (text.includes("Alpha")) order.push("Alpha");
    else if (text.includes("Beta")) order.push("Beta");
  }
  return order;
}

describe("KTypeList sort", () => {
  it("clears an active sort when its column is hidden via the Columns menu", async () => {
    const { rerenderCols } = renderList(["title", "value"]);

    // Initial order follows the records array.
    expect(rowOrder()).toEqual(["Alpha", "Beta"]);

    // Sort ascending by Value → the smaller (Beta, 100) comes first.
    await userEvent.click(screen.getByRole("button", { name: "Value" }));
    expect(rowOrder()).toEqual(["Beta", "Alpha"]);

    // Hiding the sorted column must drop the sort (otherwise records sit
    // in an unexplained order with no visible sort indicator).
    rerenderCols(["title"]);
    expect(screen.queryByRole("button", { name: "Value" })).toBeNull();
    await waitFor(() => expect(rowOrder()).toEqual(["Alpha", "Beta"]));
  });
});
