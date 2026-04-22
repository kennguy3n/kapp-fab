import type { KType, KRecord } from "@kapp/client";

interface KTypeListProps {
  ktype: KType;
  records: KRecord[];
  onRowClick: (record: KRecord) => void;
}

export function KTypeList({ ktype, records, onRowClick }: KTypeListProps) {
  const columns =
    ktype.schema?.views?.list?.columns ??
    (ktype.schema?.fields ?? []).slice(0, 4).map((f) => f.name);

  return (
    <table style={{ width: "100%", borderCollapse: "collapse" }}>
      <thead>
        <tr>
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
