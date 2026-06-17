import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ToggleRight } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardFooter,
  EmptyState,
  Skeleton,
} from "@kapp/ui";
import { api } from "../lib/api";
import { tenantKey, useTenantName } from "../lib/tenant";
import { humanizeLabel, humanizeToken } from "../lib/ktypeView";
import { AdminErrorState, AdminPageHeader, Toggle } from "./adminKit";

export function TenantFeaturesPage() {
  const qc = useQueryClient();
  const tenantId = tenantKey();
  const { name: tenantName } = useTenantName();

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
      void qc.invalidateQueries({ queryKey: ["tenant-features", tenantId] });
    },
  });

  const saved = featuresQuery.data?.features ?? {};
  const current = pending ?? saved;
  const keys = useMemo(() => Object.keys(current).sort(), [current]);
  const dirty = pending !== null;
  const enabledCount = keys.filter((k) => current[k]).length;

  const toggle = (key: string) =>
    setPending({ ...current, [key]: !current[key] });

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Features"
        description="Turn optional capabilities on or off for this workspace. Disabled features are hidden from the sidebar and rejected by the API."
        actions={
          <Badge variant="neutral" size="md">
            {tenantName}
          </Badge>
        }
      />

      {featuresQuery.isLoading ? (
        <Card>
          <CardContent className="flex flex-col gap-3 pt-4">
            {Array.from({ length: 5 }).map((_, i) => (
              <div key={i} className="flex items-center justify-between gap-3">
                <Skeleton variant="text" className="w-48" />
                <Skeleton variant="rect" className="h-5 w-9" />
              </div>
            ))}
          </CardContent>
        </Card>
      ) : featuresQuery.error ? (
        <AdminErrorState
          title="Couldn't load features"
          error={featuresQuery.error}
          onRetry={() => featuresQuery.refetch()}
        />
      ) : keys.length === 0 ? (
        <EmptyState
          icon={<ToggleRight />}
          title="No optional features"
          description="This workspace has no toggleable features configured. Features become available as add-ons are provisioned for the plan."
        />
      ) : (
        <Card>
          <CardContent className="pt-2">
            <p className="px-1 py-2 text-xs text-fg-muted">
              {enabledCount} of {keys.length} enabled
            </p>
            <ul className="divide-y divide-border">
              {keys.map((k) => {
                const on = !!current[k];
                return (
                  <li
                    key={k}
                    className="flex items-center justify-between gap-3 py-3"
                  >
                    <div className="min-w-0">
                      <p className="truncate font-medium text-fg" title={k}>
                        {humanizeLabel(k)}
                      </p>
                      <p className="text-xs text-fg-muted">
                        {on ? "Enabled" : "Disabled"}
                      </p>
                    </div>
                    <Toggle
                      checked={on}
                      onChange={() => toggle(k)}
                      label={`Toggle ${humanizeLabel(k)}`}
                    />
                  </li>
                );
              })}
            </ul>
          </CardContent>
          <CardFooter className="flex items-center justify-between gap-3">
            <span className="text-xs text-fg-muted">
              {dirty ? "You have unsaved changes." : "All changes saved."}
            </span>
            <div className="flex gap-2">
              <Button
                type="button"
                variant="outline"
                disabled={!dirty || update.isPending}
                onClick={() => setPending(null)}
              >
                Reset
              </Button>
              <Button
                type="button"
                disabled={!dirty || update.isPending}
                onClick={() => {
                  if (pending) update.mutate(pending);
                }}
              >
                {update.isPending ? "Saving…" : "Save changes"}
              </Button>
            </div>
          </CardFooter>
        </Card>
      )}

      {plansQuery.data && plansQuery.data.plans.length > 0 && (
        <div className="flex flex-col gap-2">
          <h2 className="text-sm font-medium text-fg-muted">Plans on file</h2>
          <div className="flex flex-wrap gap-2">
            {plansQuery.data.plans.map((p) => (
              <Badge key={p.name} variant="outline">
                {p.display_name || humanizeToken(p.name)}
              </Badge>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}
