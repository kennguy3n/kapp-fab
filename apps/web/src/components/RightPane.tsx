import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import type { FieldSpec, KRecord, KType, WorkflowRun } from "@kapp/client";
import { Badge, Button, cn } from "@kapp/ui";
import { X } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import {
  formatValue,
  humanizeLabel,
  humanizeToken,
  isStatusField,
  recordLabel,
  relationTargetKtype,
  statusVariant,
  useRelationLabels,
  type RelationResolver,
} from "../lib/ktypeView";

interface RightPaneProps {
  ktype: KType;
  record: KRecord | null;
  onClose: () => void;
  onAction?: (action: string) => Promise<void> | void;
}

type Tab = "details" | "timeline" | "related";

/**
 * RightPane is a slide-out detail view for a KRecord. Instead of
 * navigating away from the list/kanban, clicking a row opens this
 * panel alongside the list. It surfaces:
 *   - field-by-field record detail (humanized labels, formatted values,
 *     status Badges, resolved relations),
 *   - the active workflow run's state + legal next actions (as buttons),
 *   - a transition timeline derived from workflow_run.history,
 *   - related records for KTypes with `ref` fields.
 *
 * The state shown in the header prefers the engine's authoritative
 * workflow_run.state over the heuristic derivation from record data
 * fields, and falls back to the heuristic only when no run exists yet
 * (e.g. record created but not yet transitioned).
 */
export function RightPane({ ktype, record, onClose, onAction }: RightPaneProps) {
  const fmt = useFormatter();
  const [tab, setTab] = useState<Tab>("details");

  useEffect(() => {
    // Close on Escape for keyboard parity with modal-style panes.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Reset the active tab whenever the record changes so users don't
  // land on a Timeline tab that is empty for the new selection.
  useEffect(() => {
    setTab("details");
  }, [record?.id]);

  const workflowRun = useQuery({
    queryKey: ["workflow-run", record?.id],
    queryFn: () => api.getWorkflowRun(ktype.name, record!.id),
    enabled: !!record,
  });

  const fields = ktype.schema?.fields ?? [];
  const relations = useRelationLabels(fields);
  const refFields = useMemo(
    () => fields.filter((f) => f.ref || f.type === "ref" || relationTargetKtype(f)),
    [fields],
  );

  if (!record) return null;
  const data = record.data as Record<string, unknown>;
  const workflow = ktype.schema?.workflow;

  // Prefer the authoritative engine state when a run exists; fall back
  // to the heuristic (record's workflow-related field) for records that
  // have not yet been through a transition.
  const run = workflowRun.data ?? null;
  const state = run
    ? run.state
    : (workflow && typeof data[workflow.initial_state] === "string"
        ? String(data[workflow.initial_state])
        : String(data["stage"] ?? data["status"] ?? workflow?.initial_state ?? "")) || "";
  const nextActions = (workflow?.transitions ?? []).filter((t) => t.from.includes(state));

  return (
    <aside className="sticky top-0 flex h-screen w-[380px] shrink-0 flex-col gap-4 overflow-y-auto border-s border-border bg-bg-elevated p-4">
      <header className="flex items-start justify-between gap-2">
        <h3 className="min-w-0 truncate text-lg font-semibold tracking-tight text-fg">
          {recordLabel(record)}
        </h3>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Close"
          onClick={onClose}
        >
          <X className="size-4" aria-hidden />
        </Button>
      </header>

      {state && (
        <section className="space-y-1">
          <div className="text-xs font-medium text-fg-muted">Workflow state</div>
          <Badge variant={statusVariant(state)} size="sm">
            {humanizeToken(state)}
          </Badge>
        </section>
      )}

      <nav
        className="flex gap-1 border-b border-border text-sm"
        role="tablist"
      >
        <TabButton active={tab === "details"} onClick={() => setTab("details")}>
          Details
        </TabButton>
        <TabButton active={tab === "timeline"} onClick={() => setTab("timeline")}>
          Timeline
        </TabButton>
        {refFields.length > 0 && (
          <TabButton active={tab === "related"} onClick={() => setTab("related")}>
            Related
          </TabButton>
        )}
      </nav>

      {tab === "details" && (
        <DetailsTab
          fields={fields}
          record={record}
          state={state}
          relations={relations}
          fmt={fmt}
          nextActions={nextActions.map((a) => ({ action: a.action, to: a.to }))}
          onAction={onAction}
        />
      )}

      {tab === "timeline" && (
        <TimelineTab run={run} loading={workflowRun.isFetching} fmt={fmt} />
      )}

      {tab === "related" && refFields.length > 0 && (
        <RelatedTab fields={refFields} data={data} relations={relations} />
      )}
    </aside>
  );
}

function TabButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active}
      onClick={onClick}
      className={cn(
        "border-b-2 px-2.5 py-1.5 font-medium transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
        active
          ? "border-fg text-fg"
          : "border-transparent text-fg-muted hover:text-fg",
      )}
    >
      {children}
    </button>
  );
}

