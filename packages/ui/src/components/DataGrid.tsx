import {
  useCallback,
  useEffect,
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

// isDevBuild gates a dev-only console.warn below.  We probe
// import.meta.env.DEV (the standard Vite-injected flag) without
// declaring ambient types for it, because consumers of this UI
// package (apps/web, apps/storybook) already pull in vite/client
// type definitions and declaring a local `ImportMetaEnv` here would
// conflict with vite/client's stricter signature.  We just cast
// import.meta through `unknown` to a minimal anonymous shape we
// read at runtime, and the check tolerates the field being absent
// (true in non-Vite builds or in jest-style runners).  In Vite
// builds the const folds to a literal and the warn-loop dead-code-
// eliminates from production bundles.
const isDevBuild: boolean = !!(
  import.meta as unknown as { env?: { DEV?: boolean } }
).env?.DEV;

/**
 * DataGrid is the higher-level alternative to `<Table>` —
 * accepts a column spec + row data and handles:
 *
 *   - **Sorting**: click a sortable header to cycle asc → desc → none.
 *     A column marked `sortable: true` MUST also provide either
 *     an `accessor` (returns the raw scalar to compare) or a
 *     custom `compare`.  Without one of those, sort UI is
 *     suppressed and DataGrid logs a dev-mode warning — the
 *     previous design called `cell()` for comparison, which
 *     silently produced `[object Object]` for JSX cells and made
 *     sort a no-op.
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

/**
 * Sortable scalar — what an `accessor` callback (or the default
 * comparator) hands to the sort compare function.  We restrict the
 * type to the small set of primitives that have a sensible total
 * order: strings sort by localeCompare, numbers and bigints by
 * subtraction, booleans by !!a - !!b, dates by getTime(), and
 * null/undefined sort last regardless of direction.  Returning a
 * React element from `accessor` is a static type error — that is
 * the whole point of the type narrowing, and it prevents the
 * "sort by JSX" footgun the old default-comparator had.
 */
export type DataGridSortValue =
  | string
  | number
  | bigint
  | boolean
  | Date
  | null
  | undefined;

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
   * Extracts the **raw scalar value** used for sorting.  This is
   * intentionally separate from `cell` because `cell` returns
   * `ReactNode` (often JSX), and stringifying JSX yields
   * `[object Object]` which makes every row compare equal.
   *
   * If both `accessor` and `compare` are provided, `compare` wins.
   *
   * If a column sets `sortable: true` but provides NEITHER
   * `accessor` nor `compare`, `<DataGrid>` will log a console
   * warning at mount and skip sorting for that column rather than
   * silently producing the broken `[object Object]` behaviour.
   */
  accessor?: (row: TRow) => DataGridSortValue;
  /**
   * Optional fully-custom comparator.  Use when sort order is
   * non-monotonic in any single scalar (e.g. status-tier ranking,
   * locale-specific collation, multi-field tiebreakers).  Takes
   * precedence over `accessor`.
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

/**
 * compareSortValues totally orders two `DataGridSortValue`s.  null
 * and undefined always sort last (the same way SQL `ORDER BY ...
 * NULLS LAST` works) regardless of asc/desc direction — this is
 * the conventional UX for data tables and matches Postgres'
 * default for descending sorts.  The asc-vs-desc inversion is
 * applied by the caller around this primitive comparator; the
 * null-handling stays unidirectional on purpose so empty cells
 * always cluster at the bottom of the view.
 */
function compareSortValues(
  a: DataGridSortValue,
  b: DataGridSortValue,
): number {
  const aNull = a === null || a === undefined;
  const bNull = b === null || b === undefined;
  if (aNull && bNull) return 0;
  if (aNull) return 1;
  if (bNull) return -1;
  if (typeof a === "number" && typeof b === "number") return a - b;
  if (typeof a === "bigint" && typeof b === "bigint")
    return a < b ? -1 : a > b ? 1 : 0;
  if (typeof a === "boolean" && typeof b === "boolean")
    return Number(a) - Number(b);
  if (a instanceof Date && b instanceof Date) return a.getTime() - b.getTime();
  return String(a).localeCompare(String(b));
}

/**
 * resolveCompare returns the comparator a sortable column will
 * use, or `null` if the column declared `sortable: true` without
 * an `accessor` or `compare`.  The caller is expected to skip
 * sort UI for that column (handleSort early-returns).  We chose a
 * runtime guard over a type-level constraint because forcing
 * `accessor` or `compare` into the discriminated union would
 * cost ergonomics for the common case (most columns are not
 * sortable) and would require a much larger generic surface.
 */
function resolveCompare<TRow>(
  col: DataGridColumn<TRow>,
): ((a: TRow, b: TRow) => number) | null {
  if (col.compare) return col.compare;
  if (col.accessor) {
    const acc = col.accessor;
    return (a, b) => compareSortValues(acc(a), acc(b));
  }
  return null;
}

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

  // Surface misconfigured columns at mount + when the spec changes.
  // `sortable: true` without an accessor or compare is a silent
  // footgun (the previous implementation rendered the column with
  // sort UI but every row compared equal), so warn loudly in dev.
  // In production the same column simply renders without a sort
  // affordance — see `effectiveSortable` below.
  useEffect(() => {
    // Dev-mode-only guard.  We detect dev via Vite's import.meta.env
    // because both consumers (apps/web, apps/storybook) are Vite
    // builds and import.meta.env.DEV is statically replaced at
    // build time, so the warning + the surrounding `for` loop dead-
    // code-eliminate to nothing in production bundles.  We avoid
    // `process.env.NODE_ENV` because that requires @types/node which
    // this UI package deliberately does not pull in (it is a
    // browser-target library).
    if (!isDevBuild) return;
    for (const col of columns) {
      if (col.sortable && !col.accessor && !col.compare) {
        // eslint-disable-next-line no-console
        console.warn(
          `[DataGrid] column "${col.key}" has \`sortable: true\` but ` +
            "no `accessor` or `compare`. Sort UI will be disabled for " +
            "this column. Provide an `accessor` (returns a scalar) or " +
            "a custom `compare` to enable sorting.",
        );
      }
    }
  }, [columns]);

  const handleSort = useCallback(
    (col: DataGridColumn<TRow>) => {
      if (!col.sortable) return;
      const compare = resolveCompare(col);
      if (!compare) return; // misconfigured — already warned above
      setSort((prev) => {
        if (!prev || prev.columnKey !== col.key) {
          return {
            columnKey: col.key,
            direction: "asc",
            compare,
          };
        }
        if (prev.direction === "asc") {
          return { ...prev, direction: "desc", compare };
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

  // Clamp the page index so a parent shrinking `data` (e.g. after a
  // filter narrows the row set) doesn't strand the user on a page
  // that no longer exists.  Without this the previous page index
  // would persist and the slice below would render an empty view
  // until the user manually clicked back to page 0.  When the
  // parent owns the page state via `controlledPage`, we still
  // surface the clamp via `onPageChange` so its model stays in
  // sync with what's rendered.
  const requestedPage = controlledPage ?? uncontrolledPage;
  const page = Math.min(Math.max(0, requestedPage), totalPages - 1);
  useEffect(() => {
    if (page === requestedPage) return;
    if (onPageChange) onPageChange(page);
    if (controlledPage === undefined) setUncontrolledPage(page);
  }, [page, requestedPage, onPageChange, controlledPage]);

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
              // A column is *effectively* sortable only when it both
              // declares `sortable: true` AND has the data needed to
              // sort — either an `accessor` or a `compare`.  This
              // mirrors the useEffect warning above so the rendered
              // UI matches what's safely interactive: no chevron and
              // no click handler on misconfigured columns, even if
              // they declared `sortable: true`.
              const effectiveSortable =
                !!col.sortable && (!!col.accessor || !!col.compare);
              const isSortedBy = sort?.columnKey === col.key;
              return (
                <TableHead
                  key={col.key}
                  className={cn(
                    effectiveSortable && "cursor-pointer select-none",
                    col.headerClassName,
                  )}
                  onClick={
                    effectiveSortable ? () => handleSort(col) : undefined
                  }
                  aria-sort={
                    isSortedBy
                      ? sort!.direction === "asc"
                        ? "ascending"
                        : "descending"
                      : effectiveSortable
                        ? "none"
                        : undefined
                  }
                >
                  <span className="inline-flex items-center gap-1">
                    {col.header}
                    {effectiveSortable && (
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
