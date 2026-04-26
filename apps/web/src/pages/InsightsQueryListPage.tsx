import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import type { InsightsQuery } from "@kapp/client";
import { api } from "../lib/api";

/**
 * InsightsQueryListPage renders the saved queries for the current
 * tenant. It is the landing page for /insights/queries and links into
 * the builder (one row per query) plus a "new" button that deep-links
 * to the blank-slate builder at /insights/queries/new.
 */
export function InsightsQueryListPage() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const list = useQuery<{ queries: InsightsQuery[] }>({
    queryKey: ["insights", "queries"],
    queryFn: () => api.listInsightsQueries(),
  });

  const del = useMutation({
    mutationFn: (id: string) => api.deleteInsightsQuery(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["insights", "queries"] }),
  });

  return (
    <section>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "baseline",
          marginBottom: 8,
        }}
      >
        <h1 style={{ margin: 0 }}>Insights queries</h1>
        <button onClick={() => nav("/insights/queries/new")}>+ New query</button>
      </div>
      <p style={{ color: "#6b7280", marginTop: 0 }}>
        Saved query definitions execute under per-tenant statement_timeout and
        TTL-cached results. Share a query to let other users or roles run it.
      </p>

      {list.isLoading && <p>Loading…</p>}
      {list.error && (
        <p style={{ color: "#b91c1c" }}>Failed to load: {(list.error as Error).message}</p>
      )}
      {list.data && list.data.queries.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No saved queries yet.
        </p>
      )}
      {list.data && list.data.queries.length > 0 && (
        <table style={{ borderCollapse: "collapse", width: "100%", fontSize: 13 }}>
          <thead>
            <tr style={{ background: "#f9fafb" }}>
              <th style={th}>Name</th>
              <th style={th}>Source</th>
              <th style={th}>Cache TTL</th>
              <th style={th}>Updated</th>
              <th style={th} />
            </tr>
          </thead>
          <tbody>
            {list.data.queries.map((q) => (
              <tr key={q.id}>
                <td style={td}>
                  <Link to={`/insights/queries/${q.id}`}>{q.name}</Link>
                </td>
                <td style={td}>{q.definition.source}</td>
                <td style={td}>{ttlLabel(q.cache_ttl_seconds)}</td>
                <td style={td}>{new Date(q.updated_at).toLocaleString()}</td>
                <td style={td}>
                  <button
                    onClick={() => {
                      if (confirm(`Delete "${q.name}"?`)) del.mutate(q.id);
                    }}
                    style={{ color: "#b91c1c" }}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

const th: React.CSSProperties = {
  textAlign: "left",
  padding: "6px 8px",
  borderBottom: "1px solid #e5e7eb",
};

const td: React.CSSProperties = {
  padding: "6px 8px",
  borderBottom: "1px solid #f3f4f6",
};

function ttlLabel(ttl: number | null | undefined): string {
  if (ttl == null) return "default (300s)";
  if (ttl === 0) return "disabled";
  return `${ttl}s`;
}
