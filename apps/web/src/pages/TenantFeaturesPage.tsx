import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../lib/api";

const tenantKey = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

export function TenantFeaturesPage() {
  const qc = useQueryClient();
  const tenantId = tenantKey();
  const featuresQuery = useQuery({
    queryKey: ["tenant-features", tenantId],
    queryFn: () => api.listTenantFeatures(tenantId),
  });
  const plansQuery = useQuery({
    queryKey: ["plans"],
    queryFn: () => api.listPlans(),
  });
  const [pending, setPending] = useState<Record<string, boolean> | null>(null);
  const update = useMutation({
    mutationFn: (features: Record<string, boolean>) =>
      api.updateTenantFeatures(tenantId, features),
    onSuccess: () => {
      setPending(null);
      qc.invalidateQueries({ queryKey: ["tenant-features", tenantId] });
    },
  });

  if (featuresQuery.isLoading) return <div>Loading features…</div>;
  if (featuresQuery.error) return <div>Error loading features.</div>;

  const current = pending ?? featuresQuery.data?.features ?? {};
  const keys = Object.keys(current).sort();
  const dirty = pending !== null;

  const toggle = (key: string) => {
    setPending({ ...current, [key]: !current[key] });
  };

  return (
    <section>
      <h1>Features</h1>
      <p style={{ color: "#6b7280", fontSize: 13 }}>
        Toggle optional capabilities for the current tenant. Disabled features
        return 403 from the API and are hidden from the navigation sidebar.
      </p>
      <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 16 }}>
        <thead>
          <tr>
            <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb", padding: "6px 4px" }}>
              Feature
            </th>
            <th style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb", padding: "6px 4px" }}>
              Enabled
            </th>
          </tr>
        </thead>
        <tbody>
          {keys.map((k) => (
            <tr key={k} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={{ padding: "6px 4px", textTransform: "capitalize" }}>{k}</td>
              <td style={{ padding: "6px 4px" }}>
                <label style={{ cursor: "pointer" }}>
                  <input
                    type="checkbox"
                    checked={!!current[k]}
                    onChange={() => toggle(k)}
                  />
                  <span style={{ marginLeft: 6 }}>{current[k] ? "on" : "off"}</span>
                </label>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ marginTop: 16, display: "flex", gap: 8 }}>
        <button
          type="button"
          disabled={!dirty || update.isPending}
          onClick={() => {
            if (pending) update.mutate(pending);
          }}
        >
          {update.isPending ? "Saving…" : "Save"}
        </button>
        <button
          type="button"
          disabled={!dirty || update.isPending}
          onClick={() => setPending(null)}
        >
          Reset
        </button>
      </div>
      {plansQuery.data && (
        <p style={{ marginTop: 24, fontSize: 12, color: "#6b7280" }}>
          Plans on file: {plansQuery.data.plans.map((p) => p.name).join(", ")}
        </p>
      )}
    </section>
  );
}
