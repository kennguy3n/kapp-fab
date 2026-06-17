import { useMemo } from "react";
import type { KType, KRecord, FieldSpec } from "@kapp/client";
import { Badge, cn } from "@kapp/ui";
import { useFormatter } from "../lib/i18n";
import { formatValue, humanizeToken, recordLabel, statusVariant } from "../lib/ktypeView";

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
 * Column headers display the humanized token (e.g. `in_progress` →
 * `In Progress`) but the move callback always reports the RAW field
 * value so the caller can PATCH the record / drive the workflow.
 *
 * Drag-and-drop fires `onMove(record, toStage)` on drop; the caller is
 * responsible for (a) PATCHing the record and (b) driving any attached
 * workflow action. We deliberately split that concern outside the
 * component so the kanban stays reusable across KTypes with and without
 * workflows.
 */
export function KanbanView({ ktype, records, onCardClick, onMove }: KanbanViewProps) {
  const fmt = useFormatter();
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
    return (
      <div className="rounded-xl border border-border bg-bg-subtle px-4 py-8 text-center text-sm text-fg-muted">
        This record type has no kanban view configured.
      </div>
    );
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
    <div className="flex gap-3 overflow-x-auto pb-2">
      {columns.map((col) => (
        <KanbanColumn
          key={col}
          value={col}
          records={grouped[col] ?? []}
          titleKey={titleKey}
          subtitleKey={subtitleKey}
          fields={fields}
          fmt={fmt}
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
  value: string;
  records: KRecord[];
  titleKey: string;
  subtitleKey?: string;
  fields: FieldSpec[];
  fmt: ReturnType<typeof useFormatter>;
  onCardClick: (record: KRecord) => void;
  onDrop: (recordId: string) => void;
}

function KanbanColumn({
  value,
  records,
  titleKey,
  subtitleKey,
  fields,
  fmt,
  onCardClick,
  onDrop,
}: ColumnProps) {
  const titleField = fields.find((f) => f.name === titleKey);
  const subtitleField = subtitleKey
    ? fields.find((f) => f.name === subtitleKey)
    : undefined;

  return (
    <div
      className="flex max-h-[70vh] w-72 shrink-0 flex-col rounded-xl border border-border bg-bg-subtle"
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault();
        const id = e.dataTransfer.getData("text/plain");
        if (id) onDrop(id);
      }}
    >
      <header className="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
        <Badge variant={statusVariant(value)} size="sm">
          {humanizeToken(value)}
        </Badge>
        <span className="text-xs font-medium tabular-nums text-fg-muted">
          {records.length}
        </span>
      </header>
      <div className="flex flex-col gap-2 overflow-y-auto p-2">
        {records.map((r) => {
          const data = r.data as Record<string, unknown>;
          const rawTitle = data[titleKey];
          const title =
            rawTitle != null && rawTitle !== ""
              ? titleField
                ? formatValue(titleField, rawTitle, r, fmt)
                : String(rawTitle)
              : recordLabel(r);
          const rawSubtitle =
            subtitleKey != null ? data[subtitleKey] : undefined;
          const subtitle =
            rawSubtitle != null && rawSubtitle !== ""
              ? subtitleField
                ? formatValue(subtitleField, rawSubtitle, r, fmt)
                : String(rawSubtitle)
              : "";
          return (
            <button
              key={r.id}
              type="button"
              draggable
              onDragStart={(e) => e.dataTransfer.setData("text/plain", r.id)}
              onClick={() => onCardClick(r)}
              className={cn(
                "w-full rounded-lg border border-border bg-bg-elevated p-3 text-start",
                "transition-colors hover:border-border-strong hover:bg-bg",
                "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                "cursor-grab active:cursor-grabbing",
              )}
            >
              <div className="truncate text-sm font-medium text-fg" title={title}>
                {title}
              </div>
              {subtitle && (
                <div
                  className="mt-0.5 truncate text-xs text-fg-muted"
                  title={subtitle}
                >
                  {subtitle}
                </div>
              )}
            </button>
          );
        })}
        {records.length === 0 && (
          <p className="px-1 py-6 text-center text-xs text-fg-subtle">
            Nothing here yet
          </p>
        )}
      </div>
    </div>
  );
}
