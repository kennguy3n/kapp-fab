import {
  useCallback,
  useMemo,
  useState,
  type Key,
  type ReactNode,
} from "react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "./Table";
import { cn } from "../lib/cn";

/**
 * DataGrid is the higher-level alternative to `<Table>` —
 * accepts a column spec + row data and handles:
 *
 *   - **Sorting**: click a sortable header to cycle asc → desc → none.
 *     Sort comparator defaults to localeCompare for strings and
 *     subtract for numbers; callers can pass a custom `compare`.
 *   - **Selection**: optional row-selection via checkbox column,
 *     reports the selected keys via `onSelectionChange`.
 *   - **Pagination**: client-side pagination (the data prop
 *     is the full set, DataGrid slices it).  For server-side
 *     pagination, hold the page in state outside and feed the
 *     current slice in via `data`; pass `pageSize={undefined}`
 *     to disable the internal pager.
 *   - **Empty state**: when `data.length === 0`, renders an
 *     accessible empty-state row spanning all columns.
 *
 * What DataGrid does NOT do (intentional limits):
 *   - Virtualisation: for >5k row sets, integrate
 *     @tanstack/react-virtual at the call site.
 *   - Column resize / reorder / pin: keep state ownership at
 *     the call site; DataGrid is layout-only.
 *   - Inline editing: render input controls inside the column
 *     `cell` render prop yourself.
 *
 * This split — primitives in `<Table>`, common defaults in
 * `<DataGrid>`, escape hatches at the call site — is the
 * shadcn-style "components are owned by your app" philosophy.
 */

export interface DataGridColumn<TRow> {
  /** Stable key — used as React key and sort identity. */
  key: string;
  /** Header cell content. */
  header: ReactNode;
  /**
   * Renders the body cell for a given row.  Pass a render
   * function rather than a field name so the data shape can
   * be anything (object, tuple, Map, etc.).
   */
  cell: (row: TRow, index: number) => ReactNode;
  /** Enables sort UI on the column header. */
  sortable?: boolean;
  /**
   * Optional sort comparator. Defaults to:
   *   - number subtraction when both `cell` results are numbers,
   *   - String(localeCompare) otherwise.
   * Caller-provided comparators take precedence.
   */
  compare?: (a: TRow, b: TRow) => number;
  /** Column-specific class for body cells. */
  className?: string;
  /** Column-specific class for the header cell. */
  headerClassName?: string;
}

export type DataGridSortState<TRow> = {
  columnKey: string;
  direction: "asc" | "desc";
  compare?: (a: TRow, b: TRow) => number;
} | null;

export interface DataGridProps<TRow> {
  /** Full row set.  DataGrid clones internally for sorting. */
  data: TRow[];
  /** Column spec.  Stable key per column. */
  columns: DataGridColumn<TRow>[];
  /** Stable key extractor.  Used for selection and React keys. */
  rowKey: (row: TRow, index: number) => Key;
  /**
   * Client-side page size.  Set to 0 / undefined to disable
   * pagination (renders all rows).
   */
  pageSize?: number;
  /** Current page (0-indexed) — controllable. */
  page?: number;
  onPageChange?: (page: number) => void;
  /** Set of selected row keys.  When set, renders the selection column. */
  selectedKeys?: Set<Key>;
  onSelectionChange?: (next: Set<Key>) => void;
  /** Rendered when `data` is empty.  Defaults to "No rows to display." */
  emptyState?: ReactNode;
  /** Optional class applied to the root container. */
  className?: string;
  /**
   * When a row is clicked, fires with the row data.  Pair with
   * a router-side handler to navigate to a detail page.
   */
  onRowClick?: (row: TRow, index: number) => void;
}

const DEFAULT_COMPARE = <TRow,>(
  col: DataGridColumn<TRow>,
): ((a: TRow, b: TRow) => number) =>
  col.compare ??
  ((a: TRow, b: TRow) => {
    // Render the column to a representable value, then compare.
    // We cast to unknown then check the runtime type because the
    // generic TRow shape isn't constrained — we just need a
    // string/number comparator that's deterministic.
    const av = col.cell(a, 0) as unknown;
    const bv = col.cell(b, 0) as unknown;
    if (typeof av === "number" && typeof bv === "number") return av - bv;
    return String(av ?? "").localeCompare(String(bv ?? ""));
  });

