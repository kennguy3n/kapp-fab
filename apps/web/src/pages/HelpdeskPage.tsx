import { useMemo, useState, type DragEvent, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { KRecord, SLAPolicy, UpsertSLAPolicyInput } from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
  toast,
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, Inbox, Plus } from "lucide-react";
import { api } from "../lib/api";
import { humanizeToken, recordLabel } from "../lib/ktypeView";

const TICKET_KTYPE = "helpdesk.ticket";
const EMPLOYEE_KTYPE = "hr.employee";
const ORG_KTYPE = "crm.organization";

interface TicketData {
  subject?: string;
  status?: string;
  priority?: string;
  channel?: string;
  customer_id?: string;
  assigned_to?: string;
  sla_resolution_by?: string;
}

const BOARD_COLUMNS: { status: string; label: string }[] = [
  { status: "open", label: "Open" },
  { status: "in_progress", label: "In progress" },
  { status: "waiting", label: "Waiting on customer" },
  { status: "resolved", label: "Resolved" },
];
const BOARD_STATUSES = new Set(BOARD_COLUMNS.map((c) => c.status));

const PRIORITY_VARIANT: Record<string, BadgeProps["variant"]> = {
  urgent: "danger",
  high: "warning",
  medium: "info",
  low: "neutral",
};
// Order priorities so the most urgent tickets sort to the top of a column.
const PRIORITY_RANK: Record<string, number> = {
  urgent: 0,
  high: 1,
  medium: 2,
  low: 3,
};

const FOUR_HOURS_MS = 4 * 60 * 60 * 1000;

function priorityVariant(priority: string): BadgeProps["variant"] {
  return PRIORITY_VARIANT[priority] ?? "neutral";
}

/** Human duration from a minute count: 45 → "45 min", 90 → "1h 30m". */
function formatDuration(minutes: number): string {
  if (!Number.isFinite(minutes) || minutes <= 0) return "—";
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const mins = minutes % 60;
  if (hours < 24) return mins ? `${hours}h ${mins}m` : `${hours}h`;
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return remHours ? `${days}d ${remHours}h` : `${days}d`;
}

interface SlaState {
  variant: BadgeProps["variant"];
  label: string;
}

/** Breach-risk badge from the resolution deadline relative to now. */
function slaState(dueIso: string | undefined): SlaState | null {
  if (!dueIso) return null;
  const due = new Date(dueIso);
  if (Number.isNaN(due.getTime())) return null;
  const diffMs = due.getTime() - Date.now();
  const gap = formatDuration(Math.round(Math.abs(diffMs) / 60000));
  if (diffMs < 0) return { variant: "danger", label: `Overdue ${gap}` };
  if (diffMs < FOUR_HOURS_MS) return { variant: "warning", label: `Due in ${gap}` };
  return { variant: "success", label: `Due in ${gap}` };
}

/**
 * HelpdeskPage is the tenant-wide service triage view: a board that
 * groups open tickets by status with breach-risk SLA countdowns, plus
 * SLA-policy management. Tickets can be re-triaged by dragging a card
 * between columns or — for keyboard users — via the per-card status
 * and assignee selects; both persist through `updateRecord` with an
 * optimistic cache update and rollback on failure.
 *
 * A "my queue" default would need the signed-in user's employee id,
 * which the web client doesn't currently expose, so triage is scoped
 * with an explicit assignee filter instead.
 */
