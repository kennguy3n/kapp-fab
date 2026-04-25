import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import type { PlacementPolicy } from "@kapp/client";
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
          <p style={{ color: "#6b7280" }}>
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
      <p style={{ color: "#6b7280", fontSize: 13 }}>
        Edit the ZK Object Fabric placement policy for this tenant. The policy
        controls encryption mode, provider allow-list, country residency, and
        the cache location hint. Changes are forwarded to the fabric console
        and persisted locally on success.
      </p>
      <textarea
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        spellCheck={false}
        style={{
          width: "100%",
          minHeight: 360,
          fontFamily: "monospace",
          fontSize: 13,
          padding: 8,
          border: "1px solid #e5e7eb",
          borderRadius: 4,
        }}
      />
      {error && (
        <p style={{ color: "#dc2626", marginTop: 8, fontSize: 13 }}>{error}</p>
      )}
      <div style={{ marginTop: 12, display: "flex", gap: 8 }}>
        <button
          type="button"
          disabled={!dirty || update.isPending}
          onClick={onSave}
        >
          {update.isPending ? "Saving…" : "Save"}
        </button>
        <button
          type="button"
          disabled={!dirty || update.isPending}
          onClick={() =>
            policyQuery.data &&
            setDraft(JSON.stringify(policyQuery.data, null, 2))
          }
        >
          Reset
        </button>
      </div>
    </section>
  );
}
