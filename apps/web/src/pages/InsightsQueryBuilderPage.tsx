// Phase L Insights — visual query builder.
//
// Composes a saved insights query (extends reporting.Definition with
// calculated columns) without dropping into JSON editing. Source picker
// covers both KType-backed sources (`ktype:<name>`) and the canonical
// ledger / inventory tables. Filters / aggregations / calculated
// columns are added through structured controls, and the live preview
// hits POST /api/v1/insights/queries/{id}/run after a Save.

import { useEffect, useMemo, useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type {
  CalculatedColumn,
  InsightsQuery,
  InsightsQueryDefinition,
  InsightsRunResult,
  InsightsVizType,
  KType,
  ReportAggregation,
  ReportFilter,
  ReportSort,
} from "@kapp/client";
import { api } from "../lib/api";
import { Viz } from "../components/insights/Charts";
import { ShareModal } from "../components/insights/ShareModal";

// Curated list of non-KType ledger / inventory / helpdesk tables the
// reporting runner will accept as a `source`. Mirrors
// internal/reporting.AllowedTables — kept short on purpose; the more
// exotic surfaces are reachable via the JSON-editor escape hatch.
const LEDGER_SOURCES = [
  "journal_entries",
  "journal_lines",
  "ar_invoices",
  "ap_bills",
  "payments",
  "accounts",
  "fiscal_periods",
  "inventory_moves",
  "stock_levels",
  "ticket_sla_log",
];

const VIZ_OPTIONS: InsightsVizType[] = [
  "table",
  "bar",
  "line",
  "pie",
  "donut",
  "funnel",
  "number_card",
  "pivot",
];

const FILTER_OPS = ["=", "!=", ">", ">=", "<", "<=", "in", "not_in", "ilike"];

const AGG_OPS: ReportAggregation["op"][] = [
  "count",
  "sum",
  "avg",
  "min",
  "max",
];

interface QueryFormState {
  name: string;
  description: string;
  source: string;
  columns: string[];
  filters: ReportFilter[];
  group_by: string[];
  aggregations: ReportAggregation[];
  sort: ReportSort[];
  limit: number;
  calculated_columns: CalculatedColumn[];
  cache_ttl_seconds: number;
}

const blankForm = (): QueryFormState => ({
  name: "",
  description: "",
  source: "ktype:crm.deal",
  columns: ["id", "name"],
  filters: [],
  group_by: [],
  aggregations: [],
  sort: [],
  limit: 100,
  calculated_columns: [],
  cache_ttl_seconds: 300,
});

function fromQuery(q: InsightsQuery): QueryFormState {
  const def = q.definition;
  return {
    name: q.name,
    description: q.description ?? "",
    source: def.source,
    columns: def.columns ?? [],
    filters: def.filters ?? [],
    group_by: def.group_by ?? [],
    aggregations: def.aggregations ?? [],
    sort: def.sort ?? [],
    limit: def.limit ?? 100,
    calculated_columns: def.calculated_columns ?? [],
    cache_ttl_seconds: q.cache_ttl_seconds ?? 300,
  };
}

function buildDefinition(state: QueryFormState): InsightsQueryDefinition {
  return {
    source: state.source,
    columns: state.columns,
    filters: state.filters,
    group_by: state.group_by.length > 0 ? state.group_by : undefined,
    aggregations:
      state.aggregations.length > 0 ? state.aggregations : undefined,
    sort: state.sort.length > 0 ? state.sort : undefined,
    limit: state.limit > 0 ? state.limit : undefined,
    calculated_columns:
      state.calculated_columns.length > 0
        ? state.calculated_columns
        : undefined,
  };
}

export function InsightsQueryBuilderPage() {
  const qc = useQueryClient();
  const queriesQuery = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights-queries"],
    queryFn: () => api.listInsightsQueries(),
  });
  const ktypesQuery = useQuery<KType[]>({
    queryKey: ["ktypes"],
    queryFn: () => api.listKTypes(),
    staleTime: 60_000,
  });

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [form, setForm] = useState<QueryFormState>(blankForm());
  const [previewVizType, setPreviewVizType] =
    useState<InsightsVizType>("table");
  const [error, setError] = useState<string | null>(null);
  const [preview, setPreview] = useState<InsightsRunResult | null>(null);
  const [shareOpen, setShareOpen] = useState(false);

  // When a saved query is picked from the sidebar, hydrate the form
  // from it. Re-runs preview for live cache-aware result.
  useEffect(() => {
    if (!selectedId) return;
    const q = queriesQuery.data?.queries.find((q) => q.id === selectedId);
    if (q) setForm(fromQuery(q));
  }, [selectedId, queriesQuery.data]);

  const sourceOptions = useMemo(() => {
    const ktypeOptions = (ktypesQuery.data ?? []).map((k) => `ktype:${k.name}`);
    return [...ktypeOptions, ...LEDGER_SOURCES];
  }, [ktypesQuery.data]);

  const createMut = useMutation({
    mutationFn: () =>
      api.createInsightsQuery({
        name: form.name,
        description: form.description,
        definition: buildDefinition(form),
        cache_ttl_seconds: form.cache_ttl_seconds,
      }),
    onSuccess: (saved) => {
      setSelectedId(saved.id);
      setError(null);
      qc.invalidateQueries({ queryKey: ["insights-queries"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const updateMut = useMutation({
    mutationFn: (id: string) =>
      api.updateInsightsQuery(id, {
        name: form.name,
        description: form.description,
        definition: buildDefinition(form),
        cache_ttl_seconds: form.cache_ttl_seconds,
      }),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["insights-queries"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const runMut = useMutation({
    mutationFn: (id: string) => api.runInsightsQuery(id, { bypass_cache: false }),
    onSuccess: (res) => {
      setPreview(res);
      setError(null);
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.deleteInsightsQuery(id),
    onSuccess: () => {
      setSelectedId(null);
      setForm(blankForm());
      setPreview(null);
      qc.invalidateQueries({ queryKey: ["insights-queries"] });
    },
    onError: (err: Error) => setError(err.message),
  });

  const onSave = () => {
    if (!form.name.trim()) {
      setError("query name required");
      return;
    }
    if (selectedId) updateMut.mutate(selectedId);
    else createMut.mutate();
  };

  const onRun = () => {
    if (!selectedId) {
      setError("save the query before running it");
      return;
    }
    runMut.mutate(selectedId);
  };

  const moveColumn = (from: number, to: number) => {
    if (to < 0 || to >= form.columns.length) return;
    const next = [...form.columns];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved);
    setForm({ ...form, columns: next });
  };

  return (
    <section>
      <h1>Insights — Query Builder</h1>
      <p style={{ color: "#6b7280" }}>
        Compose a saved query over a KType or ledger table. Filters,
        group-by, aggregations and calculated columns are validated
        server-side before SQL is emitted.
      </p>

      <div style={{ display: "flex", gap: 16 }}>
        <aside style={{ flex: "0 0 220px" }}>
          <div
            style={{
              display: "flex",
              justifyContent: "space-between",
              alignItems: "center",
            }}
          >
            <h3 style={{ fontSize: 14 }}>Saved queries</h3>
            <button
              onClick={() => {
                setSelectedId(null);
                setForm(blankForm());
                setPreview(null);
              }}
              style={{ fontSize: 12 }}
            >
              + New
            </button>
          </div>
          {queriesQuery.isLoading && <p>Loading…</p>}
          <ul style={{ listStyle: "none", padding: 0, margin: 0, fontSize: 13 }}>
            {(queriesQuery.data?.queries ?? []).map((q) => (
              <li key={q.id} style={{ padding: "4px 0" }}>
                <button
                  onClick={() => setSelectedId(q.id)}
                  style={{
                    background: "none",
                    border: "none",
                    color:
                      selectedId === q.id ? "#111" : "#2563eb",
                    fontWeight: selectedId === q.id ? 600 : 400,
                    cursor: "pointer",
                    padding: 0,
                  }}
                >
                  {q.name}
                </button>
              </li>
            ))}
          </ul>
        </aside>

        <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: 12 }}>
          <div style={{ display: "flex", gap: 8 }}>
            <input
              placeholder="query name"
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
              style={{ flex: 1 }}
            />
            <input
              placeholder="description (optional)"
              value={form.description}
              onChange={(e) =>
                setForm({ ...form, description: e.target.value })
              }
              style={{ flex: 2 }}
            />
            <button onClick={onSave} disabled={createMut.isPending || updateMut.isPending}>
              {selectedId ? "Update" : "Save"}
            </button>
            {selectedId && (
              <>
                <button onClick={onRun} disabled={runMut.isPending}>
                  {runMut.isPending ? "Running…" : "Run"}
                </button>
                <button onClick={() => setShareOpen(true)}>Share…</button>
                <button
                  onClick={() => {
                    if (confirm("Delete this query?")) deleteMut.mutate(selectedId);
                  }}
                  style={{ color: "#dc2626" }}
                >
                  Delete
                </button>
              </>
            )}
          </div>

          <Section title="Source">
            <select
              value={form.source}
              onChange={(e) => setForm({ ...form, source: e.target.value })}
              style={{ width: "100%" }}
            >
              {sourceOptions.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </Section>

          <Section title="Columns (drag to reorder)">
            <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
              {form.columns.map((c, i) => (
                <li
                  key={`${c}-${i}`}
                  draggable
                  onDragStart={(e) =>
                    e.dataTransfer.setData("text/plain", String(i))
                  }
                  onDragOver={(e) => e.preventDefault()}
                  onDrop={(e) => {
                    e.preventDefault();
                    const from = Number(e.dataTransfer.getData("text/plain"));
                    moveColumn(from, i);
                  }}
                  style={{
                    display: "flex",
                    alignItems: "center",
                    gap: 8,
                    padding: 4,
                    border: "1px solid #e5e7eb",
                    marginBottom: 4,
                    background: "#f9fafb",
                    cursor: "grab",
                  }}
                >
                  <span style={{ color: "#9ca3af" }}>⋮⋮</span>
                  <input
                    value={c}
                    onChange={(e) => {
                      const next = [...form.columns];
                      next[i] = e.target.value;
                      setForm({ ...form, columns: next });
                    }}
                    style={{ flex: 1 }}
                  />
                  <button onClick={() => moveColumn(i, i - 1)}>↑</button>
                  <button onClick={() => moveColumn(i, i + 1)}>↓</button>
                  <button
                    onClick={() =>
                      setForm({
                        ...form,
                        columns: form.columns.filter((_, j) => j !== i),
                      })
                    }
                    style={{ color: "#dc2626" }}
                  >
                    ✕
                  </button>
                </li>
              ))}
            </ul>
            <button
              onClick={() =>
                setForm({ ...form, columns: [...form.columns, "new_column"] })
              }
            >
              + Add column
            </button>
          </Section>

          <Section title="Filters">
            <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
              {form.filters.map((f, i) => (
                <li
                  key={i}
                  style={{ display: "flex", gap: 6, marginBottom: 4 }}
                >
                  <input
                    placeholder="column"
                    value={f.column}
                    onChange={(e) => {
                      const next = [...form.filters];
                      next[i] = { ...f, column: e.target.value };
                      setForm({ ...form, filters: next });
                    }}
                    style={{ flex: 2 }}
                  />
                  <select
                    value={f.op}
                    onChange={(e) => {
                      const next = [...form.filters];
                      next[i] = { ...f, op: e.target.value };
                      setForm({ ...form, filters: next });
                    }}
                  >
                    {FILTER_OPS.map((op) => (
                      <option key={op} value={op}>
                        {op}
                      </option>
                    ))}
                  </select>
                  <input
                    placeholder="value"
                    value={
                      f.value === undefined || f.value === null
                        ? ""
                        : String(f.value)
                    }
                    onChange={(e) => {
                      const next = [...form.filters];
                      next[i] = { ...f, value: e.target.value };
                      setForm({ ...form, filters: next });
                    }}
                    style={{ flex: 2 }}
                  />
                  <button
                    onClick={() =>
                      setForm({
                        ...form,
                        filters: form.filters.filter((_, j) => j !== i),
                      })
                    }
                    style={{ color: "#dc2626" }}
                  >
                    ✕
                  </button>
                </li>
              ))}
            </ul>
            <button
              onClick={() =>
                setForm({
                  ...form,
                  filters: [
                    ...form.filters,
                    { column: "", op: "=", value: "" },
                  ],
                })
              }
            >
              + Add filter
            </button>
          </Section>

          <Section title="Group by">
            <input
              placeholder="comma-separated columns"
              value={form.group_by.join(", ")}
              onChange={(e) =>
                setForm({
                  ...form,
                  group_by: e.target.value
                    .split(",")
                    .map((s) => s.trim())
                    .filter(Boolean),
                })
              }
              style={{ width: "100%" }}
            />
          </Section>

          <Section title="Aggregations">
            <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
              {form.aggregations.map((agg, i) => (
                <li
                  key={i}
                  style={{ display: "flex", gap: 6, marginBottom: 4 }}
                >
                  <select
                    value={agg.op}
                    onChange={(e) => {
                      const next = [...form.aggregations];
                      next[i] = {
                        ...agg,
                        op: e.target.value as ReportAggregation["op"],
                      };
                      setForm({ ...form, aggregations: next });
                    }}
                  >
                    {AGG_OPS.map((op) => (
                      <option key={op} value={op}>
                        {op}
                      </option>
                    ))}
                  </select>
                  <input
                    placeholder="column"
                    value={agg.column ?? ""}
                    onChange={(e) => {
                      const next = [...form.aggregations];
                      next[i] = { ...agg, column: e.target.value };
                      setForm({ ...form, aggregations: next });
                    }}
                    style={{ flex: 1 }}
                  />
                  <input
                    placeholder="alias"
                    value={agg.alias ?? ""}
                    onChange={(e) => {
                      const next = [...form.aggregations];
                      next[i] = { ...agg, alias: e.target.value };
                      setForm({ ...form, aggregations: next });
                    }}
                    style={{ flex: 1 }}
                  />
                  <button
                    onClick={() =>
                      setForm({
                        ...form,
                        aggregations: form.aggregations.filter(
                          (_, j) => j !== i
                        ),
                      })
                    }
                    style={{ color: "#dc2626" }}
                  >
                    ✕
                  </button>
                </li>
              ))}
            </ul>
            <button
              onClick={() =>
                setForm({
                  ...form,
                  aggregations: [
                    ...form.aggregations,
                    { op: "count", alias: "count" },
                  ],
                })
              }
            >
              + Add aggregation
            </button>
          </Section>

          <Section title="Calculated columns">
            <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
              {form.calculated_columns.map((c, i) => (
                <li
                  key={i}
                  style={{ display: "flex", gap: 6, marginBottom: 4 }}
                >
                  <input
                    placeholder="name"
                    value={c.name}
                    onChange={(e) => {
                      const next = [...form.calculated_columns];
                      next[i] = { ...c, name: e.target.value };
                      setForm({ ...form, calculated_columns: next });
                    }}
                    style={{ flex: 1 }}
                  />
                  <input
                    placeholder="expression e.g. price * qty"
                    value={c.expression}
                    onChange={(e) => {
                      const next = [...form.calculated_columns];
                      next[i] = { ...c, expression: e.target.value };
                      setForm({ ...form, calculated_columns: next });
                    }}
                    style={{ flex: 3 }}
                  />
                  <button
                    onClick={() =>
                      setForm({
                        ...form,
                        calculated_columns: form.calculated_columns.filter(
                          (_, j) => j !== i
                        ),
                      })
                    }
                    style={{ color: "#dc2626" }}
                  >
                    ✕
                  </button>
                </li>
              ))}
            </ul>
            <button
              onClick={() =>
                setForm({
                  ...form,
                  calculated_columns: [
                    ...form.calculated_columns,
                    { name: "", expression: "" },
                  ],
                })
              }
            >
              + Add calculated column
            </button>
          </Section>

          <Section title="Sort + limit + cache">
            <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
              <label>Sort:</label>
              <input
                placeholder="column"
                value={form.sort[0]?.column ?? ""}
                onChange={(e) => {
                  const next: ReportSort[] = e.target.value
                    ? [{ column: e.target.value, direction: form.sort[0]?.direction ?? "asc" }]
                    : [];
                  setForm({ ...form, sort: next });
                }}
              />
              <select
                value={form.sort[0]?.direction ?? "asc"}
                onChange={(e) => {
                  if (form.sort.length === 0) return;
                  const next = [...form.sort];
                  next[0] = {
                    ...next[0],
                    direction: e.target.value as "asc" | "desc",
                  };
                  setForm({ ...form, sort: next });
                }}
              >
                <option value="asc">asc</option>
                <option value="desc">desc</option>
              </select>
              <label style={{ marginLeft: 12 }}>Limit:</label>
              <input
                type="number"
                value={form.limit}
                onChange={(e) =>
                  setForm({ ...form, limit: Number(e.target.value) })
                }
                style={{ width: 80 }}
              />
              <label style={{ marginLeft: 12 }}>Cache TTL (s):</label>
              <input
                type="number"
                value={form.cache_ttl_seconds}
                onChange={(e) =>
                  setForm({
                    ...form,
                    cache_ttl_seconds: Number(e.target.value),
                  })
                }
                style={{ width: 80 }}
              />
            </div>
          </Section>

          {error && (
            <div style={{ color: "#dc2626", fontSize: 13 }}>{error}</div>
          )}

          {preview && (
            <Section title={`Preview (${preview.cache_hit ? "cache" : "live"})`}>
              <div style={{ display: "flex", gap: 8, marginBottom: 8 }}>
                <label>Visualisation:</label>
                <select
                  value={previewVizType}
                  onChange={(e) =>
                    setPreviewVizType(e.target.value as InsightsVizType)
                  }
                >
                  {VIZ_OPTIONS.map((v) => (
                    <option key={v} value={v}>
                      {v}
                    </option>
                  ))}
                </select>
                <span style={{ color: "#6b7280", marginLeft: "auto" }}>
                  {preview.result.rows.length} rows
                </span>
              </div>
              <Viz vizType={previewVizType} result={preview.result} />
            </Section>
          )}
        </div>
      </div>

      {shareOpen && selectedId && (
        <ShareModal
          resource="query"
          resourceId={selectedId}
          resourceName={form.name}
          onClose={() => setShareOpen(false)}
        />
      )}
    </section>
  );
}

function Section({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <fieldset
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 6,
        padding: 12,
      }}
    >
      <legend style={{ fontSize: 13, fontWeight: 600, padding: "0 6px" }}>
        {title}
      </legend>
      {children}
    </fieldset>
  );
}
