import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import type { InsightsDashboard } from "@kapp/client";
import { api } from "../lib/api";

/**
 * InsightsDashboardListPage is the entry point for /insights/dashboards.
 * It lists saved dashboards and lets the user spin up a new one, which
 * then redirects into the dashboard editor. Creation is the only place
 * name/description are captured — the editor itself keeps them editable
 * via Update.
 */
export function InsightsDashboardListPage() {
  const qc = useQueryClient();
  const nav = useNavigate();
  const list = useQuery<{ dashboards: InsightsDashboard[] }>({
    queryKey: ["insights", "dashboards"],
    queryFn: () => api.listInsightsDashboards(),
  });
  const [newName, setNewName] = useState("");

  const create = useMutation({
    mutationFn: () =>
      api.createInsightsDashboard({ name: newName.trim(), auto_refresh_seconds: 0 }),
    onSuccess: (d) => {
      qc.invalidateQueries({ queryKey: ["insights", "dashboards"] });
      setNewName("");
      nav(`/insights/dashboards/${d.id}`);
    },
  });

  const del = useMutation({
    mutationFn: (id: string) => api.deleteInsightsDashboard(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["insights", "dashboards"] }),
  });

  return (
    <section>
      <h1>Insights dashboards</h1>
      <p style={{ color: "#6b7280" }}>
        A dashboard bundles saved queries into grid-arranged widgets. The
        GET endpoint resolves every widget's query cache-first so rendering a
        dashboard is cheap once its queries are warm.
      </p>

      <div style={{ display: "flex", gap: 6, marginBottom: 12 }}>
        <input
          value={newName}
          onChange={(e) => setNewName(e.target.value)}
          placeholder="new dashboard name"
          style={{
            flex: 1,
            padding: "4px 6px",
            border: "1px solid #d1d5db",
            borderRadius: 4,
          }}
        />
        <button
          onClick={() => create.mutate()}
          disabled={!newName.trim() || create.isPending}
        >
          + New dashboard
        </button>
      </div>

      {list.isLoading && <p>Loading…</p>}
      {list.data && list.data.dashboards.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No dashboards yet.
        </p>
      )}
      {list.data && list.data.dashboards.length > 0 && (
        <table style={{ borderCollapse: "collapse", width: "100%", fontSize: 13 }}>
          <thead>
            <tr style={{ background: "#f9fafb" }}>
              <th style={th}>Name</th>
              <th style={th}>Auto-refresh</th>
              <th style={th}>Updated</th>
              <th style={th} />
            </tr>
          </thead>
          <tbody>
            {list.data.dashboards.map((d) => (
              <tr key={d.id}>
                <td style={td}>
                  <Link to={`/insights/dashboards/${d.id}`}>{d.name}</Link>
                </td>
                <td style={td}>
                  {d.auto_refresh_seconds
                    ? `${d.auto_refresh_seconds}s`
                    : "off"}
                </td>
                <td style={td}>{new Date(d.updated_at).toLocaleString()}</td>
                <td style={td}>
                  <button
                    onClick={() => {
                      if (confirm(`Delete "${d.name}"?`)) del.mutate(d.id);
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
