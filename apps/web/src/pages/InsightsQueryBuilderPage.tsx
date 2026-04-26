import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import type {
  InsightsQuery,
  InsightsQueryDefinition,
  InsightsRunResult,
  InsightsShare,
  InsightsVizType,
  KType,
  ReportAggregation,
  ReportFilter,
} from "@kapp/client";
import { api } from "../lib/api";
import { InsightsWidgetChart } from "../components/InsightsWidgetChart";

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

const AGG_OPS: ReportAggregation["op"][] = ["count", "sum", "avg", "min", "max"];

// Non-KType sources. The reporting runner accepts both ktype:<name>
// and these ledger / stock tables as a source prefix, so the builder
// exposes them alongside the dynamic KType list instead of hard-coding
// a single picker dimension.
const LEDGER_SOURCES = [
  { value: "ledger:journal_lines", label: "Journal Lines (ledger)" },
  { value: "ledger:ar_subledger", label: "AR Subledger" },
  { value: "ledger:ap_subledger", label: "AP Subledger" },
  { value: "ledger:stock_levels", label: "Stock Levels" },
  { value: "ledger:inventory_moves", label: "Inventory Moves" },
];

const BLANK_DEFINITION: InsightsQueryDefinition = {
  source: "ktype:crm.deal",
  columns: [],
  filters: [],
  group_by: [],
  aggregations: [],
  sort: [],
  limit: 100,
};

/**
 * InsightsQueryBuilderPage is the visual query-definition editor.
 * Users pick a data source, the columns to project, filters / group-by
 * / aggregations, and a live preview hits /insights/queries/{id}/run
 * so the same code path the dashboard uses renders on the builder
 * too. Save creates or updates a saved query; the backend enforces
 * statement_timeout + caching so hitting Run repeatedly is cheap.
 */