export function HelpdeskPage() {
  const qc = useQueryClient();

  const ticketsQ = useQuery<KRecord[]>({
    queryKey: ["records", TICKET_KTYPE],
    queryFn: () => api.listRecords(TICKET_KTYPE),
  });
  const employeesQ = useQuery<KRecord[]>({
    queryKey: ["records", EMPLOYEE_KTYPE],
    queryFn: () => api.listRecords(EMPLOYEE_KTYPE),
    staleTime: 60_000,
  });
  const orgsQ = useQuery<KRecord[]>({
    queryKey: ["records", ORG_KTYPE],
    queryFn: () => api.listRecords(ORG_KTYPE),
    staleTime: 60_000,
  });
  const policiesQ = useQuery<{ policies: SLAPolicy[] }>({
    queryKey: ["helpdesk", "sla-policies"],
    queryFn: () => api.listSLAPolicies(),
  });

  const employees = useMemo(() => employeesQ.data ?? [], [employeesQ.data]);
  const employeeNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const e of employees) map.set(e.id, recordLabel(e));
    return map;
  }, [employees]);
  const orgNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const o of orgsQ.data ?? []) map.set(o.id, recordLabel(o));
    return map;
  }, [orgsQ.data]);

  const [assigneeFilter, setAssigneeFilter] = useState<string>("all");
  const [dragId, setDragId] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);

  const boardTickets = useMemo(() => {
    return (ticketsQ.data ?? []).filter((t) => {
      const d = t.data as TicketData;
      if (!d.status || !BOARD_STATUSES.has(d.status)) return false;
      if (assigneeFilter === "all") return true;
      if (assigneeFilter === "unassigned") return !d.assigned_to;
      return d.assigned_to === assigneeFilter;
    });
  }, [ticketsQ.data, assigneeFilter]);

  const ticketsByStatus = useMemo(() => {
    const map = new Map<string, KRecord[]>();
    for (const col of BOARD_COLUMNS) map.set(col.status, []);
    for (const t of boardTickets) {
      const status = (t.data as TicketData).status ?? "open";
      map.get(status)?.push(t);
    }
    for (const list of map.values()) {
      list.sort((a, b) => {
        const pa = PRIORITY_RANK[(a.data as TicketData).priority ?? ""] ?? 9;
        const pb = PRIORITY_RANK[(b.data as TicketData).priority ?? ""] ?? 9;
        if (pa !== pb) return pa - pb;
        const da = (a.data as TicketData).sla_resolution_by ?? "";
        const db = (b.data as TicketData).sla_resolution_by ?? "";
        return da.localeCompare(db);
      });
    }
    return map;
  }, [boardTickets]);

  async function patchTicket(
    id: string,
    patch: Partial<TicketData>,
    successMsg?: string,
  ) {
    const key = ["records", TICKET_KTYPE];
    const prev = qc.getQueryData<KRecord[]>(key);
    qc.setQueryData<KRecord[]>(key, (old) =>
      old?.map((r) => (r.id === id ? { ...r, data: { ...r.data, ...patch } } : r)) ??
      old,
    );
    try {
      const saved = await api.updateRecord(TICKET_KTYPE, id, patch);
      qc.setQueryData<KRecord[]>(key, (old) =>
        old?.map((r) => (r.id === id ? saved : r)) ?? old,
      );
      if (successMsg) toast.success(successMsg);
    } catch (err) {
      if (prev) qc.setQueryData(key, prev);
      toast.error(`Couldn't update ticket: ${(err as Error).message}`);
    }
  }

  function handleDrop(e: DragEvent<HTMLDivElement>, status: string) {
    e.preventDefault();
    setDropTarget(null);
    const id = dragId ?? e.dataTransfer.getData("text/plain");
    setDragId(null);
    if (!id) return;
    const ticket = (ticketsQ.data ?? []).find((t) => t.id === id);
    if (!ticket || (ticket.data as TicketData).status === status) return;
    const label = BOARD_COLUMNS.find((c) => c.status === status)?.label ?? status;
    void patchTicket(
      id,
      { status },
      `Moved “${recordLabel(ticket)}” to ${label}`,
    );
  }

  const isLoading = ticketsQ.isLoading;
  const isError = ticketsQ.isError;

  return (
    <section className="flex flex-col gap-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <Eyebrow>Service</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">
            Helpdesk
          </h1>
          <p className="max-w-prose text-sm text-fg-muted">
            Triage open tickets, keep an eye on what's about to breach its
            service level, and set the response targets your team commits to.
          </p>
        </div>
        <Button asChild variant="secondary" size="sm">
          <Link to={`/records/${TICKET_KTYPE}/new`}>
            <Plus className="h-4 w-4" aria-hidden />
            New ticket
          </Link>
        </Button>
      </header>

      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-end justify-between gap-3">
          <div className="w-full max-w-xs">
            <Field label="Show tickets for" htmlFor="assignee-filter">
              <Select
                id="assignee-filter"
                size="sm"
                value={assigneeFilter}
                onChange={(e) => setAssigneeFilter(e.target.value)}
              >
                <option value="all">Everyone</option>
                <option value="unassigned">Unassigned</option>
                {employees.map((e) => (
                  <option key={e.id} value={e.id}>
                    {recordLabel(e)}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          {!isLoading && !isError && (
            <span className="pb-2 text-sm text-fg-muted">
              {boardTickets.length}{" "}
              {boardTickets.length === 1 ? "ticket" : "tickets"} in triage
            </span>
          )}
        </div>

        {isLoading ? (
          <BoardSkeleton />
        ) : isError ? (
          <EmptyState
            icon={<AlertTriangle className="h-6 w-6" aria-hidden />}
            title="Couldn't load tickets"
            description={(ticketsQ.error as Error).message}
            action={
              <Button
                variant="secondary"
                onClick={() => void ticketsQ.refetch()}
                disabled={ticketsQ.isFetching}
              >
                Retry
              </Button>
            }
          />
        ) : boardTickets.length === 0 ? (
          <EmptyState
            icon={<Inbox className="h-6 w-6" aria-hidden />}
            title={
              assigneeFilter === "all"
                ? "No open tickets"
                : "Nothing in this queue"
            }
            description={
              assigneeFilter === "all"
                ? "When customers raise tickets they'll appear here, grouped by status, so your team can pick up the next one."
                : "There are no open tickets matching this filter. Try switching back to everyone."
            }
            action={
              assigneeFilter === "all" ? (
                <Button asChild>
                  <Link to={`/records/${TICKET_KTYPE}/new`}>
                    <Plus className="h-4 w-4" aria-hidden />
                    New ticket
                  </Link>
                </Button>
              ) : (
                <Button variant="secondary" onClick={() => setAssigneeFilter("all")}>
                  Show everyone
                </Button>
              )
            }
          />
        ) : (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
            {BOARD_COLUMNS.map((col) => {
              const list = ticketsByStatus.get(col.status) ?? [];
              return (
                <div
                  key={col.status}
                  onDragOver={(e) => {
                    e.preventDefault();
                    setDropTarget(col.status);
                  }}
                  onDragLeave={() =>
                    setDropTarget((cur) => (cur === col.status ? null : cur))
                  }
                  onDrop={(e) => handleDrop(e, col.status)}
                  className={cn(
                    "flex min-h-32 flex-col gap-2 rounded-lg border bg-bg-subtle/50 p-2 transition-colors",
                    dropTarget === col.status
                      ? "border-accent bg-accent/5"
                      : "border-border",
                  )}
                >
                  <div className="flex items-center justify-between px-1 py-0.5">
                    <span className="text-sm font-semibold text-fg">
                      {col.label}
                    </span>
                    <Badge variant="neutral" size="sm">
                      {list.length}
                    </Badge>
                  </div>
                  <div className="flex flex-col gap-2">
                    {list.map((t) => {
                      const d = t.data as TicketData;
                      const sla = slaState(d.sla_resolution_by);
                      const customer = d.customer_id
                        ? orgNames.get(d.customer_id)
                        : undefined;
                      return (
                        <article
                          key={t.id}
                          draggable
                          onDragStart={(e) => {
                            setDragId(t.id);
                            e.dataTransfer.setData("text/plain", t.id);
                            e.dataTransfer.effectAllowed = "move";
                          }}
                          onDragEnd={() => {
                            setDragId(null);
                            setDropTarget(null);
                          }}
                          className={cn(
                            "flex cursor-grab flex-col gap-2 rounded-md border border-border bg-bg-elevated p-3 shadow-sm transition-opacity active:cursor-grabbing",
                            dragId === t.id && "opacity-50",
                          )}
                        >
                          <div className="flex items-start justify-between gap-2">
                            <Link
                              to={`/records/${TICKET_KTYPE}/${t.id}`}
                              className="line-clamp-2 text-sm font-medium text-fg hover:text-accent"
                            >
                              {d.subject ?? recordLabel(t)}
                            </Link>
                            <Badge
                              variant={priorityVariant(d.priority ?? "")}
                              size="sm"
                              className="shrink-0"
                            >
                              {humanizeToken(d.priority ?? "—")}
                            </Badge>
                          </div>

                          <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-fg-muted">
                            {customer && <span className="truncate">{customer}</span>}
                            {d.channel && (
                              <>
                                <span aria-hidden>·</span>
                                <span>{humanizeToken(d.channel)}</span>
                              </>
                            )}
                          </div>

                          {sla ? (
                            <Badge variant={sla.variant} size="sm" className="w-fit">
                              {sla.label}
                            </Badge>
                          ) : (
                            <span className="text-xs italic text-fg-subtle">
                              No SLA deadline
                            </span>
                          )}

                          <div className="grid grid-cols-2 gap-2 pt-0.5">
                            <Field label="Assignee" htmlFor={`assignee-${t.id}`} hideLabel>
                              <Select
                                id={`assignee-${t.id}`}
                                size="sm"
                                aria-label={`Assignee for ${d.subject ?? "ticket"}`}
                                value={d.assigned_to ?? ""}
                                onChange={(e) =>
                                  void patchTicket(
                                    t.id,
                                    { assigned_to: e.target.value },
                                    e.target.value
                                      ? `Assigned to ${
                                          employeeNames.get(e.target.value) ??
                                          "teammate"
                                        }`
                                      : "Ticket unassigned",
                                  )
                                }
                              >
                                <option value="">Unassigned</option>
                                {employees.map((emp) => (
                                  <option key={emp.id} value={emp.id}>
                                    {recordLabel(emp)}
                                  </option>
                                ))}
                              </Select>
                            </Field>
                            <Field label="Status" htmlFor={`status-${t.id}`} hideLabel>
                              <Select
                                id={`status-${t.id}`}
                                size="sm"
                                aria-label={`Status for ${d.subject ?? "ticket"}`}
                                value={d.status ?? "open"}
                                onChange={(e) => {
                                  const label =
                                    BOARD_COLUMNS.find(
                                      (c) => c.status === e.target.value,
                                    )?.label ?? e.target.value;
                                  void patchTicket(
                                    t.id,
                                    { status: e.target.value },
                                    `Moved to ${label}`,
                                  );
                                }}
                              >
                                {BOARD_COLUMNS.map((c) => (
                                  <option key={c.status} value={c.status}>
                                    {c.label}
                                  </option>
                                ))}
                              </Select>
                            </Field>
                          </div>
                        </article>
                      );
                    })}
                    {list.length === 0 && (
                      <p className="px-1 py-6 text-center text-xs text-fg-subtle">
                        Nothing here
                      </p>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <SlaPolicies query={policiesQ} qc={qc} />
    </section>
  );
}

interface PolicyFormErrors {
  name?: string;
  response?: string;
  resolution?: string;
}

function SlaPolicies({
  query,
  qc,
}: {
  query: ReturnType<typeof useQuery<{ policies: SLAPolicy[] }>>;
  qc: ReturnType<typeof useQueryClient>;
}) {
  const [form, setForm] = useState<UpsertSLAPolicyInput>({
    name: "",
    priority: "medium",
    response_minutes: 60,
    resolution_minutes: 480,
    active: true,
  });
  const [touched, setTouched] = useState(false);

  const upsert = useMutation({
    mutationFn: (input: UpsertSLAPolicyInput) => api.upsertSLAPolicy(input),
    onSuccess: (saved) => {
      void qc.invalidateQueries({ queryKey: ["helpdesk", "sla-policies"] });
      toast.success(`Saved “${saved.name}” policy`);
      setForm({
        name: "",
        priority: "medium",
        response_minutes: 60,
        resolution_minutes: 480,
        active: true,
      });
      setTouched(false);
    },
    onError: (err) => toast.error(`Couldn't save policy: ${(err as Error).message}`),
  });

  const errors = useMemo<PolicyFormErrors>(() => {
    const e: PolicyFormErrors = {};
    if (!form.name.trim()) e.name = "Give the policy a name.";
    if (!(form.response_minutes > 0)) e.response = "Must be at least 1 minute.";
    if (!(form.resolution_minutes > 0)) {
      e.resolution = "Must be at least 1 minute.";
    } else if (form.resolution_minutes < form.response_minutes) {
      e.resolution = "Resolution target should be at or after the response target.";
    }
    return e;
  }, [form]);

  const submit = (ev: FormEvent) => {
    ev.preventDefault();
    setTouched(true);
    if (Object.keys(errors).length > 0) return;
    upsert.mutate(form);
  };

  const policies = query.data?.policies ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle>Service-level targets</CardTitle>
        <CardDescription>
          Set how quickly tickets of each priority should get a first response
          and a resolution. New tickets inherit the matching policy.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-6">
        <form onSubmit={submit} noValidate className="flex flex-col gap-4">
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <Field
              label="Policy name"
              required
              error={touched ? errors.name : undefined}
            >
              <Input
                value={form.name}
                placeholder="e.g. Standard support"
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </Field>
            <Field label="Priority" required>
              <Select
                value={form.priority}
                onChange={(e) =>
                  setForm({
                    ...form,
                    priority: e.target.value as UpsertSLAPolicyInput["priority"],
                  })
                }
              >
                <option value="low">Low</option>
                <option value="medium">Medium</option>
                <option value="high">High</option>
                <option value="urgent">Urgent</option>
              </Select>
            </Field>
            <Field
              label="First response"
              required
              error={touched ? errors.response : undefined}
              help={`Target: ${formatDuration(form.response_minutes)}`}
            >
              <Input
                type="number"
                min={1}
                inputMode="numeric"
                trailingAddon={<span className="text-xs text-fg-subtle">min</span>}
                value={Number.isFinite(form.response_minutes) ? form.response_minutes : ""}
                onChange={(e) =>
                  setForm({ ...form, response_minutes: Number(e.target.value) })
                }
              />
            </Field>
            <Field
              label="Resolution"
              required
              error={touched ? errors.resolution : undefined}
              help={`Target: ${formatDuration(form.resolution_minutes)}`}
            >
              <Input
                type="number"
                min={1}
                inputMode="numeric"
                trailingAddon={<span className="text-xs text-fg-subtle">min</span>}
                value={Number.isFinite(form.resolution_minutes) ? form.resolution_minutes : ""}
                onChange={(e) =>
                  setForm({ ...form, resolution_minutes: Number(e.target.value) })
                }
              />
            </Field>
          </div>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <Field label="Availability" htmlFor="policy-active" className="w-44">
              <Select
                id="policy-active"
                value={form.active ? "active" : "paused"}
                onChange={(e) =>
                  setForm({ ...form, active: e.target.value === "active" })
                }
              >
                <option value="active">Active</option>
                <option value="paused">Paused</option>
              </Select>
            </Field>
            <Button type="submit" disabled={upsert.isPending}>
              {upsert.isPending ? "Saving…" : "Save policy"}
            </Button>
          </div>
        </form>

        {query.isLoading ? (
          <div className="flex flex-col gap-2">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </div>
        ) : query.isError ? (
          <p className="text-sm text-danger">
            Couldn't load policies: {(query.error as Error).message}
          </p>
        ) : policies.length === 0 ? (
          <p className="rounded-md border border-dashed border-border px-4 py-6 text-center text-sm text-fg-muted">
            No policies yet. Add your first one above and it'll show here.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Priority</TableHead>
                <TableHead>First response</TableHead>
                <TableHead>Resolution</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {policies.map((p) => (
                <TableRow key={p.id}>
                  <TableCell className="font-medium text-fg">{p.name}</TableCell>
                  <TableCell>
                    <Badge variant={priorityVariant(p.priority)} size="sm">
                      {humanizeToken(p.priority)}
                    </Badge>
                  </TableCell>
                  <TableCell className="font-tabular">
                    {formatDuration(p.response_minutes)}
                  </TableCell>
                  <TableCell className="font-tabular">
                    {formatDuration(p.resolution_minutes)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={p.active ? "success" : "neutral"} size="sm">
                      {p.active ? "Active" : "Paused"}
                    </Badge>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function BoardSkeleton() {
  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
      {Array.from({ length: 4 }).map((_, col) => (
        <div
          key={col}
          className="flex flex-col gap-2 rounded-lg border border-border bg-bg-subtle/50 p-2"
        >
          <Skeleton className="h-5 w-24" />
          {Array.from({ length: 2 }).map((_, card) => (
            <div
              key={card}
              className="flex flex-col gap-2 rounded-md border border-border bg-bg-elevated p-3"
            >
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-3 w-2/3" />
              <Skeleton className="h-5 w-24" />
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}
