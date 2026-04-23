import { useMemo, useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord, SavedView } from "@kapp/client";
import { api } from "../lib/api";
import { KTypeList } from "../components/KTypeList";
import { KanbanView } from "../components/KanbanView";
import { RightPane } from "../components/RightPane";

type ViewMode = "list" | "kanban";

// NEW_VIEW_ID is the sentinel the dropdown uses to represent "no saved
// view selected". An empty string would collide with the UUID type so
// we pick a non-UUID literal the API rejects on lookup anyway.
const NEW_VIEW_ID = "__default__";

/**
 * RecordListPage is the tenant-scoped browse view for a KType. It
 * supports list + kanban modes, an inline right-pane detail view,
 * and a Phase G "saved views" dropdown that persists the operator's
 * filter/sort selection across sessions. The applied view is threaded
 * into the records query key so toggling views refetches rather than
 * silently reusing stale rows.
 */
export function RecordListPage({ defaultMode }: { defaultMode?: ViewMode } = {}) {
  const { ktype } = useParams<{ ktype: string }>();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const ktypeQuery = useQuery({
    queryKey: ["ktype", ktype],
    queryFn: () => api.getKType(ktype!),
    enabled: !!ktype,
  });

  const viewsQuery = useQuery({
    queryKey: ["views", ktype],
    queryFn: () => api.listViews(ktype!),
    enabled: !!ktype,
  });

  // Selected view id. The effective view defaults to the caller's
  // flagged default (one per user+ktype, enforced in the store)
  // when available, so returning users land on their curated list.
  const [selectedViewId, setSelectedViewId] = useState<string | null>(null);
  const activeView: SavedView | null = useMemo(() => {
    const all = viewsQuery.data ?? [];
    if (selectedViewId && selectedViewId !== NEW_VIEW_ID) {
      return all.find((v) => v.id === selectedViewId) ?? null;
    }
    if (selectedViewId === NEW_VIEW_ID) return null;
    return all.find((v) => v.is_default) ?? null;
  }, [viewsQuery.data, selectedViewId]);

  const recordsQuery = useQuery({
    queryKey: ["records", ktype, activeView?.id ?? NEW_VIEW_ID],
    queryFn: () => api.listRecords(ktype!),
    enabled: !!ktype,
  });

  // Filter + sort happen client-side so the dropdown feels immediate.
  // When the server grows richer list params we can thread filters
  // into api.listRecords and drop this local pass.
  const records = useMemo(() => {
    const rows = recordsQuery.data ?? [];
    const filtered = activeView?.filters
      ? rows.filter((r) => matchesFilters(r, activeView.filters))
      : rows;
    if (activeView?.sort) {
      return sortRecords(filtered, activeView.sort);
    }
    return filtered;
  }, [recordsQuery.data, activeView]);

  const createViewMutation = useMutation({
    mutationFn: (input: { name: string; filters: Record<string, unknown>; sort: string }) =>
      api.createView({ ktype: ktype!, ...input }),
    onSuccess: (v) => {
      qc.invalidateQueries({ queryKey: ["views", ktype] });
      setSelectedViewId(v.id);
    },
  });

  const deleteViewMutation = useMutation({
    mutationFn: (id: string) => api.deleteView(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["views", ktype] });
      setSelectedViewId(NEW_VIEW_ID);
    },
  });

  const [selected, setSelected] = useState<KRecord | null>(null);
  const hasKanban = !!ktypeQuery.data?.schema?.views?.kanban;
  const [modeOverride, setModeOverride] = useState<ViewMode | null>(null);
  const mode: ViewMode =
    modeOverride ?? defaultMode ?? (hasKanban ? "kanban" : "list");

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

  const handleSaveView = () => {
    const name = window.prompt("Name this view");
    if (!name) return;
    // Without an in-page filter editor we seed new views with an
    // empty predicate; toggling columns/sort from list headers
    // later is a PATCH. The server treats {} as "match everything"
    // so saving "all records" is the zero-effort default.
    createViewMutation.mutate({ name, filters: {}, sort: "" });
  };

  const handleDeleteView = () => {
    if (!activeView) return;
    if (!window.confirm(`Delete view "${activeView.name}"?`)) return;
    deleteViewMutation.mutate(activeView.id);
  };

  if (!ktype) return null;
  if (ktypeQuery.isLoading || recordsQuery.isLoading) return <div>Loading…</div>;
  if (ktypeQuery.error) return <div>Error loading KType.</div>;
  if (!ktypeQuery.data) return <div>KType not found.</div>;

  const kt = ktypeQuery.data;
  const views = viewsQuery.data ?? [];

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
          <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
            <label style={{ display: "flex", gap: 4, alignItems: "center" }}>
              View:
              <select
                aria-label="Saved view"
                value={activeView?.id ?? NEW_VIEW_ID}
                onChange={(e) => setSelectedViewId(e.target.value)}
              >
                <option value={NEW_VIEW_ID}>All records</option>
                {views.map((v) => (
                  <option key={v.id} value={v.id}>
                    {v.name}
                    {v.is_default ? " (default)" : ""}
                    {v.shared ? " — shared" : ""}
                  </option>
                ))}
              </select>
            </label>
            <button onClick={handleSaveView} disabled={createViewMutation.isPending}>
              Save view
            </button>
            {activeView && (
              <button onClick={handleDeleteView} disabled={deleteViewMutation.isPending}>
                Delete view
              </button>
            )}
            {hasKanban && (
              <div role="tablist" style={{ display: "flex", gap: 4 }}>
                <button
                  onClick={() => setModeOverride("list")}
                  aria-pressed={mode === "list"}
                >
                  List
                </button>
                <button
                  onClick={() => setModeOverride("kanban")}
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

// matchesFilters checks each top-level key in the filter against the
// record's `data` payload. Equality semantics mirror the BaseTable
// filter primitives: missing keys match (the predicate is undefined),
// present keys match when the value is exactly equal or, for an
// array filter value, when the record value is one of the array.
function matchesFilters(r: KRecord, filters: Record<string, unknown>): boolean {
  const data = r.data as Record<string, unknown>;
  for (const [key, expected] of Object.entries(filters)) {
    if (expected === undefined || expected === null || expected === "") continue;
    const actual = data[key];
    if (Array.isArray(expected)) {
      if (!expected.includes(actual as string | number)) return false;
    } else if (actual !== expected) {
      return false;
    }
  }
  return true;
}

// sortRecords applies the saved view's `sort` spec. The format is a
// comma-separated list of field names, each optionally prefixed with
// `-` for descending. Unknown fields fall through without error so
// evolving a KType never breaks a legacy saved view.
function sortRecords(rows: KRecord[], spec: string): KRecord[] {
  const keys = spec
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => (s.startsWith("-") ? { key: s.slice(1), dir: -1 } : { key: s, dir: 1 }));
  if (!keys.length) return rows;
  const out = [...rows];
  out.sort((a, b) => {
    for (const { key, dir } of keys) {
      const av = (a.data as Record<string, unknown>)[key];
      const bv = (b.data as Record<string, unknown>)[key];
      if (av === bv) continue;
      if (av === undefined || av === null) return 1 * dir;
      if (bv === undefined || bv === null) return -1 * dir;
      if (av < bv) return -1 * dir;
      if (av > bv) return 1 * dir;
    }
    return 0;
  });
  return out;
}