export function InsightsQueryBuilderPage() {
  const { id } = useParams<{ id: string }>();
  const isNew = !id || id === "new";
  const qc = useQueryClient();
  const nav = useNavigate();

  const ktypes = useQuery<KType[]>({
    queryKey: ["ktypes"],
    queryFn: () => api.listKTypes(),
  });

  const existing = useQuery<InsightsQuery>({
    queryKey: ["insights", "query", id],
    queryFn: () => api.getInsightsQuery(id!),
    enabled: !isNew,
  });

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [definition, setDefinition] = useState<InsightsQueryDefinition>(
    BLANK_DEFINITION
  );
  const [cacheTTL, setCacheTTL] = useState<string>(""); // "" == default
  const [preview, setPreview] = useState<InsightsRunResult | null>(null);
  const [previewViz, setPreviewViz] = useState<InsightsVizType>("table");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (existing.data) {
      setName(existing.data.name);
      setDescription(existing.data.description ?? "");
      setDefinition(existing.data.definition);
      setCacheTTL(
        existing.data.cache_ttl_seconds == null
          ? ""
          : String(existing.data.cache_ttl_seconds)
      );
    }
  }, [existing.data]);

  // Combine dynamic KType list + static ledger sources into a
  // single flat dropdown so the source picker reflects everything
  // the backend accepts, keyed by the prefix the runner parses.
  const sources = useMemo(() => {
    const kt = (ktypes.data ?? []).map((k) => ({
      value: `ktype:${k.name}`,
      label: `${k.name} (KType)`,
    }));
    return [...kt, ...LEDGER_SOURCES];
  }, [ktypes.data]);

  // Column suggestions for the currently-selected KType. Non-KType
  // sources don't expose a schema over /ktypes so we drop back to a
  // plain text input in those cases.
  const ktypeSchema = useMemo(() => {
    if (!definition.source.startsWith("ktype:")) return null;
    const name = definition.source.slice("ktype:".length);
    return (ktypes.data ?? []).find((k) => k.name === name) ?? null;
  }, [ktypes.data, definition.source]);
  const fieldNames = useMemo(
    () => (ktypeSchema?.schema?.fields ?? []).map((f) => f.name),
    [ktypeSchema]
  );

  const buildBody = () => {
    const ttl =
      cacheTTL.trim() === ""
        ? null
        : Math.max(0, Math.floor(Number(cacheTTL)));
    return {
      name,
      description,
      definition,
      cache_ttl_seconds: ttl,
    };
  };

  const create = useMutation({
    mutationFn: () => api.createInsightsQuery(buildBody()),
    onSuccess: (saved) => {
      qc.invalidateQueries({ queryKey: ["insights", "queries"] });
      setError(null);
      nav(`/insights/queries/${saved.id}`, { replace: true });
    },
    onError: (e: Error) => setError(e.message),
  });

  const update = useMutation({
    mutationFn: () => api.updateInsightsQuery(id!, buildBody()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights", "queries"] });
      qc.invalidateQueries({ queryKey: ["insights", "query", id] });
      setError(null);
    },
    onError: (e: Error) => setError(e.message),
  });

  // Live preview uses the saved-query run endpoint when we have an
  // id; for the blank-slate "new" case we can't run until the user
  // saves, since the backend only exposes /run on saved rows.
  const run = useMutation({
    mutationFn: () => {
      if (isNew) {
        throw new Error("Save the query first to run a preview.");
      }
      return api.runInsightsQuery(id!, { bypass_cache: true });
    },
    onSuccess: (res) => {
      setPreview(res);
      setError(null);
    },
    onError: (e: Error) => setError(e.message),
  });

  const onSave = () => {
    if (isNew) create.mutate();
    else update.mutate();
  };

  return (
    <section>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "baseline",
          marginBottom: 4,
        }}
      >
        <h1 style={{ margin: 0 }}>{isNew ? "New query" : "Edit query"}</h1>
        <button onClick={() => nav("/insights/queries")}>← All queries</button>
      </div>
      {error && (
        <div
          style={{
            background: "#fef2f2",
            color: "#b91c1c",
            padding: "6px 10px",
            borderRadius: 4,
            margin: "8px 0",
            fontSize: 13,
          }}
        >
          {error}
        </div>
      )}

      <div style={{ display: "flex", gap: 16, alignItems: "flex-start" }}>
        <div style={{ flex: "1 1 420px", minWidth: 360 }}>
          <section style={card}>
            <h3 style={h3}>Identity</h3>
            <label style={label}>
              Name
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                style={input}
              />
            </label>
            <label style={label}>
              Description
              <input
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                style={input}
              />
            </label>
            <label style={label}>
              Cache TTL (seconds — empty = 300 default, 0 = disabled)
              <input
                type="number"
                min={0}
                value={cacheTTL}
                onChange={(e) => setCacheTTL(e.target.value)}
                style={input}
                placeholder="300"
              />
            </label>
          </section>

          <section style={card}>
            <h3 style={h3}>Source</h3>
            <select
              value={definition.source}
              onChange={(e) =>
                setDefinition({ ...definition, source: e.target.value })
              }
              style={input}
            >
              {sources.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          </section>

          <section style={card}>
            <h3 style={h3}>Columns</h3>
            <ColumnEditor
              value={definition.columns}
              onChange={(columns) => setDefinition({ ...definition, columns })}
              suggestions={fieldNames}
            />
          </section>

          <section style={card}>
            <h3 style={h3}>Filters</h3>
            <FilterEditor
              value={definition.filters ?? []}
              onChange={(filters) => setDefinition({ ...definition, filters })}
              suggestions={fieldNames}
            />
          </section>

          <section style={card}>
            <h3 style={h3}>Group &amp; aggregate</h3>
            <div style={{ fontSize: 12, color: "#6b7280", marginBottom: 4 }}>
              Group-by columns
            </div>
            <ColumnEditor
              value={definition.group_by ?? []}
              onChange={(group_by) => setDefinition({ ...definition, group_by })}
              suggestions={fieldNames}
            />
            <div
              style={{
                fontSize: 12,
                color: "#6b7280",
                marginBottom: 4,
                marginTop: 8,
              }}
            >
              Aggregations
            </div>
            <AggregationEditor
              value={definition.aggregations ?? []}
              onChange={(aggregations) =>
                setDefinition({ ...definition, aggregations })
              }
              suggestions={fieldNames}
            />
          </section>

          <section style={card}>
            <h3 style={h3}>Sort &amp; limit</h3>
            <SortEditor
              value={definition.sort ?? []}
              onChange={(sort) => setDefinition({ ...definition, sort })}
              suggestions={fieldNames}
            />
            <label style={label}>
              Row limit (server hard-caps at 10,000)
              <input
                type="number"
                min={1}
                max={10000}
                value={definition.limit ?? 100}
                onChange={(e) =>
                  setDefinition({
                    ...definition,
                    limit: Math.max(1, Math.min(10000, Number(e.target.value))),
                  })
                }
                style={input}
              />
            </label>
          </section>

          <div style={{ display: "flex", gap: 8 }}>
            <button onClick={onSave} disabled={create.isPending || update.isPending}>
              {isNew ? "Save" : "Update"}
            </button>
            <button
              onClick={() => run.mutate()}
              disabled={isNew || run.isPending}
              title={isNew ? "Save first, then run" : "Run now, bypass cache"}
            >
              {run.isPending ? "Running…" : "Run preview"}
            </button>
          </div>
        </div>

        <aside style={{ flex: "1 1 420px", minWidth: 360 }}>
          <section style={card}>
            <div
              style={{
                display: "flex",
                justifyContent: "space-between",
                alignItems: "center",
                marginBottom: 8,
              }}
            >
              <h3 style={{ ...h3, margin: 0 }}>Preview</h3>
              <select
                value={previewViz}
                onChange={(e) => setPreviewViz(e.target.value as InsightsVizType)}
                style={{ fontSize: 12 }}
              >
                {VIZ_OPTIONS.map((v) => (
                  <option key={v} value={v}>
                    {v}
                  </option>
                ))}
              </select>
            </div>
            <InsightsWidgetChart
              viz={previewViz}
              result={preview}
              emptyText="Click Run preview after saving"
            />
            {preview && (
              <div style={{ fontSize: 11, color: "#6b7280", marginTop: 6 }}>
                {preview.cache_hit ? "cache hit" : "fresh"} · {preview.result?.rows?.length ?? 0} rows
              </div>
            )}
          </section>

          {!isNew && <SharesCard queryId={id!} />}
        </aside>
      </div>
    </section>
  );
}

