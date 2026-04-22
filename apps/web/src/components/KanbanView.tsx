import { useMemo } from "react";
import type { KType, KRecord, FieldSpec } from "@kapp/client";

interface KanbanViewProps {
  ktype: KType;
  records: KRecord[];
  onCardClick: (record: KRecord) => void;
  onMove?: (record: KRecord, toStage: string) => void;
}

/**
 * KanbanView renders records grouped by the KType's `views.kanban.group_by`
 * field. Columns are derived from that field's enum values so the UI
 * stays in sync with the schema without additional configuration.
 *
 * Drag-and-drop fires `onMove(record, toStage)` on drop; the caller is
 * responsible for (a) PATCHing the record and (b) driving any attached
 * workflow action. We deliberately split that concern outside the
 * component so the kanban stays reusable across KTypes with and without
 * workflows.
 */
export function KanbanView({ ktype, records, onCardClick, onMove }: KanbanViewProps) {
  const kanban = ktype.schema?.views?.kanban;
  const groupBy = kanban?.group_by;
  const titleKey = kanban?.card_title ?? "name";
  const subtitleKey = kanban?.card_subtitle;

  const fields = ktype.schema?.fields ?? [];
  const field = fields.find((f) => f.name === groupBy);

  const columns = useMemo(() => {
    if (!groupBy) return [];
    if (field?.values && field.values.length > 0) return field.values;
    // Fallback: derive columns from observed values so non-enum group_by
    // fields (e.g. string status on a legacy KType) still render.
    const seen = new Set<string>();
    for (const r of records) {
      const v = (r.data as Record<string, unknown>)[groupBy];
      if (typeof v === "string" && v !== "") seen.add(v);
    }
    return Array.from(seen);
  }, [field, groupBy, records]);

  if (!groupBy) {
    return <div>This KType has no kanban view configured.</div>;
  }

  const grouped: Record<string, KRecord[]> = {};
  for (const col of columns) grouped[col] = [];
  for (const r of records) {
    const v = (r.data as Record<string, unknown>)[groupBy];
    const key = typeof v === "string" ? v : "";
    if (!grouped[key]) grouped[key] = [];
    grouped[key].push(r);
  }

  return (
    <div
      style={{
        display: "grid",
        gridTemplateColumns: `repeat(${columns.length}, minmax(220px, 1fr))`,
        gap: 12,
        overflowX: "auto",
      }}
    >
      {columns.map((col) => (
        <KanbanColumn
          key={col}
          label={col}
          records={grouped[col] ?? []}
          titleKey={titleKey}
          subtitleKey={subtitleKey}
          fields={fields}
          onCardClick={onCardClick}
          onDrop={(recordId) => {
            const moved = records.find((r) => r.id === recordId);
            if (moved && onMove) onMove(moved, col);
          }}
        />
      ))}
    </div>
  );
}

interface ColumnProps {
  label: string;
  records: KRecord[];
  titleKey: string;
  subtitleKey?: string;
  fields: FieldSpec[];
  onCardClick: (record: KRecord) => void;
  onDrop: (recordId: string) => void;
}

function KanbanColumn({
  label,
  records,
  titleKey,
  subtitleKey,
  onCardClick,
  onDrop,
}: ColumnProps) {
  return (
    <div
      style={{
        background: "#f9fafb",
        borderRadius: 8,
        padding: 12,
        minHeight: 240,
      }}
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault();
        const id = e.dataTransfer.getData("text/plain");
        if (id) onDrop(id);
      }}
    >
      <header
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          marginBottom: 8,
        }}
      >
        <strong>{label}</strong>
        <span style={{ color: "#6b7280", fontSize: 12 }}>{records.length}</span>
      </header>
      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {records.map((r) => {
          const data = r.data as Record<string, unknown>;
          const title = String(data[titleKey] ?? r.id.slice(0, 8));
          const subtitle =
            subtitleKey != null ? String(data[subtitleKey] ?? "") : "";
          return (
            <div
              key={r.id}
              draggable
              onDragStart={(e) => e.dataTransfer.setData("text/plain", r.id)}
              onClick={() => onCardClick(r)}
              style={{
                background: "#ffffff",
                border: "1px solid #e5e7eb",
                borderRadius: 6,
                padding: 8,
                cursor: "pointer",
              }}
            >
              <div style={{ fontWeight: 500 }}>{title}</div>
              {subtitle && (
                <div style={{ color: "#6b7280", fontSize: 12 }}>{subtitle}</div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