function DetailValue({
  field,
  record,
  relations,
  fmt,
}: {
  field: FieldSpec;
  record: KRecord;
  relations: RelationResolver;
  fmt: ReturnType<typeof useFormatter>;
}) {
  const value = record.data[field.name];

  if (relationTargetKtype(field)) {
    const label = relations.resolve(field, value);
    return <span className="text-fg">{label ?? "—"}</span>;
  }

  if (isStatusField(field) && typeof value === "string" && value !== "") {
    return (
      <Badge variant={statusVariant(value)} size="sm">
        {humanizeToken(value)}
      </Badge>
    );
  }

  return <span className="text-fg">{formatValue(field, value, record, fmt)}</span>;
}

function DetailsTab({
  fields,
  record,
  state,
  relations,
  fmt,
  nextActions,
  onAction,
}: {
  fields: FieldSpec[];
  record: KRecord;
  state: string;
  relations: RelationResolver;
  fmt: ReturnType<typeof useFormatter>;
  nextActions: Array<{ action: string; to: string }>;
  onAction?: (action: string) => Promise<void> | void;
}) {
  return (
    <>
      <section className="space-y-2">
        <div className="text-xs font-medium text-fg-muted">Details</div>
        <dl className="grid grid-cols-[minmax(0,9rem)_1fr] gap-x-3 gap-y-2 text-sm">
          {fields.map((f) => (
            <div key={f.name} className="contents">
              <dt className="truncate text-fg-muted" title={humanizeLabel(f.name)}>
                {humanizeLabel(f.name)}
              </dt>
              <dd className="m-0 min-w-0 break-words">
                <DetailValue
                  field={f}
                  record={record}
                  relations={relations}
                  fmt={fmt}
                />
              </dd>
            </div>
          ))}
        </dl>
      </section>

      {nextActions.length > 0 && state && (
        <section className="space-y-2">
          <div className="text-xs font-medium text-fg-muted">Actions</div>
          <div className="flex flex-wrap gap-2">
            {nextActions.map((a) => (
              <Button
                key={a.action}
                size="sm"
                variant="secondary"
                onClick={() => onAction?.(a.action)}
                disabled={!onAction}
              >
                {humanizeToken(a.action)} → {humanizeToken(a.to)}
              </Button>
            ))}
          </div>
        </section>
      )}
    </>
  );
}

function TimelineTab({
  run,
  loading,
  fmt,
}: {
  run: WorkflowRun | null;
  loading: boolean;
  fmt: ReturnType<typeof useFormatter>;
}) {
  if (loading && !run) {
    return <div className="text-xs text-fg-muted">Loading run…</div>;
  }
  if (!run) {
    return (
      <div className="text-xs text-fg-muted">
        No workflow run yet. Transitions will appear here once the record is
        advanced.
      </div>
    );
  }
  const history = run.history ?? [];
  if (history.length === 0) {
    return (
      <div className="text-xs text-fg-muted">
        Run started in <strong>{humanizeToken(run.state)}</strong>. No
        transitions recorded yet.
      </div>
    );
  }
  return (
    <section className="space-y-2">
      <div className="text-xs font-medium text-fg-muted">Transitions</div>
      <ol className="m-0 flex list-none flex-col gap-3 border-s-2 border-border ps-3">
        {[...history].reverse().map((h, idx) => (
          <li key={idx}>
            <div className="text-sm font-medium text-fg">
              {humanizeToken(h.from_state)} → {humanizeToken(h.to_state)}
            </div>
            <div className="text-xs text-fg-muted">
              {humanizeToken(h.action)} · {fmt.dateTime(new Date(h.timestamp))}
            </div>
            <div className="text-[11px] text-fg-subtle">by {h.actor_id}</div>
          </li>
        ))}
      </ol>
    </section>
  );
}

function RelatedTab({
  fields,
  data,
  relations,
}: {
  fields: FieldSpec[];
  data: Record<string, unknown>;
  relations: RelationResolver;
}) {
  return (
    <section className="space-y-2">
      <div className="text-xs font-medium text-fg-muted">Related records</div>
      <ul className="m-0 flex list-none flex-col gap-2 p-0 text-sm">
        {fields.map((f) => {
          const value = data[f.name];
          const target = f.ref || f.ktype || relationTargetKtype(f) || "";
          const label = relations.resolve(f, value);
          if (!value) {
            return (
              <li key={f.name} className="text-fg-subtle">
                <span className="text-fg-muted">{humanizeLabel(f.name)}</span>: —
              </li>
            );
          }
          const id = String(value);
          return (
            <li key={f.name}>
              <span className="text-fg-muted">{humanizeLabel(f.name)}</span>:{" "}
              {target ? (
                <a
                  className="text-accent hover:underline"
                  href={`/records/${encodeURIComponent(target)}/${encodeURIComponent(id)}`}
                >
                  {label ?? humanizeLabel(target.split(".").pop() ?? target)}
                </a>
              ) : (
                (label ?? id)
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