// ---------- Column editor ----------

function ColumnEditor({
  value,
  onChange,
  suggestions,
}: {
  value: string[];
  onChange: (next: string[]) => void;
  suggestions: string[];
}) {
  const [draft, setDraft] = useState("");
  return (
    <div>
      <div style={{ display: "flex", flexWrap: "wrap", gap: 4, marginBottom: 6 }}>
        {value.map((c, i) => (
          <span key={`${c}-${i}`} style={chip}>
            {c}{" "}
            <button
              onClick={() => onChange(value.filter((_, j) => j !== i))}
              style={chipX}
              aria-label={`Remove ${c}`}
            >
              ×
            </button>
          </span>
        ))}
      </div>
      <div style={{ display: "flex", gap: 4 }}>
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && draft.trim()) {
              onChange([...value, draft.trim()]);
              setDraft("");
            }
          }}
          placeholder="field name + Enter"
          style={{ ...input, flex: 1 }}
          list="insights-column-suggestions"
        />
        <button
          onClick={() => {
            if (draft.trim()) {
              onChange([...value, draft.trim()]);
              setDraft("");
            }
          }}
        >
          Add
        </button>
      </div>
      <datalist id="insights-column-suggestions">
        {suggestions.map((s) => (
          <option key={s} value={s} />
        ))}
      </datalist>
    </div>
  );
}

// ---------- Filter editor ----------

const FILTER_OPS = ["=", "!=", "<", "<=", ">", ">=", "contains", "in", "between"];

