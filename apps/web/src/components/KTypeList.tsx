import { useEffect, useMemo, useState } from "react";
import type { FieldSpec, KType, KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
  cn,
} from "@kapp/ui";
import {
  ArrowDown,
  ArrowUp,
  ChevronsUpDown,
  MoreHorizontal,
  Pencil,
  SquareArrowOutUpRight,
  Trash2,
} from "lucide-react";
import { useFormatter } from "../lib/i18n";
import {
  formatValue,
  humanizeLabel,
  humanizeToken,
  isNumericField,
  isStatusField,
  relationTargetKtype,
  statusVariant,
  useRelationLabels,
} from "../lib/ktypeView";

export type Density = "comfortable" | "compact";

interface KTypeListProps {
  ktype: KType;
  records: KRecord[];
  onRowClick: (record: KRecord) => void;
  density?: Density;
  // Optional explicit column set (e.g. the operator's Columns menu
  // selection). Falls back to the KType's list view / first fields.
  columns?: string[];
  // Optional multi-select affordance. When both are set the table
  // renders a leading checkbox column and toggles selection per row
  // without bubbling through to onRowClick.
  selectedIds?: ReadonlySet<string>;
  onToggleSelect?: (id: string, checked: boolean) => void;
  onToggleAll?: (checked: boolean) => void;
  onEditRow?: (record: KRecord) => void;
  onDeleteRow?: (record: KRecord) => void;
}

type SortState = { key: string; dir: "asc" | "desc" } | null;

