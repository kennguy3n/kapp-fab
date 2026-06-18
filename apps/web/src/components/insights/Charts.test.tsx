import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { ReportResult } from "@kapp/client";
import { Viz } from "./Charts";

// The recharts SVG renderers need a sized container; the test setup
// stubs getBoundingClientRect to 600x400 and ResizeObserver so
// ResponsiveContainer measures non-zero. These tests assert the Viz
// dispatch picks the right renderer and that the DOM-rendering vizzes
// (table / pivot / number_card) emit the expected content. The pure
// SVG charts (bar/line/pie/...) are asserted on container presence
// only — recharts' internal SVG geometry is its own concern.

const RESULT: ReportResult = {
  columns: ["stage", "count"],
  rows: [
    { stage: "open", count: 3 },
    { stage: "won", count: 7 },
  ],
};

describe("Viz dispatch + renderers", () => {
  it("renders a number_card with the summed value and a label", () => {
    render(
      <Viz
        vizType="number_card"
        result={RESULT}
        config={{ value_column: "count", title: "Deals" }}
      />,
    );
    // 3 + 7 summed across rows.
    expect(screen.getByText("10")).toBeInTheDocument();
    expect(screen.getByText("Deals")).toBeInTheDocument();
  });

  it("renders a single-row number_card as that row's value (not a sum)", () => {
    render(
      <Viz
        vizType="number_card"
        result={{ columns: ["count"], rows: [{ count: 42 }] }}
        config={{ value_column: "count" }}
      />,
    );
    expect(screen.getByText("42")).toBeInTheDocument();
  });

  it("formats a currency number_card via Intl", () => {
    render(
      <Viz
        vizType="number_card"
        result={{ columns: ["amt"], rows: [{ amt: 1500 }] }}
        config={{ value_column: "amt", format: "currency" }}
      />,
    );
    expect(screen.getByText(/\$1,500/)).toBeInTheDocument();
  });

  it("renders a table with column headers and a cell per row", () => {
    render(<Viz vizType="table" result={RESULT} />);
    // Headers are humanized field labels; enum-token cells are humanized too.
    expect(screen.getByRole("columnheader", { name: "Stage" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Count" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "Open" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "Won" })).toBeInTheDocument();
  });

  it("falls back to a table for an unknown viz type via the default branch", () => {
    // @ts-expect-error — exercising the default switch arm with an
    // out-of-union value the runner could theoretically send.
    render(<Viz vizType="treemap" result={RESULT} />);
    expect(screen.getByRole("table")).toBeInTheDocument();
  });

  it("renders the pivot grid when the result carries a pivot block", () => {
    render(
      <Viz
        vizType="pivot"
        result={{
          columns: ["stage", "region", "count"],
          rows: [],
          pivot: {
            column_headers: ["East", "West"],
            row_headers: ["open", "won"],
            cells: [
              [1, 2],
              [3, 4],
            ],
          },
        }}
      />,
    );
    expect(screen.getByText("East")).toBeInTheDocument();
    expect(screen.getByText("West")).toBeInTheDocument();
    expect(screen.getByText("Open")).toBeInTheDocument();
    // A pivot cell value.
    expect(screen.getByText("4")).toBeInTheDocument();
  });

  it("falls back to a plain table when a pivot viz has no pivot block", () => {
    render(<Viz vizType="pivot" result={RESULT} />);
    expect(screen.getByRole("columnheader", { name: "Stage" })).toBeInTheDocument();
  });

  it.each(["bar", "line", "pie", "donut", "funnel"] as const)(
    "mounts the %s chart container without throwing",
    (vizType) => {
      const { container } = render(<Viz vizType={vizType} result={RESULT} />);
      expect(
        container.querySelector(".recharts-responsive-container"),
      ).not.toBeNull();
    },
  );
});