function FilterEditor({
  value,
  onChange,
  suggestions,
}: {
  value: ReportFilter[];
  onChange: (next: ReportFilter[]) => void;
  suggestions: string[];
}) {
  const update = (i: number, patch: Partial<ReportFilter>) => {
    onChange(value.map((f, j) => (j === i ? { ...f, ...patch } : f)));
  };
  return (
    <div>
      {value.map((f, i) => (
        <div
          key={i}
          style={{ display: "flex", gap: 4, marginBottom: 4, fontSize: 13 }}
        >
          <input
            value={f.column}
            onChange={(e) => update(i, { column: e.target.value })}
            placeholder="column"
            style={{ ...input, flex: 2 }}
            list="insights-column-suggestions"
          />
          <select
            value={f.op}
            onChange={(e) => update(i, { op: e.target.value })}
            style={{ ...input, flex: 1 }}
          >
            {FILTER_OPS.map((o) => (
              <option key={o}>{o}</option>
            ))}
          </select>
          <input
            value={stringify(f.value)}
            onChange={(e) => update(i, { value: coerce(e.target.value) })}
            placeholder="value"
            style={{ ...input, flex: 2 }}
          />
          <button onClick={() => onChange(value.filter((_, j) => j !== i))}>×</button>
        </div>
      ))}
      <button
        onClick={() => onChange([...value, { column: "", op: "=", value: "" }])}
        style={{ fontSize: 12 }}
      >
        + Add filter
      </button>
      {suggestions.length === 0 && null}
    </div>
  );
}

// ---------- Aggregation editor ----------

function AggregationEditor({
  value,
  onChange,
  suggestions,
}: {
  value: ReportAggregation[];
  onChange: (next: ReportAggregation[]) => void;
  suggestions: string[];
}) {
  const update = (i: number, patch: Partial<ReportAggregation>) => {
    onChange(value.map((a, j) => (j === i ? { ...a, ...patch } : a)));
  };
  return (
    <div>
      {value.map((a, i) => (
        <div key={i} style={{ display: "flex", gap: 4, marginBottom: 4 }}>
          <select
            value={a.op}
            onChange={(e) =>
              update(i, { op: e.target.value as ReportAggregation["op"] })
            }
            style={{ ...input, flex: 1 }}
          >
            {AGG_OPS.map((o) => (
              <option key={o}>{o}</option>
            ))}
          </select>
          <input
            value={a.column ?? ""}
            onChange={(e) => update(i, { column: e.target.value })}
            placeholder={a.op === "count" ? "(optional column)" : "column"}
            style={{ ...input, flex: 2 }}
            list="insights-column-suggestions"
          />
          <input
            value={a.alias ?? ""}
            onChange={(e) => update(i, { alias: e.target.value })}
            placeholder="alias (optional)"
            style={{ ...input, flex: 1 }}
          />
          <button onClick={() => onChange(value.filter((_, j) => j !== i))}>×</button>
        </div>
      ))}
      <button
        onClick={() =>
          onChange([...value, { op: "count", column: "", alias: "" }])
        }
        style={{ fontSize: 12 }}
      >
        + Add aggregation
      </button>
      {suggestions.length === 0 && null}
    </div>
  );
}

// ---------- Sort editor ----------

function SortEditor({
  value,
  onChange,
  suggestions,
}: {
  value: { column: string; direction?: "asc" | "desc" }[];
  onChange: (next: { column: string; direction?: "asc" | "desc" }[]) => void;
  suggestions: string[];
}) {
  return (
    <div>
      {value.map((s, i) => (
        <div key={i} style={{ display: "flex", gap: 4, marginBottom: 4 }}>
          <input
            value={s.column}
            onChange={(e) =>
              onChange(
                value.map((x, j) => (j === i ? { ...x, column: e.target.value } : x))
              )
            }
            placeholder="column"
            style={{ ...input, flex: 2 }}
            list="insights-column-suggestions"
          />
          <select
            value={s.direction ?? "asc"}
            onChange={(e) =>
              onChange(
                value.map((x, j) =>
                  j === i
                    ? { ...x, direction: e.target.value as "asc" | "desc" }
                    : x
                )
              )
            }
            style={{ ...input, flex: 1 }}
          >
            <option value="asc">asc</option>
            <option value="desc">desc</option>
          </select>
          <button onClick={() => onChange(value.filter((_, j) => j !== i))}>×</button>
        </div>
      ))}
      <button
        onClick={() => onChange([...value, { column: "", direction: "asc" }])}
        style={{ fontSize: 12 }}
      >
        + Add sort
      </button>
      {suggestions.length === 0 && null}
    </div>
  );
}

