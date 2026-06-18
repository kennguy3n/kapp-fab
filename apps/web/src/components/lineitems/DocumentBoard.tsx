import { useMemo, useState, type ReactNode } from "react";
import type { KRecord } from "@kapp/client";
import { Badge, Button, Eyebrow, EmptyState, Skeleton } from "@kapp/ui";
import { Inbox, Plus, RotateCcw } from "lucide-react";
import { titleCase } from "./format";

export interface DocumentBoardProps {
  eyebrow: string;
  title: string;
  description: string;
  /** Label for the primary "new document" action. */
  newLabel: string;
  onNew: () => void;
  /** Ordered workflow stage ids that form the columns. */
  stages: string[];
  records: KRecord[] | undefined;
  /** Resolve a record's current stage id. */
  statusOf: (record: KRecord) => string;
  isLoading: boolean;
  isError: boolean;
  error?: unknown;
  onRetry: () => void;
  /** Move a record to a stage (drag-drop target). */
  onMove: (record: KRecord, to: string) => void;
  onCardClick: (record: KRecord) => void;
  /** Render the inner content of a card; the board owns the chrome. */
  renderCard: (record: KRecord) => ReactNode;
  emptyTitle: string;
  emptyDescription: string;
}

/**
 * DocumentBoard is the shared workflow kanban for the four
 * sales/procurement documents. It centralises the polish bar —
 * consistent page header, all four async states (skeleton / teaching
 * empty / error+retry / populated), accessible drag-and-drop cards,
 * and column counts — so each page only supplies its data, stage
 * transitions, and card body. Stages found in the data but absent
 * from `stages` are surfaced as warning columns rather than hidden,
 * so records never silently disappear.
 */
export function DocumentBoard({
  eyebrow,
  title,
  description,
  newLabel,
  onNew,
  stages,
  records,
  statusOf,
  isLoading,
  isError,
  error,
  onRetry,
  onMove,
  onCardClick,
  renderCard,
  emptyTitle,
  emptyDescription,
}: DocumentBoardProps) {
  const [dragStage, setDragStage] = useState<string | null>(null);

  const { columns, extraStages } = useMemo(() => {
    const map = new Map<string, KRecord[]>();
    for (const s of stages) map.set(s, []);
    const extras: string[] = [];
    for (const r of records ?? []) {
      const s = statusOf(r);
      if (!map.has(s)) {
        map.set(s, []);
        if (!extras.includes(s)) extras.push(s);
      }
      map.get(s)!.push(r);
    }
    return { columns: map, extraStages: extras };
  }, [records, stages, statusOf]);

  const allStages = [...stages, ...extraStages];
  const total = records?.length ?? 0;

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div className="flex flex-col gap-1">
          <Eyebrow>{eyebrow}</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">{title}</h1>
          <p className="max-w-prose text-sm text-fg-muted">{description}</p>
        </div>
        <Button
          onClick={onNew}
          leadingIcon={<Plus className="h-4 w-4" aria-hidden="true" />}
        >
          {newLabel}
        </Button>
      </header>

      {isError ? (
        <div
          role="alert"
          className="flex flex-col items-start gap-3 rounded-lg border border-danger/40 bg-danger/5 p-4"
        >
          <p className="text-sm text-danger">
            We couldn’t load this board. {(error as Error)?.message ?? ""}
          </p>
          <Button
            variant="outline"
            size="sm"
            onClick={onRetry}
            leadingIcon={<RotateCcw className="h-4 w-4" aria-hidden="true" />}
          >
            Try again
          </Button>
        </div>
      ) : isLoading ? (
        <div className="flex gap-3 overflow-x-auto pb-2">
          {stages.map((s) => (
            <div
              key={s}
              className="min-w-[260px] flex-1 rounded-lg border border-border bg-bg-subtle p-3"
            >
              <Skeleton className="mb-3 h-4 w-24" />
              <div className="flex flex-col gap-2">
                <Skeleton variant="rect" className="h-20 w-full" />
                <Skeleton variant="rect" className="h-20 w-full" />
              </div>
            </div>
          ))}
        </div>
      ) : total === 0 ? (
        <EmptyState
          icon={<Inbox aria-hidden="true" />}
          title={emptyTitle}
          description={emptyDescription}
          action={
            <Button
              onClick={onNew}
              leadingIcon={<Plus className="h-4 w-4" aria-hidden="true" />}
            >
              {newLabel}
            </Button>
          }
        />
      ) : (
        <div className="flex gap-3 overflow-x-auto pb-2">
          {allStages.map((s) => {
            const isExtra = extraStages.includes(s);
            const cards = columns.get(s) ?? [];
            return (
              <div
                key={s}
                onDragOver={(e) => {
                  e.preventDefault();
                  if (dragStage !== s) setDragStage(s);
                }}
                onDragLeave={() => setDragStage((cur) => (cur === s ? null : cur))}
                onDrop={(e) => {
                  e.preventDefault();
                  setDragStage(null);
                  const id = e.dataTransfer.getData("text/plain");
                  const r = (records ?? []).find((x) => x.id === id);
                  if (r) onMove(r, s);
                }}
                className={[
                  "min-w-[260px] flex-1 rounded-lg border p-3 transition-colors",
                  isExtra ? "border-dashed border-warning bg-warning/10" : "border-border bg-bg-subtle",
                  dragStage === s ? "ring-2 ring-(--focus-ring)" : "",
                ].join(" ")}
              >
                <div className="mb-2 flex items-center justify-between">
                  <span className="text-sm font-medium text-fg">
                    {titleCase(s)}
                    {isExtra && (
                      <span className="ms-1 text-xs font-normal text-fg-muted">
                        (unrecognized)
                      </span>
                    )}
                  </span>
                  <Badge variant={isExtra ? "warning" : "neutral"} size="xs">
                    {cards.length}
                  </Badge>
                </div>
                <div className="flex flex-col gap-2">
                  {cards.map((r) => (
                    <div
                      key={r.id}
                      role="button"
                      tabIndex={0}
                      draggable
                      onDragStart={(e) => e.dataTransfer.setData("text/plain", r.id)}
                      onClick={() => onCardClick(r)}
                      onKeyDown={(e) => {
                        if (e.key === "Enter" || e.key === " ") {
                          e.preventDefault();
                          onCardClick(r);
                        }
                      }}
                      className="cursor-pointer rounded-md border border-border bg-bg-elevated p-3 text-left shadow-sm transition-colors hover:border-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
                    >
                      {renderCard(r)}
                    </div>
                  ))}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}
