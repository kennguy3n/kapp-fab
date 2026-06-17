import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { RetentionPolicy } from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { tenantKey, useTenantName } from "../lib/tenant";
import { useFormatter } from "../lib/i18n";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
  Toggle,
} from "./adminKit";

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

const CATEGORY_META: Record<string, { label: string; description: string }> = {
  audit_log: {
    label: "Audit log",
    description: "Security and change history",
  },
  events: { label: "Events", description: "Internal domain events" },
  sla_log: { label: "SLA log", description: "SLA timer history" },
  webhook_deliveries: {
    label: "Webhook deliveries",
    description: "Outbound delivery attempts",
  },
  notifications: {
    label: "Notifications",
    description: "In-app and email notifications",
  },
  import_staging: {
    label: "Import staging",
    description: "Temporary staged import rows",
  },
};

const TABLE_COLUMNS = ["Category", "Retention", "Status", "Last updated", ""];

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
  const fmt = useFormatter();
  const tenantId = tenantKey();
  const { name: tenantName } = useTenantName();

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
    mutationFn: (row: DraftRow) => api.upsertRetentionPolicy(tenantId, row),
    onSuccess: (_data, row) => {
      toast.success(
        `${CATEGORY_META[row.category]?.label ?? row.category} retention saved`,
      );
      void qc.invalidateQueries({ queryKey: ["retention-policies", tenantId] });
      setDrafts((d) => {
        const next = { ...d };
        delete next[row.category];
        return next;
      });
    },
    onError: (err) => {
      toast.error(err instanceof Error ? err.message : "Couldn't save");
    },
  });

  const updateField = (category: string, patch: Partial<DraftRow>) => {
    setDrafts((d) => ({
      ...d,
      [category]: { ...effective[category]!, ...patch },
    }));
  };

  const isDirty = (category: string): boolean => {
    const a = drafts[category];
    if (!a) return false;
    const b = initialDrafts[category]!;
    return a.retention_days !== b.retention_days || a.enabled !== b.enabled;
  };

  const policyByCat: Record<string, RetentionPolicy | undefined> = {};
  for (const p of policiesQuery.data?.policies ?? []) {
    policyByCat[p.category] = p;
  }

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Data retention"
        description="Choose how long the platform keeps each category of operational data. A daily sweep deletes rows older than the retention window. Turn a category off to pause its sweep without losing the configured days."
        actions={
          <Badge variant="neutral" size="md">
            {tenantName}
          </Badge>
        }
      />

      {policiesQuery.isLoading ? (
        <AdminTableSkeleton columns={TABLE_COLUMNS} rows={CATEGORIES.length} />
      ) : policiesQuery.error ? (
        <AdminErrorState
          title="Couldn't load retention policies"
          error={policiesQuery.error}
          onRetry={() => policiesQuery.refetch()}
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Category</TableHead>
              <TableHead className="text-end">Retention</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Last updated</TableHead>
              <TableHead className="text-end">
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {CATEGORIES.map((c) => {
              const row = effective[c]!;
              const meta = CATEGORY_META[c];
              const updated = policyByCat[c]?.updated_at;
              const invalid =
                !Number.isFinite(row.retention_days) ||
                row.retention_days < 1 ||
                row.retention_days > 3650;
              const savingThis =
                mutation.isPending && mutation.variables?.category === c;
              return (
                <TableRow key={c}>
                  <TableCell>
                    <div className="flex flex-col">
                      <span className="font-medium text-fg">
                        {meta?.label ?? c}
                      </span>
                      <span className="text-xs text-fg-muted">
                        {meta?.description}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell className="text-end">
                    <div className="flex justify-end">
                      <Input
                        type="number"
                        min={1}
                        max={3650}
                        size="sm"
                        value={row.retention_days}
                        aria-label={`${meta?.label ?? c} retention in days`}
                        invalid={invalid}
                        trailingAddon="days"
                        onChange={(e) =>
                          updateField(c, {
                            retention_days: Number(e.target.value),
                          })
                        }
                        className="w-28 text-end font-tabular"
                      />
                    </div>
                  </TableCell>
                  <TableCell>
                    <div className="flex items-center gap-2">
                      <Toggle
                        checked={row.enabled}
                        onChange={(next) => updateField(c, { enabled: next })}
                        label={`Enable retention sweep for ${meta?.label ?? c}`}
                      />
                      <span className="text-xs text-fg-muted">
                        {row.enabled ? "On" : "Off"}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell className="text-xs text-fg-muted">
                    {updated ? fmt.dateTime(new Date(updated)) : "Never"}
                  </TableCell>
                  <TableCell className="text-end">
                    <Button
                      type="button"
                      size="sm"
                      variant="outline"
                      disabled={!isDirty(c) || invalid || mutation.isPending}
                      onClick={() => mutation.mutate(row)}
                    >
                      {savingThis ? "Saving…" : "Save"}
                    </Button>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
