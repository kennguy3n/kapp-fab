import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Plus, ShieldCheck, Trash2 } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  cn,
  ConfirmDialog,
  EmptyState,
  Field,
  Input,
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  Select,
  Skeleton,
  Spinner,
  toast,
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@kapp/ui";
import { api } from "../lib/api";
import { humanizeToken, ktypeSingular } from "../lib/ktypeView";
import { AdminErrorState, AdminPageHeader } from "./adminKit";

/**
 * RoleManagementPage is the tenant-admin surface for the per-tenant
 * role graph. It lists every role defined for the active tenant and
 * presents the granular grants stored in the `permissions` table as a
 * readable permission MATRIX (entities × actions) rather than raw
 * JSON. Toggling a cell grants or revokes a single permission.
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

/** Canonical CRUD actions every entity can be granted, shown first. */
const BASE_ACTIONS = ["read", "create", "update", "delete"] as const;

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

// In demo mode the mock layer installs a window.fetch shim that serves
// the /roles routes from in-memory fixtures. api.ts installs it on boot,
// but ensure the (idempotent) shim is in place before the first request
// so a cold page load can't race the install and 500 through the proxy.
const demoMode = import.meta.env.VITE_DEMO_MODE === "true";
async function ensureDemoFetch(): Promise<void> {
  if (!demoMode) return;
  const { installPortalDemoFetch } = await import("../lib/mock-api");
  installPortalDemoFetch();
}

