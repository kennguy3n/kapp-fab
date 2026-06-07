import { useEffect, useMemo, useState } from "react";
import {
  Button,
  ConfirmDialog,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";

/**
 * RoleManagementPage is the tenant-admin surface for the per-tenant
 * role graph. It lists every role defined for the active tenant,
 * lets an operator create / edit / delete custom roles, and
 * exposes the per-role permissions list (the granular grants stored
 * in the `permissions` table — `roles.permissions` JSONB stays the
 * coarse-grained role pack).
 *
 * The backend lives at /api/v1/roles and is gated behind
 * `authz.Middleware(authzEval, "tenant.admin", "")`, so an actor
 * without the `tenant.admin` permission gets a 403 before any of
 * these requests reach the database.
 */

interface Role {
  name: string;
  description?: string;
  permissions: unknown;
  parent_role?: string;
}

interface PermissionRow {
  id: string;
  role_name: string;
  ktype: string;
  action: string;
  conditions?: unknown;
  granted_at?: string;
}

const HEADER_NAMES = (): HeadersInit => {
  const tenant = localStorage.getItem("kapp.tenant") ?? "";
  const token = localStorage.getItem("kapp.token");
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Tenant-ID": tenant,
  };
  if (token) {
    headers.Authorization = `Bearer ${token}`;
  }
  return headers;
};

async function jsonFetch<T>(url: string, init?: RequestInit): Promise<T> {
  const res = await fetch(url, { ...init, headers: HEADER_NAMES() });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || `${res.status} ${res.statusText}`);
  }
  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

