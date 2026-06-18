import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Building2, Plus, Search } from "lucide-react";
import {
  Badge,
  Button,
  DataGrid,
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
  toast,
  type BadgeProps,
  type DataGridColumn,
} from "@kapp/ui";
import type { CreateTenantInput, Plan, Tenant } from "@kapp/client";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken } from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
  CopyableId,
} from "./adminKit";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

const STATUS_VARIANT: Record<Tenant["status"], BadgeVariant> = {
  active: "success",
  suspended: "warning",
  archived: "neutral",
  deleting: "danger",
};

const TABLE_COLUMNS = ["Workspace", "Plan", "Status", "Created", "ID"];

function slugify(value: string): string {
  return value
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

export function TenantListPage() {
  const queryClient = useQueryClient();
  const fmt = useFormatter();
  const [search, setSearch] = useState("");
  const [createOpen, setCreateOpen] = useState(false);

  const tenantsQuery = useQuery({
    queryKey: ["tenants"],
    queryFn: () => api.listTenants(),
  });

  const tenants = useMemo(() => tenantsQuery.data ?? [], [tenantsQuery.data]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return tenants;
    return tenants.filter((t) =>
      [t.name, t.slug, t.plan, t.status].some((v) =>
        v.toLowerCase().includes(q),
      ),
    );
  }, [tenants, search]);

  const columns: DataGridColumn<Tenant>[] = [
    {
      key: "workspace",
      header: "Workspace",
      sortable: true,
      accessor: (t) => t.name.toLowerCase(),
      cell: (t) => (
        <div className="flex flex-col">
          <span className="font-medium text-fg">{t.name}</span>
          <span className="text-xs text-fg-muted">{t.slug}</span>
        </div>
      ),
    },
    {
      key: "plan",
      header: "Plan",
      sortable: true,
      accessor: (t) => t.plan.toLowerCase(),
      cell: (t) => <Badge variant="outline">{humanizeToken(t.plan)}</Badge>,
    },
    {
      key: "status",
      header: "Status",
      sortable: true,
      accessor: (t) => t.status,
      cell: (t) => (
        <Badge variant={STATUS_VARIANT[t.status] ?? "neutral"}>
          {humanizeToken(t.status)}
        </Badge>
      ),
    },
    {
      key: "created",
      header: "Created",
      sortable: true,
      accessor: (t) => new Date(t.created_at),
      cell: (t) => (
        <span className="text-fg-muted">{fmt.date(new Date(t.created_at))}</span>
      ),
    },
    {
      key: "id",
      header: "ID",
      cell: (t) => <CopyableId value={t.id} label="workspace id" />,
      headerClassName: "text-end",
      className: "text-end",
    },
  ];

  const actions = (
    <>
      <Badge variant="neutral" size="md">
        {tenants.length} {tenants.length === 1 ? "workspace" : "workspaces"}
      </Badge>
      <Button leadingIcon={<Plus />} onClick={() => setCreateOpen(true)}>
        New workspace
      </Button>
    </>
  );

  let body: React.ReactNode;
  if (tenantsQuery.isLoading) {
    body = <AdminTableSkeleton columns={TABLE_COLUMNS} />;
  } else if (tenantsQuery.error) {
    body = (
      <AdminErrorState
        title="Couldn't load workspaces"
        error={tenantsQuery.error}
        onRetry={() => tenantsQuery.refetch()}
      />
    );
  } else if (tenants.length === 0) {
    body = (
      <EmptyState
        icon={<Building2 />}
        title="No workspaces yet"
        description="No tenants registered yet. Create a workspace to onboard a customer and configure their plan, features, and data."
        action={
          <Button leadingIcon={<Plus />} onClick={() => setCreateOpen(true)}>
            New workspace
          </Button>
        }
      />
    );
  } else {
    body = (
      <div className="flex flex-col gap-3">
        <div className="max-w-xs">
          <Input
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search workspaces…"
            aria-label="Search workspaces"
            leadingAddon={<Search className="h-4 w-4" />}
          />
        </div>
        <DataGrid
          data={filtered}
          columns={columns}
          rowKey={(t) => t.id}
          pageSize={10}
          emptyState={`No workspaces match “${search}”.`}
        />
      </div>
    );
  }

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Workspaces"
        description="Every customer tenant on the platform. Manage plans, features, and data residency from here."
        actions={actions}
      />
      {body}
      <CreateTenantModal
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={() => {
          void queryClient.invalidateQueries({ queryKey: ["tenants"] });
        }}
      />
    </section>
  );
}