async function jsonFetch<T>(url: string, init?: RequestInit): Promise<T> {
  await ensureDemoFetch();
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

/** Title-case a dotted/underscored identifier (role or action name). */
function prettify(token: string): string {
  return token
    .split(/[._]/)
    .filter(Boolean)
    .map((part) => humanizeToken(part))
    .join(" ");
}

function rolePacks(role: Role | undefined): string[] {
  if (!role || !Array.isArray(role.permissions)) return [];
  return role.permissions.filter((p): p is string => typeof p === "string");
}

function hasConditions(row: PermissionRow): boolean {
  const c = row.conditions;
  if (c == null) return false;
  if (typeof c === "object" && Object.keys(c as object).length === 0)
    return false;
  return true;
}

export function RoleManagementPage() {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string | null>(null);
  const [extraActions, setExtraActions] = useState<string[]>([]);
  const [newAction, setNewAction] = useState("");
  const [pendingCell, setPendingCell] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [deleteName, setDeleteName] = useState<string | null>(null);

  const rolesQuery = useQuery({
    queryKey: ["roles"],
    queryFn: () => jsonFetch<Role[]>("/api/v1/roles"),
  });

  const ktypesQuery = useQuery({
    queryKey: ["ktypes"],
    queryFn: () => api.listKTypes(),
    staleTime: 60_000,
  });

  const roles = useMemo(
    () => [...(rolesQuery.data ?? [])].sort((a, b) => a.name.localeCompare(b.name)),
    [rolesQuery.data],
  );

  // Default the selection to the first role once data arrives.
  const activeRole = selected ?? roles[0]?.name ?? null;
  const selectedRole = roles.find((r) => r.name === activeRole);

  const permissionsQuery = useQuery({
    queryKey: ["role-permissions", activeRole],
    queryFn: () =>
      jsonFetch<PermissionRow[]>(
        `/api/v1/roles/${encodeURIComponent(activeRole as string)}/permissions`,
      ),
    enabled: activeRole !== null,
  });

  const permIndex = useMemo(() => {
    const map = new Map<string, PermissionRow>();
    for (const row of permissionsQuery.data ?? []) {
      map.set(`${row.ktype}|${row.action}`, row);
    }
    return map;
  }, [permissionsQuery.data]);

  // Columns: canonical CRUD actions, then any extra actions that
  // already exist in this role's grants or were added in this session.
  const actionColumns = useMemo(() => {
    const set = new Set<string>(BASE_ACTIONS);
    for (const row of permissionsQuery.data ?? []) set.add(row.action);
    for (const a of extraActions) set.add(a);
    const base = BASE_ACTIONS.filter((a) => set.has(a));
    const extra = [...set]
      .filter((a) => !BASE_ACTIONS.includes(a as (typeof BASE_ACTIONS)[number]))
      .sort((a, b) => a.localeCompare(b));
    return [...base, ...extra];
  }, [permissionsQuery.data, extraActions]);

  // Rows: a wildcard "All record types" row, then every KType in the
  // registry, plus any ktype referenced by an existing grant.
  const entityRows = useMemo(() => {
    const names = new Set<string>();
    for (const k of ktypesQuery.data ?? []) names.add(k.name);
    for (const row of permissionsQuery.data ?? [])
      if (row.ktype) names.add(row.ktype);
    const list = [...names]
      .map((name) => ({ value: name, label: ktypeSingular(name) }))
      .sort((a, b) => a.label.localeCompare(b.label));
    return [{ value: "", label: "All record types" }, ...list];
  }, [ktypesQuery.data, permissionsQuery.data]);

  const grantMutation = useMutation({
    mutationFn: (input: { ktype: string; action: string }) =>
      jsonFetch<PermissionRow>(
        `/api/v1/roles/${encodeURIComponent(activeRole as string)}/permissions`,
        {
          method: "POST",
          body: JSON.stringify({
            action: input.action,
            ktype: input.ktype,
            conditions: {},
          }),
        },
      ),
    onError: (err: Error) => toast.error(err.message),
    onSettled: () => {
      setPendingCell(null);
      queryClient.invalidateQueries({
        queryKey: ["role-permissions", activeRole],
      });
    },
  });

  const revokeMutation = useMutation({
    mutationFn: (id: string) =>
      jsonFetch<void>(
        `/api/v1/roles/${encodeURIComponent(activeRole as string)}/permissions/${encodeURIComponent(id)}`,
        { method: "DELETE" },
      ),
    onError: (err: Error) => toast.error(err.message),
    onSettled: () => {
      setPendingCell(null);
      queryClient.invalidateQueries({
        queryKey: ["role-permissions", activeRole],
      });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (name: string) =>
      jsonFetch<void>(`/api/v1/roles/${encodeURIComponent(name)}`, {
        method: "DELETE",
      }),
    onSuccess: (_data, name) => {
      toast.success(`Deleted the ${prettify(name)} role`);
      if (activeRole === name) setSelected(null);
      queryClient.invalidateQueries({ queryKey: ["roles"] });
    },
    onError: (err: Error) => toast.error(err.message),
    onSettled: () => setDeleteName(null),
  });

  function toggleCell(ktype: string, action: string) {
    if (!activeRole) return;
    const key = `${ktype}|${action}`;
    const existing = permIndex.get(key);
    setPendingCell(key);
    if (existing) {
      revokeMutation.mutate(existing.id);
    } else {
      grantMutation.mutate({ ktype, action });
    }
  }

  function addActionColumn() {
    const value = newAction.trim().toLowerCase().replace(/\s+/g, "_");
    if (!value) return;
    setExtraActions((prev) => (prev.includes(value) ? prev : [...prev, value]));
    setNewAction("");
  }

  if (rolesQuery.isLoading) {
    return (
      <section className="flex flex-col gap-6">
        <AdminPageHeader area="Platform" title="Roles & permissions" />
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[18rem_1fr]">
          <Skeleton variant="rect" className="h-64" />
          <Skeleton variant="rect" className="h-64" />
        </div>
      </section>
    );
  }

  if (rolesQuery.isError) {
    return (
      <section className="flex flex-col gap-6">
        <AdminPageHeader area="Platform" title="Roles & permissions" />
        <AdminErrorState
          title="Couldn't load roles"
          error={rolesQuery.error}
          onRetry={() => rolesQuery.refetch()}
        />
      </section>
    );
  }

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Roles & permissions"
        description="Define who can do what in this workspace. Pick a role to see exactly which actions it allows on each record type, and toggle access on or off."
        actions={
          <Button leadingIcon={<Plus />} onClick={() => setCreateOpen(true)}>
            New role
          </Button>
        }
      />

      {roles.length === 0 ? (
        <EmptyState
          icon={<ShieldCheck />}
          title="No roles defined yet"
          description="Create a role to start granting access to the people and agents in your workspace."
          action={
            <Button leadingIcon={<Plus />} onClick={() => setCreateOpen(true)}>
              New role
            </Button>
          }
        />
      ) : (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[18rem_1fr]">
          <nav aria-label="Roles" className="flex flex-col gap-2">
            {roles.map((role) => {
              const isActive = role.name === activeRole;
              const packs = rolePacks(role);
              return (
                <button
                  key={role.name}
                  type="button"
                  onClick={() => setSelected(role.name)}
                  aria-current={isActive ? "true" : undefined}
                  className={cn(
                    "flex flex-col gap-1 rounded-lg border p-3 text-start transition-colors",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                    isActive
                      ? "border-accent bg-accent/5"
                      : "border-border bg-bg-elevated hover:bg-bg-subtle",
                  )}
                >
                  <span className="flex items-center justify-between gap-2">
                    <span className="truncate font-medium text-fg">
                      {prettify(role.name)}
                    </span>
                    {role.name === "owner" && (
                      <Badge variant="neutral" size="xs">
                        Built-in
                      </Badge>
                    )}
                  </span>
                  {role.parent_role && (
                    <span className="text-xs text-fg-muted">
                      Inherits from {prettify(role.parent_role)}
                    </span>
                  )}
                  {packs.length > 0 && (
                    <span className="flex flex-wrap gap-1">
                      {packs.map((p) => (
                        <Badge key={p} variant="outline" size="xs">
                          {p}
                        </Badge>
                      ))}
                    </span>
                  )}
                </button>
              );
            })}
          </nav>

          <div className="min-w-0">
            {selectedRole && (
              <Card>
                <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3">
                  <div className="min-w-0">
                    <CardTitle>{prettify(selectedRole.name)}</CardTitle>
                    <p className="mt-0.5 text-sm text-fg-muted">
                      {permIndex.size}{" "}
                      {permIndex.size === 1 ? "permission" : "permissions"}{" "}
                      granted
                    </p>
                  </div>
                  {selectedRole.name !== "owner" && (
                    <Button
                      variant="outline"
                      size="sm"
                      leadingIcon={<Trash2 />}
                      onClick={() => setDeleteName(selectedRole.name)}
                    >
                      Delete role
                    </Button>
                  )}
                </CardHeader>
                <CardContent className="flex flex-col gap-3">
                  {permissionsQuery.isLoading ? (
                    <Skeleton variant="rect" className="h-48" />
                  ) : permissionsQuery.isError ? (
                    <AdminErrorState
                      title="Couldn't load permissions"
                      error={permissionsQuery.error}
                      onRetry={() => permissionsQuery.refetch()}
                    />
                  ) : (
                    <PermissionMatrix
                      entityRows={entityRows}
                      actionColumns={actionColumns}
                      permIndex={permIndex}
                      pendingCell={pendingCell}
                      onToggle={toggleCell}
                    />
                  )}

                  <div className="flex flex-wrap items-end gap-2 border-t border-border pt-3">
                    <Field label="Add an action" className="w-56">
                      <Input
                        value={newAction}
                        onChange={(e) => setNewAction(e.target.value)}
                        placeholder="e.g. approve, export"
                        onKeyDown={(e) => {
                          if (e.key === "Enter") {
                            e.preventDefault();
                            addActionColumn();
                          }
                        }}
                      />
                    </Field>
                    <Button
                      variant="secondary"
                      onClick={addActionColumn}
                      disabled={!newAction.trim()}
                    >
                      Add column
                    </Button>
                  </div>
                </CardContent>
              </Card>
            )}
          </div>
        </div>
      )}

      <CreateRoleModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        roles={roles}
        onCreated={(name) => {
          setSelected(name);
          queryClient.invalidateQueries({ queryKey: ["roles"] });
        }}
      />

      <ConfirmDialog
        open={deleteName !== null}
        onOpenChange={(o) => {
          if (!o && !deleteMutation.isPending) setDeleteName(null);
        }}
        title={
          deleteName ? `Delete the ${prettify(deleteName)} role?` : "Delete role?"
        }
        description="This removes the role and its permission grants. Anyone assigned to it loses the associated access."
        confirmLabel="Delete role"
        destructive
        loading={deleteMutation.isPending}
        onConfirm={() => {
          if (deleteName) deleteMutation.mutate(deleteName);
        }}
      />
    </section>
  );
}

function PermissionMatrix({
  entityRows,
  actionColumns,
  permIndex,
  pendingCell,
  onToggle,
}: {
  entityRows: { value: string; label: string }[];
  actionColumns: string[];
  permIndex: Map<string, PermissionRow>;
  pendingCell: string | null;
  onToggle: (ktype: string, action: string) => void;
}) {
  return (
    <div className="w-full overflow-x-auto">
      <table className="w-full caption-bottom border-separate border-spacing-0 text-sm">
        <thead>
          <tr>
            <th className="sticky left-0 z-10 bg-bg-subtle px-3 py-2 text-start text-xs font-medium text-fg-muted">
              Record type
            </th>
            {actionColumns.map((action) => (
              <th
                key={action}
                scope="col"
                className="bg-bg-subtle px-3 py-2 text-center text-xs font-medium text-fg-muted"
              >
                {prettify(action)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {entityRows.map((row) => (
            <tr key={row.value || "__all__"} className="hover:bg-bg-subtle">
              <th
                scope="row"
                className="sticky left-0 z-10 bg-bg-elevated px-3 py-2 text-start font-medium text-fg"
              >
                {row.label}
              </th>
              {actionColumns.map((action) => {
                const key = `${row.value}|${action}`;
                const grant = permIndex.get(key);
                return (
                  <td key={action} className="px-3 py-2 text-center">
                    <MatrixCell
                      checked={Boolean(grant)}
                      pending={pendingCell === key}
                      conditional={grant ? hasConditions(grant) : false}
                      label={`${prettify(action)} on ${row.label}`}
                      onToggle={() => onToggle(row.value, action)}
                    />
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function MatrixCell({
  checked,
  pending,
  conditional,
  label,
  onToggle,
}: {
  checked: boolean;
  pending: boolean;
  conditional: boolean;
  label: string;
  onToggle: () => void;
}) {
  const button = (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={pending}
      onClick={onToggle}
      className={cn(
        "relative inline-flex h-6 w-6 items-center justify-center rounded-md border transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
        "disabled:cursor-not-allowed disabled:opacity-60",
        checked
          ? "border-accent bg-accent text-accent-fg"
          : "border-border bg-bg-elevated text-transparent hover:border-border-strong",
      )}
    >
      {pending ? (
        <Spinner size="xs" />
      ) : (
        <Check className="h-3.5 w-3.5" aria-hidden="true" />
      )}
      {conditional && !pending && (
        <span
          aria-hidden="true"
          className="absolute -right-0.5 -top-0.5 h-2 w-2 rounded-full bg-warning"
        />
      )}
    </button>
  );

  if (!conditional) return button;
  return (
    <Tooltip>
      <TooltipTrigger asChild>{button}</TooltipTrigger>
      <TooltipContent>Granted with conditions</TooltipContent>
    </Tooltip>
  );
}

function CreateRoleModal({
  open,
  onClose,
  roles,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  roles: Role[];
  onCreated: (name: string) => void;
}) {
  const [name, setName] = useState("");
  const [parent, setParent] = useState("");
  const [packs, setPacks] = useState("");
  const [submitted, setSubmitted] = useState(false);

  const reset = () => {
    setName("");
    setParent("");
    setPacks("");
    setSubmitted(false);
  };

  const createMutation = useMutation({
    mutationFn: () =>
      jsonFetch<Role>("/api/v1/roles", {
        method: "POST",
        body: JSON.stringify({
          name: name.trim(),
          parent_role: parent || undefined,
          permissions: packs
            .split(",")
            .map((p) => p.trim())
            .filter(Boolean),
        }),
      }),
    onSuccess: () => {
      toast.success(`Created the ${prettify(name.trim())} role`);
      const created = name.trim();
      reset();
      onClose();
      onCreated(created);
    },
    onError: (err: Error) => toast.error(err.message),
  });

  const nameError =
    submitted && !name.trim() ? "Give the role a name." : undefined;

  return (
    <Modal
      open={open}
      onOpenChange={(next) => {
        if (!next && !createMutation.isPending) {
          reset();
          onClose();
        }
      }}
    >
      <ModalContent>
        <ModalHeader>
          <ModalTitle>New role</ModalTitle>
          <ModalDescription>
            Roles bundle permissions so you can assign access in one step.
            You can fine-tune exactly what each role allows afterwards.
          </ModalDescription>
        </ModalHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            setSubmitted(true);
            if (!name.trim()) return;
            createMutation.mutate();
          }}
        >
          <Field label="Role name" required error={nameError}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. Sales Manager"
              autoFocus
            />
          </Field>
          <Field
            label="Inherits from"
            help="Optional. The new role starts with everything the parent role can do."
          >
            <Select value={parent} onChange={(e) => setParent(e.target.value)}>
              <option value="">No parent</option>
              {roles.map((r) => (
                <option key={r.name} value={r.name}>
                  {prettify(r.name)}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Permission packs"
            help='Optional shortcuts, comma-separated. Use "*" wildcards, e.g. finance.*, crm.read.'
          >
            <Input
              value={packs}
              onChange={(e) => setPacks(e.target.value)}
              placeholder="finance.*, crm.read"
            />
          </Field>
          <ModalFooter>
            <Button
              type="button"
              variant="ghost"
              onClick={() => {
                reset();
                onClose();
              }}
              disabled={createMutation.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={createMutation.isPending}>
              {createMutation.isPending ? "Creating…" : "Create role"}
            </Button>
          </ModalFooter>
        </form>
      </ModalContent>
    </Modal>
  );
}
