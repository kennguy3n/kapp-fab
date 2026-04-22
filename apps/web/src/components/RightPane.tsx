import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import type { FieldSpec, KRecord, KType, WorkflowRun } from "@kapp/client";
import { api } from "../lib/api";

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
 *   - field-by-field record detail,
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
  const refFields = useMemo(
    () => fields.filter((f) => f.ref || f.type === "ref"),
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
    <aside
      style={{
        width: 380,
        borderLeft: "1px solid #e5e7eb",
        padding: 16,
        background: "#ffffff",
        display: "flex",
        flexDirection: "column",
        gap: 16,
        position: "sticky",
        top: 0,
        height: "100vh",
        overflowY: "auto",
      }}
    >
      <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h3 style={{ margin: 0 }}>
          {String(data["name"] ?? data["title"] ?? record.id.slice(0, 8))}
        </h3>
        <button onClick={onClose} aria-label="Close">
          ×
        </button>
      </header>

      {state && (
        <section>
          <div style={{ color: "#6b7280", fontSize: 12 }}>Workflow state</div>
          <div style={{ fontWeight: 500 }}>{state}</div>
        </section>
      )}

      <nav
        style={{
          display: "flex",
          gap: 4,
          borderBottom: "1px solid #e5e7eb",
          fontSize: 13,
        }}
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
          data={data}
          state={state}
          nextActions={nextActions.map((a) => ({ action: a.action, to: a.to }))}
          onAction={onAction}
        />
      )}

      {tab === "timeline" && <TimelineTab run={run} loading={workflowRun.isFetching} />}

      {tab === "related" && refFields.length > 0 && (
        <RelatedTab fields={refFields} data={data} />
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
      onClick={onClick}
      style={{
        background: "transparent",
        border: "none",
        borderBottom: active ? "2px solid #111827" : "2px solid transparent",
        padding: "6px 10px",
        fontWeight: active ? 600 : 400,
        cursor: "pointer",
      }}
    >
      {children}
    </button>
  );
}

function DetailsTab({
  fields,
  data,
  state,
  nextActions,
  onAction,
}: {
  fields: FieldSpec[];
  data: Record<string, unknown>;
  state: string;
  nextActions: Array<{ action: string; to: string }>;
  onAction?: (action: string) => Promise<void> | void;
}) {
  return (
    <>
      <section>
        <div style={{ color: "#6b7280", fontSize: 12, marginBottom: 4 }}>Details</div>
        <dl
          style={{
            display: "grid",
            gridTemplateColumns: "auto 1fr",
            gap: "4px 12px",
            margin: 0,
          }}
        >
          {fields.map((f) => (
            <div key={f.name} style={{ display: "contents" }}>
              <dt style={{ color: "#6b7280" }}>{f.name}</dt>
              <dd style={{ margin: 0 }}>{formatValue(data[f.name])}</dd>
            </div>
          ))}
        </dl>
      </section>

      {nextActions.length > 0 && state && (
        <section>
          <div style={{ color: "#6b7280", fontSize: 12, marginBottom: 4 }}>Actions</div>
          <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
            {nextActions.map((a) => (
              <button
                key={a.action}
                onClick={() => onAction?.(a.action)}
                disabled={!onAction}
              >
                {a.action} → {a.to}
              </button>
            ))}
          </div>
        </section>
      )}
    </>
  );
}

function TimelineTab({ run, loading }: { run: WorkflowRun | null; loading: boolean }) {
  if (loading && !run) {
    return <div style={{ color: "#6b7280", fontSize: 12 }}>Loading run…</div>;
  }
  if (!run) {
    return (
      <div style={{ color: "#6b7280", fontSize: 12 }}>
        No workflow run yet. Transitions will appear here once the record is advanced.
      </div>
    );
  }
  const history = run.history ?? [];
  if (history.length === 0) {
    return (
      <div style={{ color: "#6b7280", fontSize: 12 }}>
        Run started in <strong>{run.state}</strong>. No transitions recorded yet.
      </div>
    );
  }
  return (
    <section>
      <div style={{ color: "#6b7280", fontSize: 12, marginBottom: 8 }}>Transitions</div>
      <ol
        style={{
          listStyle: "none",
          margin: 0,
          padding: 0,
          display: "flex",
          flexDirection: "column",
          gap: 10,
          borderLeft: "2px solid #e5e7eb",
          paddingLeft: 12,
        }}
      >
        {[...history].reverse().map((h, idx) => (
          <li key={idx}>
            <div style={{ fontWeight: 500 }}>
              {h.from_state} → {h.to_state}
            </div>
            <div style={{ color: "#6b7280", fontSize: 12 }}>
              {h.action} · {new Date(h.timestamp).toLocaleString()}
            </div>
            <div style={{ color: "#9ca3af", fontSize: 11 }}>by {h.actor_id}</div>
          </li>
        ))}
      </ol>
    </section>
  );
}

function RelatedTab({
  fields,
  data,
}: {
  fields: FieldSpec[];
  data: Record<string, unknown>;
}) {
  return (
    <section>
      <div style={{ color: "#6b7280", fontSize: 12, marginBottom: 4 }}>Related records</div>
      <ul
        style={{
          listStyle: "none",
          margin: 0,
          padding: 0,
          display: "flex",
          flexDirection: "column",
          gap: 8,
        }}
      >
        {fields.map((f) => {
          const value = data[f.name];
          if (!value) {
            return (
              <li key={f.name} style={{ color: "#9ca3af", fontSize: 13 }}>
                <span style={{ color: "#6b7280" }}>{f.name}</span>: —
              </li>
            );
          }
          const target = f.ref || f.ktype || "";
          const id = String(value);
          return (
            <li key={f.name} style={{ fontSize: 13 }}>
              <span style={{ color: "#6b7280" }}>{f.name}</span>:{" "}
              {target ? (
                <a href={`/records/${encodeURIComponent(target)}/${encodeURIComponent(id)}`}>
                  {target} · {id.slice(0, 8)}
                </a>
              ) : (
                id
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function formatValue(v: unknown): string {
  if (v == null) return "—";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
