import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import {
  Button,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
      <p className="text-[13px] text-fg-muted">
        Toggle optional capabilities for the current tenant. Disabled features
        return 403 from the API and are hidden from the navigation sidebar.
      </p>
      <Table className="mt-4">
        <TableHeader>
          <TableRow>
            <TableHead>Feature</TableHead>
            <TableHead>Enabled</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {keys.map((k) => (
            <TableRow key={k}>
              <TableCell className="capitalize">{k}</TableCell>
              <TableCell>
                <label className="flex cursor-pointer items-center gap-1.5">
                  <input
                    type="checkbox"
                    checked={!!current[k]}
                    onChange={() => toggle(k)}
                  />
                  <span>{current[k] ? "on" : "off"}</span>
                </label>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <div className="mt-4 flex gap-2">
        <Button
          type="button"
          disabled={!dirty || update.isPending}
          onClick={() => {
            if (pending) update.mutate(pending);
          }}
        >
          {update.isPending ? "Saving…" : "Save"}
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={!dirty || update.isPending}
          onClick={() => setPending(null)}
        >
          Reset
        </Button>
      </div>
      {plansQuery.data && (
        <p className="mt-6 text-xs text-fg-muted">
          Plans on file: {plansQuery.data.plans.map((p) => p.name).join(", ")}
        </p>
      )}
    </section>
  );
}
