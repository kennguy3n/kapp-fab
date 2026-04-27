// Phase L Insights — share modal.
//
// Reused by the QueryBuilder and Dashboard pages to grant view/edit
// access on a saved query or a dashboard to a user (by id) or a role
// (by name). Lists the existing grants so the caller can revoke them
// in-place, and posts new grants via the appropriate /share route.

import { useState } from "react";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import type {
  InsightsGranteeType,
  InsightsPermission,
  InsightsShare,
} from "@kapp/client";
import { api } from "../../lib/api";

export type ShareResource = "query" | "dashboard";

interface ShareModalProps {
  resource: ShareResource;
  resourceId: string;
  resourceName: string;
  onClose: () => void;
}

function listShares(resource: ShareResource, id: string) {
  return resource === "query"
    ? api.listInsightsQueryShares(id)
    : api.listInsightsDashboardShares(id);
}

function postShare(
  resource: ShareResource,
  id: string,
  input: { grantee_type: InsightsGranteeType; grantee: string; permission: InsightsPermission }
) {
  return resource === "query"
    ? api.shareInsightsQuery(id, input)
    : api.shareInsightsDashboard(id, input);
}

function deleteShare(resource: ShareResource, id: string, shareId: string) {
  return resource === "query"
    ? api.deleteInsightsQueryShare(id, shareId)
    : api.deleteInsightsDashboardShare(id, shareId);
}

export function ShareModal({
  resource,
  resourceId,
  resourceName,
  onClose,
}: ShareModalProps) {
  const qc = useQueryClient();
  const sharesQuery = useQuery<{ shares: InsightsShare[] }>({
    queryKey: ["insights-shares", resource, resourceId],
    queryFn: () => listShares(resource, resourceId),
  });

  const [granteeType, setGranteeType] = useState<InsightsGranteeType>("user");
  const [grantee, setGrantee] = useState("");
  const [permission, setPermission] = useState<InsightsPermission>("view");
  const [error, setError] = useState<string | null>(null);

  const createMut = useMutation({
    mutationFn: () =>
      postShare(resource, resourceId, {
        grantee_type: granteeType,
        grantee,
        permission,
      }),
    onSuccess: () => {
      setGrantee("");
      setError(null);
      qc.invalidateQueries({
        queryKey: ["insights-shares", resource, resourceId],
      });
    },
    onError: (err: Error) => setError(err.message),
  });

  const deleteMut = useMutation({
    mutationFn: (shareId: string) =>
      deleteShare(resource, resourceId, shareId),
    onSuccess: () =>
      qc.invalidateQueries({
        queryKey: ["insights-shares", resource, resourceId],
      }),
    onError: (err: Error) => setError(err.message),
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!grantee.trim()) {
      setError("grantee required");
      return;
    }
    createMut.mutate();
  };

  return (
    <div
      role="dialog"
      aria-modal="true"
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(15, 23, 42, 0.45)",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        zIndex: 1000,
      }}
      onClick={onClose}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: "white",
          borderRadius: 8,
          padding: 20,
          minWidth: 480,
          maxWidth: 640,
          maxHeight: "90vh",
          overflow: "auto",
          boxShadow: "0 10px 25px rgba(0,0,0,0.2)",
        }}
      >
        <div
          style={{
            display: "flex",
            justifyContent: "space-between",
            alignItems: "center",
            marginBottom: 12,
          }}
        >
          <h3 style={{ margin: 0 }}>
            Share {resource} — {resourceName}
          </h3>
          <button onClick={onClose} aria-label="close">
            ✕
          </button>
        </div>

        <form
          onSubmit={submit}
          style={{
            display: "grid",
            gridTemplateColumns: "120px 1fr 120px auto",
            gap: 8,
            alignItems: "center",
            marginBottom: 12,
          }}
        >
          <select
            value={granteeType}
            onChange={(e) =>
              setGranteeType(e.target.value as InsightsGranteeType)
            }
          >
            <option value="user">User (id)</option>
            <option value="role">Role (name)</option>
          </select>
          <input
            value={grantee}
            placeholder={
              granteeType === "user" ? "user uuid" : "role name (e.g. analyst)"
            }
            onChange={(e) => setGrantee(e.target.value)}
          />
          <select
            value={permission}
            onChange={(e) =>
              setPermission(e.target.value as InsightsPermission)
            }
          >
            <option value="view">View</option>
            <option value="edit">Edit</option>
          </select>
          <button type="submit" disabled={createMut.isPending}>
            {createMut.isPending ? "Sharing…" : "Share"}
          </button>
        </form>

        {error && (
          <div style={{ color: "#dc2626", fontSize: 13, marginBottom: 8 }}>
            {error}
          </div>
        )}

        <h4 style={{ fontSize: 14, margin: "16px 0 8px" }}>Existing shares</h4>
        {sharesQuery.isLoading && <p>Loading…</p>}
        {!sharesQuery.isLoading &&
          (sharesQuery.data?.shares ?? []).length === 0 && (
            <p style={{ color: "#9ca3af", fontStyle: "italic", fontSize: 13 }}>
              Not shared with anyone yet.
            </p>
          )}
        <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
          {(sharesQuery.data?.shares ?? []).map((s) => (
            <li
              key={s.id}
              style={{
                display: "flex",
                justifyContent: "space-between",
                alignItems: "center",
                padding: "6px 0",
                borderBottom: "1px solid #f3f4f6",
                fontSize: 13,
              }}
            >
              <span>
                <strong>{s.grantee_type}</strong>: {s.grantee}{" "}
                <span style={{ color: "#6b7280" }}>({s.permission})</span>
              </span>
              <button
                onClick={() => deleteMut.mutate(s.id)}
                disabled={deleteMut.isPending}
                style={{ color: "#dc2626" }}
              >
                Revoke
              </button>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
