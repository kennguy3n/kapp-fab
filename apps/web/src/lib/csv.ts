/**
 * Client-side CSV export helpers shared by the inventory and
 * manufacturing list pages (Stock Levels, Valuation, Landed Cost,
 * BOM). Pages build a header row plus a matrix of string cells and
 * hand them to {@link downloadCsv}; quoting is handled per-cell so a
 * value containing a comma, quote, or newline can't break the layout.
 */

/** Quote a CSV cell only when it contains a delimiter, quote, or newline. */
export function csvCell(value: string): string {
  return /[",\n]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}

/** Trigger a client-side download of a CSV built from the given rows. */
export function downloadCsv(
  filename: string,
  headers: string[],
  rows: string[][],
) {
  const body = [headers, ...rows]
    .map((cols) => cols.map(csvCell).join(","))
    .join("\n");
  const blob = new Blob([body], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}
