import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import type { RetentionPolicy } from "@kapp/client";
import { api } from "../lib/api";

const tenantKey = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

// Categories the platform's RetentionSweeper knows how to delete. The
// list mirrors `retentionTargets` in internal/platform/retention.go;
// the UI seeds a row per category so operators can edit even when the
// wizard hasn't backfilled defaults yet.
const CATEGORIES = [
  "audit_log",
  "events",
  "sla_log",
  "webhook_deliveries",
  "notifications",
  "import_staging",
] as const;

type DraftRow = {
  category: string;
  retention_days: number;
  enabled: boolean;
};

// RetentionPoliciesPage is the per-category editor for the retention
// sweeper. PUT one (category, retention_days, enabled) at a time to
// avoid the all-or-nothing failure mode of a bulk save.
export function RetentionPoliciesPage() {
  const qc = useQueryClient();
  const tenantId = tenantKey();
  const policiesQuery = useQuery({
    queryKey: ["retention-policies", tenantId],
    queryFn: () => api.listRetentionPolicies(tenantId),
  });

  const initialDrafts: Record<string, DraftRow> = useMemo(() => {
    const out: Record<string, DraftRow> = {};
    for (const c of CATEGORIES) {
      out[c] = { category: c, retention_days: 90, enabled: true };
    }
    for (const p of policiesQuery.data?.policies ?? []) {
      out[p.category] = {
        category: p.category,
        retention_days: p.retention_days,
        enabled: p.enabled,
      };
    }
    return out;
  }, [policiesQuery.data]);

  const [drafts, setDrafts] = useState<Record<string, DraftRow>>({});
  const effective: Record<string, DraftRow> = { ...initialDrafts, ...drafts };

  const mutation = useMutation({
    mutationFn: (row: DraftRow) =>
      api.upsertRetentionPolicy(tenantId, row),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["retention-policies", tenantId] });
      setDrafts({});
    },
  });

  if (policiesQuery.isLoading) return <div>Loading retention policies…</div>;
  if (policiesQuery.error) {
    const msg =
      policiesQuery.error instanceof Error
        ? policiesQuery.error.message
        : String(policiesQuery.error);
    return <div>Error loading retention policies: {msg}</div>;
  }

  const updateField = (category: string, patch: Partial<DraftRow>) => {
    setDrafts((d) => ({
      ...d,
      [category]: { ...effective[category], ...patch },
    }));
  };

  const isDirty = (category: string): boolean => {
    const a = drafts[category];
    if (!a) return false;
    const b = initialDrafts[category];
    return (
      a.retention_days !== b.retention_days || a.enabled !== b.enabled
    );
  };

  const policyByCat: Record<string, RetentionPolicy | undefined> = {};
  for (const p of policiesQuery.data?.policies ?? []) {
    policyByCat[p.category] = p;
  }

  return (
    <section>
      <h1>Data retention</h1>
      <p style={{ color: "#6b7280", fontSize: 13 }}>
        Configure how long the platform keeps each category of operational
        data. The daily sweep deletes rows older than the chosen retention
        window per tenant. Disable a category to skip the sweep without
        losing the configured days.
      </p>
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={{ padding: "8px 4px" }}>Category</th>
            <th style={{ padding: "8px 4px" }}>Days</th>
            <th style={{ padding: "8px 4px" }}>Enabled</th>
            <th style={{ padding: "8px 4px" }}>Last updated</th>
            <th style={{ padding: "8px 4px" }}></th>
          </tr>
        </thead>
        <tbody>
          {CATEGORIES.map((c) => {
            const row = effective[c];
            const updated = policyByCat[c]?.updated_at;
            return (
              <tr key={c} style={{ borderBottom: "1px solid #f3f4f6" }}>
                <td style={{ padding: "8px 4px", fontFamily: "monospace" }}>
                  {c}
                </td>
                <td style={{ padding: "8px 4px" }}>
                  <input
                    type="number"
                    min={1}
                    max={3650}
                    value={row.retention_days}
                    onChange={(e) =>
                      updateField(c, {
                        retention_days: Number(e.target.value),
                      })
                    }
                    style={{ width: 80 }}
                  />
                </td>
                <td style={{ padding: "8px 4px" }}>
                  <input
                    type="checkbox"
                    checked={row.enabled}
                    onChange={(e) =>
                      updateField(c, { enabled: e.target.checked })
                    }
                  />
                </td>
                <td style={{ padding: "8px 4px", color: "#6b7280", fontSize: 12 }}>
                  {updated ?? "—"}
                </td>
                <td style={{ padding: "8px 4px" }}>
                  <button
                    type="button"
                    disabled={!isDirty(c) || mutation.isPending}
                    onClick={() => mutation.mutate(row)}
                  >
                    {mutation.isPending ? "Saving…" : "Save"}
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {mutation.error && (
        <p style={{ color: "#dc2626", marginTop: 12, fontSize: 13 }}>
          {mutation.error instanceof Error
            ? mutation.error.message
            : String(mutation.error)}
        </p>
      )}
    </section>
  );
}