// ---------- Shares card ----------

function SharesCard({ queryId }: { queryId: string }) {
  const qc = useQueryClient();
  const shares = useQuery<{ shares: InsightsShare[] }>({
    queryKey: ["insights", "query-shares", queryId],
    queryFn: () => api.listInsightsQueryShares(queryId),
  });
  const [granteeType, setGranteeType] = useState("user");
  const [grantee, setGrantee] = useState("");
  const [permission, setPermission] = useState("view");
  const create = useMutation({
    mutationFn: () =>
      api.createInsightsQueryShare(queryId, {
        grantee_type: granteeType,
        grantee,
        permission,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["insights", "query-shares", queryId] });
      setGrantee("");
    },
  });
  return (
    <section style={card}>
      <h3 style={h3}>Shares</h3>
      {(shares.data?.shares ?? []).length === 0 && (
        <div style={{ color: "#9ca3af", fontStyle: "italic", fontSize: 13 }}>
          Not shared.
        </div>
      )}
      <ul style={{ listStyle: "none", padding: 0, margin: "6px 0", fontSize: 13 }}>
        {(shares.data?.shares ?? []).map((s) => (
          <li key={s.id} style={{ padding: "2px 0" }}>
            <strong>{s.grantee_type}</strong>: {s.grantee} — {s.permission}
          </li>
        ))}
      </ul>
      <div style={{ display: "flex", gap: 4, marginTop: 6 }}>
        <select
          value={granteeType}
          onChange={(e) => setGranteeType(e.target.value)}
          style={{ ...input, flex: 1 }}
        >
          <option value="user">user</option>
          <option value="role">role</option>
        </select>
        <input
          value={grantee}
          onChange={(e) => setGrantee(e.target.value)}
          placeholder="user id or role name"
          style={{ ...input, flex: 2 }}
        />
        <select
          value={permission}
          onChange={(e) => setPermission(e.target.value)}
          style={{ ...input, flex: 1 }}
        >
          <option value="view">view</option>
          <option value="edit">edit</option>
        </select>
        <button onClick={() => create.mutate()} disabled={!grantee.trim()}>
          Share
        </button>
      </div>
    </section>
  );
}

// ---------- shared style chunks ----------

const card: React.CSSProperties = {
  border: "1px solid #e5e7eb",
  borderRadius: 6,
  padding: 12,
  marginBottom: 12,
};
const h3: React.CSSProperties = { margin: "0 0 8px", fontSize: 14 };
const label: React.CSSProperties = {
  display: "block",
  fontSize: 12,
  color: "#374151",
  marginTop: 6,
};
const input: React.CSSProperties = {
  padding: "4px 6px",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
  width: "100%",
};
const chip: React.CSSProperties = {
  background: "#eef2ff",
  color: "#3730a3",
  padding: "2px 6px",
  borderRadius: 4,
  fontSize: 12,
};
const chipX: React.CSSProperties = {
  background: "none",
  border: "none",
  cursor: "pointer",
  color: "#3730a3",
  fontWeight: 700,
};

function stringify(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

function coerce(raw: string): unknown {
  if (raw === "") return "";
  // Let the user enter JSON for complex values (arrays for `in`,
  // pair-objects for `between`, booleans, numbers). Fall back to
  // the raw string so "hello" without quotes is still a string
  // filter value instead of a parse error.
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}