function CreateTenantModal({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const [cell, setCell] = useState("");
  const [plan, setPlan] = useState("");
  const [submitted, setSubmitted] = useState(false);

  const plansQuery = useQuery({
    queryKey: ["plans"],
    queryFn: () => api.listPlans(),
    enabled: open,
  });
  const plans: Plan[] = plansQuery.data?.plans ?? [];

  const effectiveSlug = slugTouched ? slug : slugify(name);

  const createMutation = useMutation({
    mutationFn: (input: CreateTenantInput) => api.createTenant(input),
    onSuccess: (tenant) => {
      toast.success(`Workspace “${tenant.name}” created`);
      onCreated();
      close();
    },
    onError: (err) => {
      toast.error(
        err instanceof Error ? err.message : "Couldn't create the workspace",
      );
    },
  });

  function close() {
    onOpenChange(false);
    setName("");
    setSlug("");
    setSlugTouched(false);
    setCell("");
    setPlan("");
    setSubmitted(false);
  }

  const nameError = submitted && !name.trim() ? "Name is required" : undefined;
  const slugError =
    submitted && !effectiveSlug
      ? "URL slug is required"
      : submitted && !/^[a-z0-9-]+$/.test(effectiveSlug)
        ? "Use lowercase letters, numbers, and hyphens only"
        : undefined;
  const cellError = submitted && !cell.trim() ? "Cell is required" : undefined;
  const planError = submitted && !plan ? "Choose a plan" : undefined;

  function submit() {
    setSubmitted(true);
    if (
      !name.trim() ||
      !effectiveSlug ||
      !/^[a-z0-9-]+$/.test(effectiveSlug) ||
      !cell.trim() ||
      !plan
    ) {
      return;
    }
    createMutation.mutate({
      name: name.trim(),
      slug: effectiveSlug,
      cell: cell.trim(),
      plan,
    });
  }

  return (
    <Modal open={open} onOpenChange={(next) => (next ? onOpenChange(true) : close())}>
      <ModalContent>
        <ModalHeader>
          <ModalTitle>New workspace</ModalTitle>
          <ModalDescription>
            Onboard a customer tenant. You can adjust their plan and features
            afterwards.
          </ModalDescription>
        </ModalHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            submit();
          }}
        >
          <Field label="Workspace name" required error={nameError}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Acme Inc"
              autoFocus
            />
          </Field>
          <Field
            label="URL slug"
            required
            error={slugError}
            help="Used in URLs and API calls. Lowercase letters, numbers, and hyphens."
          >
            <Input
              value={effectiveSlug}
              onChange={(e) => {
                setSlugTouched(true);
                setSlug(e.target.value);
              }}
              placeholder="acme-inc"
            />
          </Field>
          <Field
            label="Plan"
            required
            error={planError}
            help={plansQuery.isLoading ? "Loading plans…" : undefined}
          >
            <Select value={plan} onChange={(e) => setPlan(e.target.value)}>
              <option value="" disabled>
                Select a plan…
              </option>
              {plans.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.display_name}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Cell"
            required
            error={cellError}
            help="The infrastructure cell this workspace is placed in."
          >
            <Input
              value={cell}
              onChange={(e) => setCell(e.target.value)}
              placeholder="cell-local"
            />
          </Field>
          <ModalFooter>
            <Button type="button" variant="outline" onClick={close}>
              Cancel
            </Button>
            <Button type="submit" disabled={createMutation.isPending}>
              {createMutation.isPending ? "Creating…" : "Create workspace"}
            </Button>
          </ModalFooter>
        </form>
      </ModalContent>
    </Modal>
  );
}
