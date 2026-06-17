// Insights — share modal.
//
// Reused by the QueryBuilder and Dashboard pages to grant view/edit
// access on a saved query or a dashboard to a user (by id) or a role
// (by name). Lists the existing grants so the caller can revoke them
// in-place, and posts new grants via the appropriate /share route.

import { useEffect, useState } from "react";
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
import { Button, Field, Input, Select, Skeleton } from "@kapp/ui";
import { humanizeToken } from "../../lib/ktypeView";
import { api } from "../../lib/api";

export type ShareResource = "query" | "dashboard";

interface ShareModalProps {
  resource: ShareResource;
  resourceId: string;
  resourceName: string;
  onClose: () => void;
}

const PERMISSION_LABEL: Record<InsightsPermission, string> = {
  view: "View",
  edit: "Edit",
};

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

  // Escape-to-close, matching the app's modal convention.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

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

  const shares = sharesQuery.data?.shares ?? [];

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={`Share ${resource}`}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 backdrop-blur-sm"
      onClick={onClose}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        className="flex max-h-[90vh] w-full max-w-xl flex-col overflow-auto rounded-lg border border-border bg-bg-elevated text-fg shadow-lg"
      >
        <div className="flex items-start justify-between gap-3 border-b border-border px-5 py-4">
          <div className="min-w-0">
            <h3 className="text-base font-semibold text-fg">
              Share {resource} — {resourceName}
            </h3>
            <p className="mt-0.5 text-sm text-fg-muted">
              Give a teammate or role permission to view or edit this{" "}
              {resource}.
            </p>
          </div>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </Button>
        </div>

        <div className="flex flex-col gap-4 px-5 py-4">
          <form onSubmit={submit} className="flex flex-col gap-3">
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-[140px_1fr_130px]">
              <Field label="Share with">
                <Select
                  value={granteeType}
                  onChange={(e) =>
                    setGranteeType(e.target.value as InsightsGranteeType)
                  }
                >
                  <option value="user">A person</option>
                  <option value="role">A role</option>
                </Select>
              </Field>
              <Field label={granteeType === "user" ? "User ID" : "Role name"}>
                <Input
                  value={grantee}
                  placeholder={
                    granteeType === "user"
                      ? "User UUID"
                      : "Role name (e.g. analyst)"
                  }
                  onChange={(e) => setGrantee(e.target.value)}
                />
              </Field>
              <Field label="Access">
                <Select
                  value={permission}
                  onChange={(e) =>
                    setPermission(e.target.value as InsightsPermission)
                  }
                >
                  <option value="view">View</option>
                  <option value="edit">Edit</option>
                </Select>
              </Field>
            </div>
            {error && (
              <div
                role="alert"
                className="rounded-md border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-danger"
              >
                {error}
              </div>
            )}
            <div className="flex justify-end">
              <Button type="submit" disabled={createMut.isPending}>
                {createMut.isPending ? "Sharing…" : "Share"}
              </Button>
            </div>
          </form>

          <div>
            <h4 className="mb-2 text-sm font-semibold text-fg">
              Who has access
            </h4>
            {sharesQuery.isLoading ? (
              <div className="flex flex-col gap-1.5" aria-hidden>
                <Skeleton className="h-7 w-full" />
                <Skeleton className="h-7 w-2/3" />
              </div>
            ) : shares.length === 0 ? (
              <p className="text-sm italic text-fg-muted">
                Not shared with anyone yet.
              </p>
            ) : (
              <ul className="m-0 flex list-none flex-col p-0">
                {shares.map((s) => (
                  <li
                    key={s.id}
                    className="flex items-center justify-between gap-3 border-b border-border py-2 text-sm last:border-0"
                  >
                    <span className="min-w-0 truncate text-fg">
                      <strong className="font-medium">
                        {humanizeToken(s.grantee_type)}
                      </strong>
                      : {s.grantee}{" "}
                      <span className="text-fg-muted">
                        ({PERMISSION_LABEL[s.permission]})
                      </span>
                    </span>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-danger hover:text-danger"
                      onClick={() => deleteMut.mutate(s.id)}
                      disabled={deleteMut.isPending}
                    >
                      Revoke
                    </Button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