export function KTypeList({
  ktype,
  records,
  onRowClick,
  density = "comfortable",
  columns: columnsProp,
  selectedIds,
  onToggleSelect,
  onToggleAll,
  onEditRow,
  onDeleteRow,
}: KTypeListProps) {
  const fmt = useFormatter();
  const fields = ktype.schema?.fields ?? [];
  const relations = useRelationLabels(fields);

  const columns =
    columnsProp ??
    ktype.schema?.views?.list?.columns ??
    fields.slice(0, 4).map((f) => f.name);
  // A stable key over the column contents so effects can react to the
  // operator hiding/showing columns without depending on the array's
  // identity (which changes every render).
  const columnsKey = columns.join("\u0000");

  const fieldByName = useMemo(() => {
    const map = new Map<string, FieldSpec>();
    for (const field of fields) map.set(field.name, field);
    return map;
  }, [fields]);

  const fieldFor = (name: string): FieldSpec =>
    fieldByName.get(name) ?? { name, type: "string" };

  const [sort, setSort] = useState<SortState>(null);

  // If the active sort column is hidden via the Columns menu, drop the
  // sort so records aren't left in an unexplained order with no visible
  // sort indicator on any header.
  useEffect(() => {
    if (sort && !columns.includes(sort.key)) setSort(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [columnsKey, sort]);

  const sortedRecords = useMemo(() => {
    if (!sort) return records;
    const field = fieldFor(sort.key);
    const numeric = isNumericField(field);
    const dir = sort.dir === "asc" ? 1 : -1;
    return [...records].sort((a, b) => {
      const av = a.data[sort.key];
      const bv = b.data[sort.key];
      if (av == null && bv == null) return 0;
      if (av == null) return 1;
      if (bv == null) return -1;
      if (numeric) return (Number(av) - Number(bv)) * dir;
      return String(av).localeCompare(String(bv)) * dir;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [records, sort, fieldByName]);

  const multiSelect = !!(selectedIds && onToggleSelect);
  const allSelected =
    multiSelect &&
    records.length > 0 &&
    records.every((r) => selectedIds!.has(r.id));
  const someSelected =
    multiSelect && !allSelected && records.some((r) => selectedIds!.has(r.id));
  const hasRowActions = !!(onEditRow || onDeleteRow);

  const cellPad = density === "compact" ? "py-1.5" : "py-3";

  function toggleSort(key: string) {
    setSort((prev) => {
      if (!prev || prev.key !== key) return { key, dir: "asc" };
      if (prev.dir === "asc") return { key, dir: "desc" };
      return null;
    });
  }

  function renderCell(field: FieldSpec, record: KRecord) {
    const value = record.data[field.name];

    if (relationTargetKtype(field)) {
      const label = relations.resolve(field, value);
      return (
        <span className="block truncate text-fg" title={label ?? undefined}>
          {label ?? "—"}
        </span>
      );
    }

    if (isStatusField(field) && value != null && value !== "") {
      const token = String(value);
      return (
        <Badge variant={statusVariant(token)}>{humanizeToken(token)}</Badge>
      );
    }

    const text = formatValue(field, value, record, fmt);
    return (
      <span className="block truncate text-fg" title={text === "—" ? undefined : text}>
        {text}
      </span>
    );
  }

  return (
    <div className="overflow-auto rounded-xl border border-border bg-bg-elevated">
      <table className="w-full border-collapse text-sm font-tabular">
        <thead className="sticky top-0 z-10 bg-bg-subtle">
          <tr className="border-b border-border">
            {multiSelect && (
              <th scope="col" className="w-10 px-3 py-2.5 text-start">
                <input
                  type="checkbox"
                  aria-label="Select all rows"
                  className="size-4 cursor-pointer rounded border-border accent-accent"
                  checked={allSelected}
                  ref={(el) => {
                    if (el) el.indeterminate = someSelected;
                  }}
                  onChange={(e) => onToggleAll?.(e.target.checked)}
                />
              </th>
            )}
            {columns.map((name) => {
              const field = fieldFor(name);
              const numeric = isNumericField(field);
              const active = sort?.key === name;
              return (
                <th
                  key={name}
                  scope="col"
                  className={cn(
                    "px-3 py-2.5 font-medium text-fg-muted",
                    numeric ? "text-end" : "text-start",
                  )}
                >
                  <button
                    type="button"
                    onClick={() => toggleSort(name)}
                    className={cn(
                      "inline-flex items-center gap-1 rounded-md outline-none hover:text-fg focus-visible:ring-2 focus-visible:ring-ring",
                      numeric && "flex-row-reverse",
                      active && "text-fg",
                    )}
                  >
                    <span className="truncate">{humanizeLabel(name)}</span>
                    {active ? (
                      sort!.dir === "asc" ? (
                        <ArrowUp className="size-3.5 shrink-0" aria-hidden />
                      ) : (
                        <ArrowDown className="size-3.5 shrink-0" aria-hidden />
                      )
                    ) : (
                      <ChevronsUpDown
                        className="size-3.5 shrink-0 opacity-0 group-hover:opacity-100"
                        aria-hidden
                      />
                    )}
                  </button>
                </th>
              );
            })}
            {hasRowActions && (
              <th scope="col" className="w-12 px-3 py-2.5">
                <span className="sr-only">Row actions</span>
              </th>
            )}
          </tr>
        </thead>
        <tbody>
          {sortedRecords.map((record) => (
            <tr
              key={record.id}
              onClick={() => onRowClick(record)}
              className="group cursor-pointer border-b border-border last:border-b-0 hover:bg-bg-subtle"
            >
              {multiSelect && (
                <td
                  className="w-10 px-3 align-middle"
                  onClick={(e) => e.stopPropagation()}
                >
                  <input
                    type="checkbox"
                    aria-label={`Select row ${record.id}`}
                    className="size-4 cursor-pointer rounded border-border accent-accent"
                    checked={selectedIds!.has(record.id)}
                    onChange={(e) => onToggleSelect!(record.id, e.target.checked)}
                  />
                </td>
              )}
              {columns.map((name, colIndex) => {
                const field = fieldFor(name);
                const numeric = isNumericField(field);
                return (
                  <td
                    key={name}
                    className={cn(
                      "max-w-[22rem] px-3 align-middle",
                      cellPad,
                      numeric ? "text-end tabular-nums" : "text-start",
                      colIndex === 0 && "font-medium",
                    )}
                  >
                    {renderCell(field, record)}
                  </td>
                );
              })}
              {hasRowActions && (
                <td
                  className="w-12 px-3 align-middle text-end"
                  onClick={(e) => e.stopPropagation()}
                >
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <Button
                        variant="ghost"
                        size="icon"
                        aria-label="Row actions"
                        className="size-8 opacity-60 group-hover:opacity-100"
                      >
                        <MoreHorizontal className="size-4" aria-hidden />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuItem onSelect={() => onRowClick(record)}>
                        <SquareArrowOutUpRight
                          className="size-4"
                          aria-hidden
                        />
                        Open
                      </DropdownMenuItem>
                      {onEditRow && (
                        <DropdownMenuItem onSelect={() => onEditRow(record)}>
                          <Pencil className="size-4" aria-hidden />
                          Edit
                        </DropdownMenuItem>
                      )}
                      {onDeleteRow && (
                        <>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem
                            className="text-danger focus:bg-danger/10 focus:text-danger"
                            onSelect={() => onDeleteRow(record)}
                          >
                            <Trash2 className="size-4" aria-hidden />
                            Delete
                          </DropdownMenuItem>
                        </>
                      )}
                    </DropdownMenuContent>
                  </DropdownMenu>
                </td>
              )}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