export function DataGrid<TRow>({
  data,
  columns,
  rowKey,
  pageSize,
  page: controlledPage,
  onPageChange,
  selectedKeys,
  onSelectionChange,
  emptyState = "No rows to display.",
  className,
  onRowClick,
}: DataGridProps<TRow>) {
  const [sort, setSort] = useState<DataGridSortState<TRow>>(null);
  const [uncontrolledPage, setUncontrolledPage] = useState(0);
  const page = controlledPage ?? uncontrolledPage;

  const handleSort = useCallback(
    (col: DataGridColumn<TRow>) => {
      if (!col.sortable) return;
      setSort((prev) => {
        if (!prev || prev.columnKey !== col.key) {
          return {
            columnKey: col.key,
            direction: "asc",
            compare: DEFAULT_COMPARE(col),
          };
        }
        if (prev.direction === "asc") {
          return { ...prev, direction: "desc" };
        }
        return null;
      });
    },
    [],
  );

  const sortedData = useMemo(() => {
    if (!sort) return data;
    const compare = sort.compare!;
    const copy = data.slice();
    copy.sort((a, b) =>
      sort.direction === "asc" ? compare(a, b) : compare(b, a),
    );
    return copy;
  }, [data, sort]);

  const totalPages =
    pageSize && pageSize > 0 ? Math.max(1, Math.ceil(sortedData.length / pageSize)) : 1;
  const pageStart = pageSize && pageSize > 0 ? page * pageSize : 0;
  const pageEnd =
    pageSize && pageSize > 0 ? pageStart + pageSize : sortedData.length;
  const pageRows = sortedData.slice(pageStart, pageEnd);

  const setPage = useCallback(
    (next: number) => {
      if (onPageChange) onPageChange(next);
      if (controlledPage === undefined) setUncontrolledPage(next);
    },
    [controlledPage, onPageChange],
  );

  const allOnPageSelected =
    selectedKeys !== undefined &&
    pageRows.length > 0 &&
    pageRows.every((row, i) => selectedKeys.has(rowKey(row, pageStart + i)));

  const toggleAllOnPage = useCallback(() => {
    if (!onSelectionChange || !selectedKeys) return;
    const next = new Set(selectedKeys);
    if (allOnPageSelected) {
      pageRows.forEach((row, i) => next.delete(rowKey(row, pageStart + i)));
    } else {
      pageRows.forEach((row, i) => next.add(rowKey(row, pageStart + i)));
    }
    onSelectionChange(next);
  }, [
    allOnPageSelected,
    onSelectionChange,
    pageRows,
    pageStart,
    rowKey,
    selectedKeys,
  ]);

  const toggleOne = useCallback(
    (key: Key) => {
      if (!onSelectionChange || !selectedKeys) return;
      const next = new Set(selectedKeys);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      onSelectionChange(next);
    },
    [onSelectionChange, selectedKeys],
  );

  const showSelection = selectedKeys !== undefined && !!onSelectionChange;

  return (
    <div className={cn("flex flex-col gap-2", className)}>
      <Table>
        <TableHeader>
          <TableRow>
            {showSelection && (
              <TableHead className="w-9">
                <input
                  type="checkbox"
                  aria-label={
                    allOnPageSelected ? "Deselect all rows" : "Select all rows"
                  }
                  checked={allOnPageSelected}
                  onChange={toggleAllOnPage}
                  className="h-4 w-4 cursor-pointer rounded border-border accent-(--accent)"
                />
              </TableHead>
            )}
            {columns.map((col) => {
              const isSortedBy = sort?.columnKey === col.key;
              return (
                <TableHead
                  key={col.key}
                  className={cn(
                    col.sortable && "cursor-pointer select-none",
                    col.headerClassName,
                  )}
                  onClick={col.sortable ? () => handleSort(col) : undefined}
                  aria-sort={
                    isSortedBy
                      ? sort!.direction === "asc"
                        ? "ascending"
                        : "descending"
                      : col.sortable
                        ? "none"
                        : undefined
                  }
                >
                  <span className="inline-flex items-center gap-1">
                    {col.header}
                    {col.sortable && (
                      <span
                        aria-hidden="true"
                        className={cn(
                          "inline-flex h-3 w-3 items-center justify-center text-fg-subtle",
                          isSortedBy && "text-fg",
                        )}
                      >
                        {!isSortedBy && (
                          <svg
                            viewBox="0 0 24 24"
                            className="h-3 w-3"
                            fill="none"
                            stroke="currentColor"
                            strokeWidth="2"
                          >
                            <polyline points="8 9 12 5 16 9" />
                            <polyline points="8 15 12 19 16 15" />
                          </svg>
                        )}
                        {isSortedBy && sort!.direction === "asc" && (
                          <svg
                            viewBox="0 0 24 24"
                            className="h-3 w-3"
                            fill="none"
                            stroke="currentColor"
                            strokeWidth="2"
                          >
                            <polyline points="8 14 12 10 16 14" />
                          </svg>
                        )}
                        {isSortedBy && sort!.direction === "desc" && (
                          <svg
                            viewBox="0 0 24 24"
                            className="h-3 w-3"
                            fill="none"
                            stroke="currentColor"
                            strokeWidth="2"
                          >
                            <polyline points="8 10 12 14 16 10" />
                          </svg>
                        )}
                      </span>
                    )}
                  </span>
                </TableHead>
              );
            })}
          </TableRow>
        </TableHeader>
        <TableBody>
          {pageRows.length === 0 ? (
            <TableRow>
              <TableCell
                colSpan={columns.length + (showSelection ? 1 : 0)}
                className="text-center py-8 text-fg-muted"
              >
                {emptyState}
              </TableCell>
            </TableRow>
          ) : (
            pageRows.map((row, i) => {
              const absoluteIndex = pageStart + i;
              const key = rowKey(row, absoluteIndex);
              const isSelected = selectedKeys?.has(key) ?? false;
              return (
                <TableRow
                  key={String(key)}
                  data-state={isSelected ? "selected" : undefined}
                  onClick={
                    onRowClick ? () => onRowClick(row, absoluteIndex) : undefined
                  }
                  className={onRowClick ? "cursor-pointer" : undefined}
                >
                  {showSelection && (
                    <TableCell>
                      <input
                        type="checkbox"
                        aria-label={isSelected ? "Deselect row" : "Select row"}
                        checked={isSelected}
                        onClick={(e) => e.stopPropagation()}
                        onChange={() => toggleOne(key)}
                        className="h-4 w-4 cursor-pointer rounded border-border accent-(--accent)"
                      />
                    </TableCell>
                  )}
                  {columns.map((col) => (
                    <TableCell key={col.key} className={col.className}>
                      {col.cell(row, absoluteIndex)}
                    </TableCell>
                  ))}
                </TableRow>
              );
            })
          )}
        </TableBody>
      </Table>
      {pageSize && pageSize > 0 && totalPages > 1 && (
        <div className="flex items-center justify-between px-1 py-2 text-sm text-fg-muted">
          <div>
            Page {page + 1} of {totalPages} — {sortedData.length} rows
          </div>
          <div className="flex items-center gap-1">
            <button
              type="button"
              onClick={() => setPage(Math.max(0, page - 1))}
              disabled={page === 0}
              className={cn(
                "inline-flex h-7 items-center rounded-md border border-border px-2 text-sm",
                "hover:bg-bg-subtle disabled:opacity-50 disabled:pointer-events-none",
              )}
              aria-label="Previous page"
            >
              Previous
            </button>
            <button
              type="button"
              onClick={() => setPage(Math.min(totalPages - 1, page + 1))}
              disabled={page >= totalPages - 1}
              className={cn(
                "inline-flex h-7 items-center rounded-md border border-border px-2 text-sm",
                "hover:bg-bg-subtle disabled:opacity-50 disabled:pointer-events-none",
              )}
              aria-label="Next page"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
