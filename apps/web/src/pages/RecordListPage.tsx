import { useEffect, useMemo, useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord, SavedView } from "@kapp/client";
import {
  Button,
  ConfirmDialog,
  EmptyState,
  PromptDialog,
  Select,
  Skeleton,
  toast,
} from "@kapp/ui";
import { AlertTriangle, Inbox, Plus } from "lucide-react";
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

  // Dialog visibility for the prompt/confirm flows that previously
  // used window.prompt / window.confirm. The host owns the open flag;
  // each dialog hands its value/confirmation back through a callback
  // and stays open (showing the `loading` pending state) until the
  // mutation settles, at which point onSuccess/onError closes it.
  const [statusPromptOpen, setStatusPromptOpen] = useState(false);
  const [bulkDeleteOpen, setBulkDeleteOpen] = useState(false);
  const [saveViewOpen, setSaveViewOpen] = useState(false);
  const [deleteViewOpen, setDeleteViewOpen] = useState(false);

  const createViewMutation = useMutation({
    mutationFn: (input: { name: string; filters: Record<string, unknown>; sort: string }) =>
      api.createView({ ktype: ktype!, ...input }),
    onSuccess: (v) => {
      qc.invalidateQueries({ queryKey: ["views", ktype] });
      setSelectedViewId(v.id);
      setSaveViewOpen(false);
      toast.success("View saved", { description: v.name });
    },
    onError: (err) => {
      setSaveViewOpen(false);
      toast.error("Couldn't save view", { description: (err as Error).message });
    },
  });

  const deleteViewMutation = useMutation({
    mutationFn: (id: string) => api.deleteView(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["views", ktype] });
      setSelectedViewId(NEW_VIEW_ID);
      setDeleteViewOpen(false);
      toast.success("View deleted");
    },
    onError: (err) => {
      setDeleteViewOpen(false);
      toast.error("Couldn't delete view", {
        description: (err as Error).message,
      });
    },
  });

  const [selected, setSelected] = useState<KRecord | null>(null);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  // React Router reuses this component across /records/:ktype
  // transitions (same route pattern), so useState does not reset on
  // the navigation. Clear the bulk-action selection and right-pane
  // focus explicitly whenever the KType changes — otherwise the
  // toolbar keeps showing "N selected" with stale IDs from the
  // previous KType, and clicking a bulk action would send them to
  // the new KType's /bulk endpoint (backend safely rejects, UX
  // looks broken).
  useEffect(() => {
    setSelectedIds(new Set());
    setSelected(null);
  }, [ktype]);
  const toggleSelect = (id: string, checked: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  };
  const toggleSelectAll = (checked: boolean, rows: KRecord[]) => {
    setSelectedIds(checked ? new Set(rows.map((r) => r.id)) : new Set());
  };

  const bulkStatusMutation = useMutation({
    mutationFn: async ({ ids, status }: { ids: string[]; status: string }) =>
      api.bulkRecords(ktype!, {
        ids,
        action: "status_change",
        payload: { status },
      }),
    onSuccess: (_data, { ids }) => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
      setSelectedIds(new Set());
      setStatusPromptOpen(false);
      toast.success(`Updated ${ids.length} record(s)`);
    },
    onError: (err) => {
      setStatusPromptOpen(false);
      toast.error("Bulk update failed", {
        description: (err as Error).message,
      });
    },
  });

  const bulkDeleteMutation = useMutation({
    mutationFn: async (ids: string[]) =>
      api.bulkRecords(ktype!, { ids, action: "delete" }),
    onSuccess: (_data, ids) => {
      qc.invalidateQueries({ queryKey: ["records", ktype] });
      setSelectedIds(new Set());
      setBulkDeleteOpen(false);
      toast.success(`Deleted ${ids.length} record(s)`);
    },
    onError: (err) => {
      setBulkDeleteOpen(false);
      toast.error("Bulk delete failed", {
        description: (err as Error).message,
      });
    },
  });

  // Keep the dialog open while the mutation runs so its `loading`
  // pending state is visible; onSuccess/onError closes it.
  const submitBulkStatus = (status: string) => {
    bulkStatusMutation.mutate({ ids: [...selectedIds], status });
  };

  const confirmBulkDelete = () => {
    bulkDeleteMutation.mutate([...selectedIds]);
  };

  const handleBulkExport = async () => {
    // The two mutations above route errors through useMutation's
    // builtin handling, but bulkExportRecords returns a plain
    // Promise<string> because the response is a blob — without a
    // try/catch a 4xx/5xx surfaces as an unhandled rejection and
    // the user gets no feedback at all.
    try {
      const csv = await api.bulkExportRecords(ktype!, [...selectedIds]);
      const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${ktype}.csv`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(url);
      toast.success("Export complete", { description: `${ktype}.csv` });
    } catch (err) {
      toast.error("Export failed", {
        description: (err as Error).message,
      });
    }
  };

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

  const submitSaveView = (name: string) => {
    // Without an in-page filter editor we seed new views with an
    // empty predicate; toggling columns/sort from list headers
    // later is a PATCH. The server treats {} as "match everything"
    // so saving "all records" is the zero-effort default.
    createViewMutation.mutate({ name, filters: {}, sort: "" });
  };

  const confirmDeleteView = () => {
    if (!activeView) return;
    deleteViewMutation.mutate(activeView.id);
  };

  if (!ktype) return null;
  if (ktypeQuery.isLoading || recordsQuery.isLoading) {
    return (
      <div className="flex flex-col gap-4">
        <div className="flex items-center justify-between gap-3">
          <Skeleton className="h-8 w-48" />
          <Skeleton className="h-8 w-32" />
        </div>
        <div className="flex flex-col gap-2">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-11 w-full" />
          ))}
        </div>
      </div>
    );
  }
  if (ktypeQuery.error) {
    return (
      <EmptyState
        icon={<AlertTriangle />}
        title="Couldn't load this list"
        description={(ktypeQuery.error as Error).message}
        action={
          <Button variant="secondary" onClick={() => ktypeQuery.refetch()}>
            Retry
          </Button>
        }
      />
    );
  }
  if (!ktypeQuery.data) {
    return (
      <EmptyState
        icon={<AlertTriangle />}
        title="KType not found"
        description="This record type doesn't exist or you don't have access to it."
      />
    );
  }

  const kt = ktypeQuery.data;
  const views = viewsQuery.data ?? [];

  const hasRecords = records.length > 0;

  return (
    <div className="flex items-start gap-4">
      <section className="min-w-0 flex-1">
        <header className="flex flex-wrap items-center justify-between gap-3">
          <h1 className="text-2xl font-semibold tracking-tight text-fg">
            {kt.name}
          </h1>
          <div className="flex flex-wrap items-center gap-2">
            <label className="flex items-center gap-2 text-sm text-fg-muted">
              View:
              <Select
                size="sm"
                aria-label="Saved view"
                value={activeView?.id ?? NEW_VIEW_ID}
                onChange={(e) => setSelectedViewId(e.target.value)}
                className="w-auto"
              >
                <option value={NEW_VIEW_ID}>All records</option>
                {views.map((v) => (
                  <option key={v.id} value={v.id}>
                    {v.name}
                    {v.is_default ? " (default)" : ""}
                    {v.shared ? " — shared" : ""}
                  </option>
                ))}
              </Select>
            </label>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setSaveViewOpen(true)}
              disabled={createViewMutation.isPending}
            >
              Save view
            </Button>
            {activeView && (
              <Button
                size="sm"
                variant="ghost"
                onClick={() => setDeleteViewOpen(true)}
                disabled={deleteViewMutation.isPending}
              >
                Delete view
              </Button>
            )}
            {hasKanban && (
              <div role="tablist" className="flex gap-1">
                <Button
                  size="sm"
                  variant={mode === "list" ? "primary" : "outline"}
                  onClick={() => setModeOverride("list")}
                  aria-pressed={mode === "list"}
                >
                  List
                </Button>
                <Button
                  size="sm"
                  variant={mode === "kanban" ? "primary" : "outline"}
                  onClick={() => setModeOverride("kanban")}
                  aria-pressed={mode === "kanban"}
                >
                  Kanban
                </Button>
              </div>
            )}
            <Button
              size="sm"
              leadingIcon={<Plus className="h-4 w-4" />}
              onClick={() => navigate(`/records/${ktype}/new`)}
            >
              New
            </Button>
          </div>
        </header>
        <div className="mt-4">
          {!hasRecords ? (
            <EmptyState
              icon={<Inbox />}
              title={
                activeView
                  ? `No matching ${kt.name} records`
                  : `No ${kt.name} records yet`
              }
              description={
                activeView
                  ? "No records match this view's filters."
                  : "Create your first one to get started."
              }
              action={
                <Button
                  leadingIcon={<Plus className="h-4 w-4" />}
                  onClick={() => navigate(`/records/${ktype}/new`)}
                >
                  New {kt.name}
                </Button>
              }
            />
          ) : mode === "kanban" && hasKanban ? (
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
              selectedIds={selectedIds}
              onToggleSelect={toggleSelect}
              onToggleAll={(checked) => toggleSelectAll(checked, records)}
            />
          )}
        </div>
        {selectedIds.size > 0 && (
          <div
            role="toolbar"
            aria-label="Bulk actions"
            className="sticky bottom-4 z-10 mt-3 flex items-center gap-2 rounded-lg border border-border bg-bg-elevated px-3 py-2 shadow-lg"
          >
            <span className="text-sm font-medium text-fg">
              {selectedIds.size} selected
            </span>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setStatusPromptOpen(true)}
              disabled={bulkStatusMutation.isPending}
            >
              Change Status
            </Button>
            <Button
              size="sm"
              variant="destructive"
              onClick={() => setBulkDeleteOpen(true)}
              disabled={bulkDeleteMutation.isPending}
            >
              Delete
            </Button>
            <Button size="sm" variant="secondary" onClick={handleBulkExport}>
              Export CSV
            </Button>
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setSelectedIds(new Set())}
            >
              Clear
            </Button>
          </div>
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

      <PromptDialog
        open={statusPromptOpen}
        onOpenChange={(o) => !bulkStatusMutation.isPending && setStatusPromptOpen(o)}
        title="Change status"
        description={`Apply a new status to ${selectedIds.size} record(s).`}
        label="New status"
        placeholder="e.g. won, lost, on_hold"
        confirmLabel="Apply"
        loading={bulkStatusMutation.isPending}
        onSubmit={submitBulkStatus}
      />
      <ConfirmDialog
        open={bulkDeleteOpen}
        onOpenChange={(o) => !bulkDeleteMutation.isPending && setBulkDeleteOpen(o)}
        destructive
        title={`Delete ${selectedIds.size} record(s)?`}
        description="This permanently removes the selected records. This action cannot be undone."
        confirmLabel="Delete"
        loading={bulkDeleteMutation.isPending}
        onConfirm={confirmBulkDelete}
      />
      <PromptDialog
        open={saveViewOpen}
        onOpenChange={(o) => !createViewMutation.isPending && setSaveViewOpen(o)}
        title="Save view"
        description="Save the current filters and sort as a reusable view."
        label="View name"
        placeholder="e.g. Open deals"
        confirmLabel="Save"
        loading={createViewMutation.isPending}
        onSubmit={submitSaveView}
      />
      <ConfirmDialog
        open={deleteViewOpen}
        onOpenChange={(o) => !deleteViewMutation.isPending && setDeleteViewOpen(o)}
        destructive
        title={`Delete view "${activeView?.name ?? ""}"?`}
        description="This removes the saved view for you. Records are not affected."
        confirmLabel="Delete view"
        loading={deleteViewMutation.isPending}
        onConfirm={confirmDeleteView}
      />
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
