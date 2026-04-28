import { useEffect, useMemo, useState } from "react";

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

  const deleteRole = async (name: string) => {
    if (!confirm(`Delete role "${name}"?`)) return;
    try {
      await jsonFetch(`/api/v1/roles/${encodeURIComponent(name)}`, {
        method: "DELETE",
      });
      if (selected === name) setSelected(null);
      loadRoles();
    } catch (err) {
      setError(String((err as Error).message ?? err));
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
      <p style={{ color: "#6b7280" }}>
        Manage tenant-scoped roles, their permission grants, and the
        parent-role hierarchy. Mutations require the{" "}
        <code>tenant.admin</code> permission and invalidate the
        authorization cache so changes take effect on the next
        request.
      </p>
      {error && (
        <div
          style={{
            color: "#b91c1c",
            background: "#fee2e2",
            padding: 8,
            borderRadius: 4,
            margin: "8px 0",
          }}
        >
          {error}
        </div>
      )}

      <div style={{ display: "flex", gap: 24, alignItems: "flex-start" }}>
        <div style={{ flex: 1 }}>
          <h2>Roles</h2>
          {loading && <p>Loading…</p>}
          <table style={{ width: "100%", borderCollapse: "collapse" }}>
            <thead>
              <tr style={{ textAlign: "left" }}>
                <th>Name</th>
                <th>Parent</th>
                <th>Permissions</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {sortedRoles.map((r) => (
                <tr
                  key={r.name}
                  style={{
                    background: selected === r.name ? "#eef2ff" : undefined,
                  }}
                >
                  <td
                    style={{ cursor: "pointer", padding: "4px 8px" }}
                    onClick={() => setSelected(r.name)}
                  >
                    {r.name}
                  </td>
                  <td>{r.parent_role ?? ""}</td>
                  <td>
                    <code style={{ fontSize: 12 }}>
                      {JSON.stringify(r.permissions)}
                    </code>
                  </td>
                  <td>
                    {r.name !== "owner" && (
                      <button onClick={() => deleteRole(r.name)}>Delete</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          <h3 style={{ marginTop: 24 }}>Create role</h3>
          <div style={{ display: "grid", gap: 8, maxWidth: 400 }}>
            <input
              placeholder="role.name"
              value={newRoleName}
              onChange={(e) => setNewRoleName(e.target.value)}
            />
            <input
              placeholder='Parent role (e.g. "tenant.member")'
              value={newRoleParent}
              onChange={(e) => setNewRoleParent(e.target.value)}
            />
            <textarea
              placeholder='Permissions JSON (e.g. ["finance.*"])'
              value={newRolePerms}
              onChange={(e) => setNewRolePerms(e.target.value)}
              rows={3}
            />
            <button onClick={createRole} disabled={!newRoleName}>
              Create
            </button>
          </div>
        </div>

        <div style={{ flex: 1 }}>
          <h2>{selected ? `Permissions: ${selected}` : "Select a role"}</h2>
          {selected && (
            <>
              <table style={{ width: "100%", borderCollapse: "collapse" }}>
                <thead>
                  <tr style={{ textAlign: "left" }}>
                    <th>Action</th>
                    <th>KType</th>
                    <th>Conditions</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {permissions.map((p) => (
                    <tr key={p.id}>
                      <td>{p.action}</td>
                      <td>{p.ktype}</td>
                      <td>
                        <code style={{ fontSize: 12 }}>
                          {JSON.stringify(p.conditions)}
                        </code>
                      </td>
                      <td>
                        <button onClick={() => revokePermission(p.id)}>
                          Revoke
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>

              <h3 style={{ marginTop: 24 }}>Grant permission</h3>
              <div style={{ display: "grid", gap: 8, maxWidth: 400 }}>
                <input
                  placeholder="action (e.g. finance.invoice.write)"
                  value={newPermAction}
                  onChange={(e) => setNewPermAction(e.target.value)}
                />
                <input
                  placeholder="ktype (optional)"
                  value={newPermKType}
                  onChange={(e) => setNewPermKType(e.target.value)}
                />
                <textarea
                  placeholder='Conditions JSON (e.g. {"owner_only":true})'
                  value={newPermConditions}
                  onChange={(e) => setNewPermConditions(e.target.value)}
                  rows={3}
                />
                <button onClick={grantPermission} disabled={!newPermAction}>
                  Grant
                </button>
              </div>
            </>
          )}
        </div>
      </div>
    </section>
  );
}
