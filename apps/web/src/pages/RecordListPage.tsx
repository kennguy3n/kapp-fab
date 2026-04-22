import { useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";
import { KTypeList } from "../components/KTypeList";
import { KanbanView } from "../components/KanbanView";
import { RightPane } from "../components/RightPane";

type ViewMode = "list" | "kanban";

/**
 * RecordListPage is the tenant-scoped browse view for a KType. It
 * supports list + kanban modes and an inline right-pane detail view.
 * The mode defaults to kanban when the KType defines a kanban view;
 * otherwise it falls back to the classic list.
 */
export function RecordListPage() {
  const { ktype } = useParams<{ ktype: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const ktypeQuery = useQuery({
    queryKey: ["ktype", ktype],
    queryFn: () => api.getKType(ktype!),
    enabled: !!ktype,
  });

  const recordsQuery = useQuery({
    queryKey: ["records", ktype],
    queryFn: () => api.listRecords(ktype!),
    enabled: !!ktype,
  });

  const [selected, setSelected] = useState<KRecord | null>(null);
  const hasKanban = !!ktypeQuery.data?.schema?.views?.kanban;
  const [mode, setMode] = useState<ViewMode>(hasKanban ? "kanban" : "list");

  const moveMutation = useMutation({
    mutationFn: async ({
      record,
      toStage,
    }: {
      record: KRecord;
      toStage: string;
    }) => {
      const groupBy = ktypeQuery.data?.schema?.views?.kanban?.group_by;
      if (!groupBy) return;
      await api.updateRecord(ktype!, record.id, { [groupBy]: toStage });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
    },
  });

  const actionMutation = useMutation({
    mutationFn: async ({
      record,
      action,
    }: {
      record: KRecord;
      action: string;
    }) => {
      await api.runAction(ktype!, record.id, action);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
    },
  });

  if (!ktype) return null;
  if (ktypeQuery.isLoading || recordsQuery.isLoading) return <div>Loading…</div>;
  if (ktypeQuery.error) return <div>Error loading KType.</div>;
  if (!ktypeQuery.data) return <div>KType not found.</div>;

  const records = recordsQuery.data ?? [];
  const kt = ktypeQuery.data;

  return (
    <div style={{ display: "flex", gap: 16, alignItems: "flex-start" }}>
      <section style={{ flex: 1, minWidth: 0 }}>
        <header
          style={{
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            gap: 12,
          }}
        >
          <h1>{kt.name}</h1>
          <div style={{ display: "flex", gap: 8 }}>
            {hasKanban && (
              <div role="tablist" style={{ display: "flex", gap: 4 }}>
                <button
                  onClick={() => setMode("list")}
                  aria-pressed={mode === "list"}
                >
                  List
                </button>
                <button
                  onClick={() => setMode("kanban")}
                  aria-pressed={mode === "kanban"}
                >
                  Kanban
                </button>
              </div>
            )}
            <button onClick={() => navigate(`/records/${ktype}/new`)}>
              New
            </button>
          </div>
        </header>
        {mode === "kanban" && hasKanban ? (
          <KanbanView
            ktype={kt}
            records={records}
            onCardClick={(r) => setSelected(r)}
            onMove={(record, toStage) =>
              moveMutation.mutate({ record, toStage })
            }
          />
        ) : (
          <KTypeList
            ktype={kt}
            records={records}
            onRowClick={(r) => setSelected(r)}
          />
        )}
      </section>
      {selected && (
        <RightPane
          ktype={kt}
          record={selected}
          onClose={() => setSelected(null)}
          onAction={async (action) => {
            await actionMutation.mutateAsync({ record: selected, action });
            setSelected(null);
          }}
        />
      )}
    </div>
  );
}
