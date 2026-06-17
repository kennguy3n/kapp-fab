import { useCallback } from "react";
import { useFormatter } from "../i18n";

/** Parse a backend decimal string (or number) into a JS number. */
export function parseAmount(value: string | number | null | undefined): number {
  if (value === null || value === undefined || value === "") return NaN;
  return typeof value === "number" ? value : Number(value);
}

export interface MoneyOptions {
  /** ISO-4217 code — when present the value renders with a symbol. */
  currency?: string;
  /** Render an exact zero as an em dash (for sparse debit/credit cells). */
  blankZero?: boolean;
}

/**
 * useMoney returns a locale-aware money formatter. Report figures
 * arrive as decimal strings; this renders them with grouping +
 * exactly two fraction digits (and a currency symbol when a code is
 * supplied), so a raw `120000.00` never reaches the user.
 */
export function useMoney() {
  const f = useFormatter();
  return useCallback(
    (value: string | number | null | undefined, opts: MoneyOptions = {}) => {
      const n = parseAmount(value);
      if (!Number.isFinite(n)) return "—";
      if (opts.blankZero && n === 0) return "—";
      if (opts.currency) {
        return f.currency(n, opts.currency, {
          minimumFractionDigits: 2,
          maximumFractionDigits: 2,
        });
      }
      return f.number(n, {
        minimumFractionDigits: 2,
        maximumFractionDigits: 2,
      });
    },
    [f],
  );
}

/** The money-formatting callback returned by {@link useMoney}. */
export type Money = ReturnType<typeof useMoney>;

// --- CSV export -------------------------------------------------------

type CsvCell = string | number | null | undefined;

// A bare number such as -120000.00 is data, not a formula. Guarding it
// like a formula (prefixing ') turns it into text and breaks SUM/AVERAGE
// in the exported sheet, so numeric cells are exempt from the prefix.
const NUMERIC_CELL = /^-?\d+(\.\d+)?$/;

// Escape a single CSV cell. Beyond RFC-4180 quoting we neutralise
// formula-injection vectors (a cell beginning with = + - @ tab CR is
// interpreted as a formula by spreadsheet apps) by prefixing a single
// quote, so an exported ledger can't smuggle executable content.
function escapeCsvCell(cell: CsvCell): string {
  let s = cell === null || cell === undefined ? "" : String(cell);
  if (/^[=+\-@\t\r]/.test(s) && !NUMERIC_CELL.test(s)) s = `'${s}`;
  if (/[",\n\r]/.test(s)) s = `"${s.replace(/"/g, '""')}"`;
  return s;
}

/** Serialise a header row + body rows into an RFC-4180 CSV string. */
export function toCsv(headers: string[], rows: CsvCell[][]): string {
  const lines = [headers, ...rows].map((row) =>
    row.map(escapeCsvCell).join(","),
  );
  return lines.join("\r\n");
}

/**
 * Trigger a client-side download of a CSV file. A BOM is prepended so
 * Excel opens UTF-8 content (e.g. accented account names) correctly.
 */
export function downloadCsv(
  filename: string,
  headers: string[],
  rows: CsvCell[][],
): void {
  const csv = toCsv(headers, rows);
  const blob = new Blob(["\uFEFF", csv], {
    type: "text/csv;charset=utf-8",
  });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  document.body.appendChild(anchor);
  anchor.click();
  document.body.removeChild(anchor);
  URL.revokeObjectURL(url);
}

/**
 * todayLocalISO returns YYYY-MM-DD in the viewer's local timezone.
 * `new Date().toISOString().slice(0, 10)` is off-by-one for UTC+ zones
 * because it formats the UTC instant rather than the local calendar day.
 */
export function todayLocalISO(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** Compose a date-stamped CSV filename, e.g. `trial-balance_2026-06-17.csv`. */
export function csvFilename(base: string, stamp?: string): string {
  const date = stamp ?? todayLocalISO();
  return `${base}_${date}.csv`;
}
