import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { KRecord, KType } from "@kapp/client";
import { renderWithProviders } from "../test-utils";
import { KanbanView } from "./KanbanView";

// KanbanView groups records into columns derived from the KType's
// kanban.group_by field. Two column-derivation paths matter: enum
// fields (columns = declared enum values, in schema order) and the
// observed-values fallback for non-enum group_by fields. Both are
// pinned, along with card rendering and the click/move callbacks.

function rec(id: string, data: Record<string, unknown>): KRecord {
  return {
    id,
    tenant_id: "t1",
    ktype: "task",
    ktype_version: 1,
    data,
    status: "active",
    version: 1,
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
  };
}

function ktype(opts: {
  groupBy?: string;
  values?: string[];
  cardTitle?: string;
  cardSubtitle?: string;
}): KType {
  const fields = opts.groupBy
    ? [{ name: opts.groupBy, type: "enum", values: opts.values }]
    : [];
  return {
    name: "task",
    version: 1,
    schema: {
      name: "task",
      version: 1,
      fields,
      views: opts.groupBy
        ? {
            kanban: {
              group_by: opts.groupBy,
              card_title: opts.cardTitle,
              card_subtitle: opts.cardSubtitle,
            },
          }
        : undefined,
    },
  };
}

describe("KanbanView", () => {
  it("renders an explanatory message when no kanban view is configured", () => {
    renderWithProviders(
      <KanbanView ktype={ktype({})} records={[]} onCardClick={() => {}} />,
    );
    expect(
      screen.getByText(/no kanban view configured/i),
    ).toBeInTheDocument();
  });

  it("derives columns from the enum values in schema order", () => {
    renderWithProviders(
      <KanbanView
        ktype={ktype({ groupBy: "stage", values: ["todo", "doing", "done"] })}
        records={[rec("1", { stage: "doing", name: "Card A" })]}
        onCardClick={() => {}}
      />,
    );
    for (const label of ["todo", "doing", "done"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it("places each card in its group_by column and shows the per-column count", () => {
    renderWithProviders(
      <KanbanView
        ktype={ktype({ groupBy: "stage", values: ["todo", "done"] })}
        records={[
          rec("1", { stage: "todo", name: "First" }),
          rec("2", { stage: "todo", name: "Second" }),
          rec("3", { stage: "done", name: "Third" }),
        ]}
        onCardClick={() => {}}
      />,
    );
    expect(screen.getByText("First")).toBeInTheDocument();
    expect(screen.getByText("Second")).toBeInTheDocument();
    expect(screen.getByText("Third")).toBeInTheDocument();
  });

  it("falls back to observed string values when group_by is not an enum", () => {
    // group_by field has no declared enum values, so columns are the
    // distinct observed values in first-seen order.
    const kt = ktype({ groupBy: "stage" });
    kt.schema.fields = [{ name: "stage", type: "string" }];
    renderWithProviders(
      <KanbanView
        ktype={kt}
        records={[
          rec("1", { stage: "backlog", name: "A" }),
          rec("2", { stage: "shipped", name: "B" }),
        ]}
        onCardClick={() => {}}
      />,
    );
    expect(screen.getByText("backlog")).toBeInTheDocument();
    expect(screen.getByText("shipped")).toBeInTheDocument();
  });

  it("uses card_title / card_subtitle from the kanban config", () => {
    renderWithProviders(
      <KanbanView
        ktype={ktype({
          groupBy: "stage",
          values: ["todo"],
          cardTitle: "summary",
          cardSubtitle: "owner",
        })}
        records={[rec("1", { stage: "todo", summary: "Fix bug", owner: "ken" })]}
        onCardClick={() => {}}
      />,
    );
    expect(screen.getByText("Fix bug")).toBeInTheDocument();
    expect(screen.getByText("ken")).toBeInTheDocument();
  });

  it("invokes onCardClick with the clicked record", async () => {
    const user = userEvent.setup();
    const onCardClick = vi.fn();
    const card = rec("1", { stage: "todo", name: "Clickable" });
    renderWithProviders(
      <KanbanView
        ktype={ktype({ groupBy: "stage", values: ["todo"] })}
        records={[card]}
        onCardClick={onCardClick}
      />,
    );
    await user.click(screen.getByText("Clickable"));
    expect(onCardClick).toHaveBeenCalledWith(card);
  });

  it("fires onMove(record, toStage) on a drag-and-drop between columns", () => {
    const onMove = vi.fn();
    const card = rec("1", { stage: "todo", name: "Movable" });
    renderWithProviders(
      <KanbanView
        ktype={ktype({ groupBy: "stage", values: ["todo", "done"] })}
        records={[card]}
        onCardClick={() => {}}
        onMove={onMove}
      />,
    );
    const cardEl = screen.getByText("Movable").closest("[draggable]");
    expect(cardEl).not.toBeNull();
    // Simulate the HTML5 DnD: dragstart stashes the id on the
    // dataTransfer, drop on the "done" column reads it back. jsdom has
    // no DataTransfer, so provide a minimal stub.
    const data: Record<string, string> = {};
    const dataTransfer = {
      setData: (k: string, v: string) => {
        data[k] = v;
      },
      getData: (k: string) => data[k] ?? "",
    };
    // Locate the "done" column container (the header label's grid cell).
    const doneColumn = screen.getByText("done").closest("div");
    expect(doneColumn).not.toBeNull();

    fireEvent.dragStart(cardEl as Element, { dataTransfer });
    fireEvent.drop(doneColumn as Element, { dataTransfer });
    expect(onMove).toHaveBeenCalledWith(card, "done");
  });
});
