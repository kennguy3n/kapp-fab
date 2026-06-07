import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import type { PlacementPolicy } from "@kapp/client";
import { Button } from "@kapp/ui";
import { api } from "../lib/api";

const tenantKey = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";

// PlacementPolicyPage is the JSON editor for a tenant's ZK Object
// Fabric placement policy. Free-plan tenants see the platform-derived
// default and a notice that the editor is read-only; paid plans can
// edit the policy and PUT it back, which forwards to the fabric
// console and persists locally on success.
export function PlacementPolicyPage() {
  const qc = useQueryClient();
  const tenantId = tenantKey();
  const policyQuery = useQuery({
    queryKey: ["placement-policy", tenantId],
    queryFn: () => api.getPlacementPolicy(tenantId),
  });
  const [draft, setDraft] = useState<string>("");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (policyQuery.data) {
      setDraft(JSON.stringify(policyQuery.data, null, 2));
      setError(null);
    }
  }, [policyQuery.data]);

  const update = useMutation({
    mutationFn: (policy: PlacementPolicy) =>
      api.updatePlacementPolicy(tenantId, policy),
    onSuccess: () => {
      setError(null);
      qc.invalidateQueries({ queryKey: ["placement-policy", tenantId] });
    },
    onError: (err: unknown) => {
      setError(err instanceof Error ? err.message : String(err));
    },
  });

  const dirty = useMemo(() => {
    if (!policyQuery.data) return false;
    return draft !== JSON.stringify(policyQuery.data, null, 2);
  }, [draft, policyQuery.data]);

  if (policyQuery.isLoading) return <div>Loading placement policy…</div>;
  if (policyQuery.error) {
    const msg =
      policyQuery.error instanceof Error
        ? policyQuery.error.message
        : String(policyQuery.error);
    if (msg.includes("free")) {
      return (
        <section>
          <h1>Placement policy</h1>
          <p className="text-fg-muted">
            Placement policy customisation is available on paid plans only.
            Upgrade to choose providers, country residency, and encryption mode.
          </p>
        </section>
      );
    }
    return <div>Error loading placement policy: {msg}</div>;
  }

  const onSave = () => {
    try {
      const parsed = JSON.parse(draft) as PlacementPolicy;
      update.mutate(parsed);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <section>
      <h1>Placement policy</h1>
      <p className="text-[13px] text-fg-muted">
        Edit the ZK Object Fabric placement policy for this tenant. The policy
        controls encryption mode, provider allow-list, country residency, and
        the cache location hint. Changes are forwarded to the fabric console
        and persisted locally on success.
      </p>
      <textarea
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        spellCheck={false}
        className="min-h-[360px] w-full rounded border border-border p-2 font-mono text-[13px]"
      />
      {error && (
        <p className="mt-2 text-[13px] text-danger">{error}</p>
      )}
      <div className="mt-3 flex gap-2">
        <Button
          type="button"
          disabled={!dirty || update.isPending}
          onClick={onSave}
        >
          {update.isPending ? "Saving…" : "Save"}
        </Button>
        <Button
          type="button"
          variant="outline"
          disabled={!dirty || update.isPending}
          onClick={() =>
            policyQuery.data &&
            setDraft(JSON.stringify(policyQuery.data, null, 2))
          }
        >
          Reset
        </Button>
      </div>
    </section>
  );
}
