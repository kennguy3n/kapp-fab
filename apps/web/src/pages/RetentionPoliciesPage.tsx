import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import type { RetentionPolicy } from "@kapp/client";
import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";
import { tenantKey } from "../lib/tenant";

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
      <p className="text-[13px] text-fg-muted">
        Configure how long the platform keeps each category of operational
        data. The daily sweep deletes rows older than the chosen retention
        window per tenant. Disable a category to skip the sweep without
        losing the configured days.
      </p>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Category</TableHead>
            <TableHead>Days</TableHead>
            <TableHead>Enabled</TableHead>
            <TableHead>Last updated</TableHead>
            <TableHead></TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {CATEGORIES.map((c) => {
            const row = effective[c];
            const updated = policyByCat[c]?.updated_at;
            return (
              <TableRow key={c}>
                <TableCell className="font-mono">{c}</TableCell>
                <TableCell>
                  <Input
                    type="number"
                    min={1}
                    max={3650}
                    value={row.retention_days}
                    onChange={(e) =>
                      updateField(c, {
                        retention_days: Number(e.target.value),
                      })
                    }
                    className="w-20"
                  />
                </TableCell>
                <TableCell>
                  <input
                    type="checkbox"
                    checked={row.enabled}
                    onChange={(e) =>
                      updateField(c, { enabled: e.target.checked })
                    }
                  />
                </TableCell>
                <TableCell className="text-xs text-fg-muted">
                  {updated ?? "—"}
                </TableCell>
                <TableCell>
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    disabled={!isDirty(c) || mutation.isPending}
                    onClick={() => mutation.mutate(row)}
                  >
                    {mutation.isPending ? "Saving…" : "Save"}
                  </Button>
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      {mutation.error && (
        <p className="mt-3 text-[13px] text-danger">
          {mutation.error instanceof Error
            ? mutation.error.message
            : String(mutation.error)}
        </p>
      )}
    </section>
  );
}
