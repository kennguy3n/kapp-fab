import { useQuery } from "@tanstack/react-query";
import { useEffect } from "react";
import type { KType, KRecord } from "@kapp/client";
import { api } from "../lib/api";

interface RightPaneProps {
  ktype: KType;
  record: KRecord | null;
  onClose: () => void;
  onAction?: (action: string) => Promise<void> | void;
}

/**
 * RightPane is a slide-out detail view for a KRecord. Instead of
 * navigating away from the list/kanban, clicking a row opens this
 * panel alongside the list. It surfaces:
 *   - field-by-field record detail,
 *   - the active workflow run's state + legal next actions (as buttons),
 *   - lightweight audit breadcrumbs when available.
 *
 * This keeps the kanban/list visible while users drill into records —
 * matching the Frappe "quick-view" + ARCHITECTURE.md §7 UX spec.
 */
export function RightPane({ ktype, record, onClose, onAction }: RightPaneProps) {
  useEffect(() => {
    // Close on Escape for keyboard parity with modal-style panes.
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const workflowRun = useQuery({
    queryKey: ["workflow-run", record?.id],
    queryFn: () => api.getRecord(ktype.name, record!.id),
    enabled: !!record,
  });

  if (!record) return null;
  const data = record.data as Record<string, unknown>;
  const fields = ktype.schema?.fields ?? [];
  const workflow = ktype.schema?.workflow;

  // Derive "state" for the action-button list from either the record's
  // workflow-related field or the KType's workflow initial_state — we
  // don't pull the workflow_run separately here because it's cheap for
  // Phase B to read state from the record's group_by field.
  const state =
    (workflow && typeof data[workflow.initial_state] === "string"
      ? String(data[workflow.initial_state])
      : String(data["stage"] ?? data["status"] ?? workflow?.initial_state ?? "")) ||
    "";
  const nextActions = (workflow?.transitions ?? []).filter((t) =>
    t.from.includes(state),
  );

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
        <h3 style={{ margin: 0 }}>{String(data["name"] ?? data["title"] ?? record.id.slice(0, 8))}</h3>
        <button onClick={onClose} aria-label="Close">×</button>
      </header>

      {state && (
        <section>
          <div style={{ color: "#6b7280", fontSize: 12 }}>Workflow state</div>
          <div style={{ fontWeight: 500 }}>{state}</div>
        </section>
      )}

      <section>
        <div style={{ color: "#6b7280", fontSize: 12, marginBottom: 4 }}>Details</div>
        <dl style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "4px 12px", margin: 0 }}>
          {fields.map((f) => (
            <div key={f.name} style={{ display: "contents" }}>
              <dt style={{ color: "#6b7280" }}>{f.name}</dt>
              <dd style={{ margin: 0 }}>{formatValue(data[f.name])}</dd>
            </div>
          ))}
        </dl>
      </section>

      {nextActions.length > 0 && (
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

      {workflowRun.isFetching && (
        <div style={{ color: "#6b7280", fontSize: 12 }}>Loading run…</div>
      )}
    </aside>
  );
}

function formatValue(v: unknown): string {
  if (v == null) return "—";
  if (typeof v === "object") return JSON.stringify(v);
  return String(v);
}
