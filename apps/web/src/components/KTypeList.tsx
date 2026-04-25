import type { KType, KRecord } from "@kapp/client";

interface KTypeListProps {
  ktype: KType;
  records: KRecord[];
  onRowClick: (record: KRecord) => void;
  // Optional multi-select affordance. When both are set the table
  // renders a leading checkbox column and toggles selection per row
  // without bubbling through to onRowClick.
  selectedIds?: ReadonlySet<string>;
  onToggleSelect?: (id: string, checked: boolean) => void;
  onToggleAll?: (checked: boolean) => void;
}

export function KTypeList({
  ktype,
  records,
  onRowClick,
  selectedIds,
  onToggleSelect,
  onToggleAll,
}: KTypeListProps) {
  const columns =
    ktype.schema?.views?.list?.columns ??
    (ktype.schema?.fields ?? []).slice(0, 4).map((f) => f.name);
  const multiSelect = !!(selectedIds && onToggleSelect);
  const allSelected =
    multiSelect && records.length > 0 && records.every((r) => selectedIds!.has(r.id));

  return (
    <table style={{ width: "100%", borderCollapse: "collapse" }}>
      <thead>
        <tr>
          {multiSelect && (
            <th style={{ width: 28, borderBottom: "1px solid #e5e7eb" }}>
              <input
                type="checkbox"
                aria-label="Select all rows"
                checked={allSelected}
                onChange={(e) => onToggleAll?.(e.target.checked)}
              />
            </th>
          )}
          {columns.map((c) => (
            <th key={c} style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              {c}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {records.map((r) => (
          <tr
            key={r.id}
            onClick={() => onRowClick(r)}
            style={{ cursor: "pointer", borderBottom: "1px solid #f3f4f6" }}
          >
            {multiSelect && (
              <td
                style={{ padding: "6px 4px" }}
                onClick={(e) => e.stopPropagation()}
              >
                <input
                  type="checkbox"
                  aria-label={`Select row ${r.id}`}
                  checked={selectedIds!.has(r.id)}
                  onChange={(e) => onToggleSelect!(r.id, e.target.checked)}
                />
              </td>
            )}
            {columns.map((c) => (
              <td key={c} style={{ padding: "6px 4px" }}>
                {String((r.data as Record<string, unknown>)[c] ?? "")}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