export function RoleManagementPage() {
  const [roles, setRoles] = useState<Role[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [permissions, setPermissions] = useState<PermissionRow[]>([]);
  const [newRoleName, setNewRoleName] = useState("");
  const [newRoleParent, setNewRoleParent] = useState("");
  const [newRolePerms, setNewRolePerms] = useState("[]");
  const [newPermAction, setNewPermAction] = useState("");
  const [newPermKType, setNewPermKType] = useState("");
  const [newPermConditions, setNewPermConditions] = useState("{}");
  const [deleteName, setDeleteName] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  const loadRoles = () => {
    setLoading(true);
    jsonFetch<Role[]>("/api/v1/roles")
      .then((data) => setRoles(data ?? []))
      .catch((err) => setError(String(err.message ?? err)))
      .finally(() => setLoading(false));
  };

  const loadPermissions = (name: string) => {
    jsonFetch<PermissionRow[]>(
      `/api/v1/roles/${encodeURIComponent(name)}/permissions`,
    )
      .then((rows) => setPermissions(rows ?? []))
      .catch((err) => setError(String(err.message ?? err)));
  };

  useEffect(() => {
    loadRoles();
  }, []);

  useEffect(() => {
    if (selected) {
      loadPermissions(selected);
    } else {
      setPermissions([]);
    }
  }, [selected]);

  const sortedRoles = useMemo(
    () => [...roles].sort((a, b) => a.name.localeCompare(b.name)),
    [roles],
  );

  const createRole = async () => {
    setError(null);
    try {
      const perms = JSON.parse(newRolePerms);
      await jsonFetch("/api/v1/roles", {
        method: "POST",
        body: JSON.stringify({
          name: newRoleName,
          permissions: perms,
          parent_role: newRoleParent || undefined,
        }),
      });
      setNewRoleName("");
      setNewRoleParent("");
      setNewRolePerms("[]");
      loadRoles();
    } catch (err) {
      setError(String((err as Error).message ?? err));
    }
  };

  // Keep the confirm dialog open (showing its `loading` state) until
  // the delete settles, then close it — matching the await-mutation
  // pattern used elsewhere so destructive actions give consistent
  // "Working…" feedback. This page uses imperative fetch rather than
  // React Query, so a local `deleting` flag stands in for isPending.
  const deleteRole = async (name: string) => {
    setDeleting(true);
    try {
      await jsonFetch(`/api/v1/roles/${encodeURIComponent(name)}`, {
        method: "DELETE",
      });
      if (selected === name) setSelected(null);
      loadRoles();
    } catch (err) {
      setError(String((err as Error).message ?? err));
    } finally {
      setDeleting(false);
      setDeleteName(null);
    }
  };

  const grantPermission = async () => {
    if (!selected) return;
    setError(null);
    try {
      const cond = newPermConditions ? JSON.parse(newPermConditions) : {};
      await jsonFetch(
        `/api/v1/roles/${encodeURIComponent(selected)}/permissions`,
        {
          method: "POST",
          body: JSON.stringify({
            action: newPermAction,
            ktype: newPermKType,
            conditions: cond,
          }),
        },
      );
      setNewPermAction("");
      setNewPermKType("");
      setNewPermConditions("{}");
      loadPermissions(selected);
    } catch (err) {
      setError(String((err as Error).message ?? err));
    }
  };

  const revokePermission = async (id: string) => {
    if (!selected) return;
    try {
      await jsonFetch(
        `/api/v1/roles/${encodeURIComponent(selected)}/permissions/${encodeURIComponent(id)}`,
        { method: "DELETE" },
      );
      loadPermissions(selected);
    } catch (err) {
      setError(String((err as Error).message ?? err));
    }
  };

  return (
    <section>
      <h1>Role Management</h1>
      <p className="text-fg-muted">
        Manage tenant-scoped roles, their permission grants, and the
        parent-role hierarchy. Mutations require the{" "}
        <code>tenant.admin</code> permission and invalidate the
        authorization cache so changes take effect on the next
        request.
      </p>
      {error && (
        <div className="my-2 rounded border border-danger/30 bg-danger/10 p-2 text-danger">
          {error}
        </div>
      )}

      <div className="flex items-start gap-6">
        <div className="flex-1">
          <h2>Roles</h2>
          {loading && <p>Loading…</p>}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Parent</TableHead>
                <TableHead>Permissions</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {sortedRoles.map((r) => (
                <TableRow
                  key={r.name}
                  className={selected === r.name ? "bg-bg-muted" : undefined}
                >
                  <TableCell
                    className="cursor-pointer"
                    onClick={() => setSelected(r.name)}
                  >
                    {r.name}
                  </TableCell>
                  <TableCell>{r.parent_role ?? ""}</TableCell>
                  <TableCell>
                    <code className="text-xs">
                      {JSON.stringify(r.permissions)}
                    </code>
                  </TableCell>
                  <TableCell>
                    {r.name !== "owner" && (
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => setDeleteName(r.name)}
                      >
                        Delete
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          <h3 className="mt-6">Create role</h3>
          <div className="grid max-w-[400px] gap-2">
            <Input
              placeholder="role.name"
              value={newRoleName}
              onChange={(e) => setNewRoleName(e.target.value)}
            />
            <Input
              placeholder='Parent role (e.g. "tenant.member")'
              value={newRoleParent}
              onChange={(e) => setNewRoleParent(e.target.value)}
            />
            <textarea
              placeholder='Permissions JSON (e.g. ["finance.*"])'
              value={newRolePerms}
              onChange={(e) => setNewRolePerms(e.target.value)}
              rows={3}
              className="w-full rounded-md border border-border bg-bg-elevated p-1.5 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
            />
            <div>
              <Button onClick={createRole} disabled={!newRoleName}>
                Create
              </Button>
            </div>
          </div>
        </div>

        <div className="flex-1">
          <h2>{selected ? `Permissions: ${selected}` : "Select a role"}</h2>
          {selected && (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Action</TableHead>
                    <TableHead>KType</TableHead>
                    <TableHead>Conditions</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {permissions.map((p) => (
                    <TableRow key={p.id}>
                      <TableCell>{p.action}</TableCell>
                      <TableCell>{p.ktype}</TableCell>
                      <TableCell>
                        <code className="text-xs">
                          {JSON.stringify(p.conditions)}
                        </code>
                      </TableCell>
                      <TableCell>
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => revokePermission(p.id)}
                        >
                          Revoke
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>

              <h3 className="mt-6">Grant permission</h3>
              <div className="grid max-w-[400px] gap-2">
                <Input
                  placeholder="action (e.g. finance.invoice.write)"
                  value={newPermAction}
                  onChange={(e) => setNewPermAction(e.target.value)}
                />
                <Input
                  placeholder="ktype (optional)"
                  value={newPermKType}
                  onChange={(e) => setNewPermKType(e.target.value)}
                />
                <textarea
                  placeholder='Conditions JSON (e.g. {"owner_only":true})'
                  value={newPermConditions}
                  onChange={(e) => setNewPermConditions(e.target.value)}
                  rows={3}
                  className="w-full rounded-md border border-border bg-bg-elevated p-1.5 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
                />
                <div>
                  <Button onClick={grantPermission} disabled={!newPermAction}>
                    Grant
                  </Button>
                </div>
              </div>
            </>
          )}
        </div>
      </div>

      <ConfirmDialog
        open={deleteName !== null}
        onOpenChange={(o) => {
          if (!o && !deleting) setDeleteName(null);
        }}
        title={deleteName ? `Delete role "${deleteName}"?` : "Delete role?"}
        description="This removes the role and its permission grants. Users assigned to it lose the associated access."
        confirmLabel="Delete"
        destructive
        loading={deleting}
        onConfirm={() => {
          if (deleteName) deleteRole(deleteName);
        }}
      />
    </section>
  );
}
