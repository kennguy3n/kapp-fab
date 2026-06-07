import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { KRecord, SLAPolicy, UpsertSLAPolicyInput } from "@kapp/client";
import {
  Button,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
} from "@kapp/ui";
import { api } from "../lib/api";

const TICKET_KTYPE = "helpdesk.ticket";

interface TicketData {
  subject: string;
  status?: string;
  priority?: string;
  channel?: string;
  customer_id?: string;
  assigned_to?: string;
  sla_resolution_by?: string;
}

/**
 * HelpdeskPage combines an open-tickets list with SLA policy
 * management. Tickets themselves ride the generic KRecord list/form
 * pages for deep links; this page is the tenant-wide triage view.
 */
export function HelpdeskPage() {
  const qc = useQueryClient();
  const tickets = useQuery<KRecord[]>({
    queryKey: ["records", TICKET_KTYPE],
    queryFn: () => api.listRecords(TICKET_KTYPE),
  });
  const policies = useQuery<{ policies: SLAPolicy[] }>({
    queryKey: ["helpdesk", "sla-policies"],
    queryFn: () => api.listSLAPolicies(),
  });

  const upsert = useMutation({
    mutationFn: (input: UpsertSLAPolicyInput) => api.upsertSLAPolicy(input),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["helpdesk", "sla-policies"] }),
  });

  const [form, setForm] = useState<UpsertSLAPolicyInput>({
    name: "Standard",
    priority: "medium",
    response_minutes: 60,
    resolution_minutes: 480,
    active: true,
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    upsert.mutate(form);
  };

  const openTickets = (tickets.data ?? []).filter((r) => {
    const d = r.data as unknown as TicketData;
    return d.status !== "closed" && d.status !== "resolved";
  });

  return (
    <section className="flex flex-col gap-2">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">Helpdesk</h1>
      <p className="text-sm text-fg-muted">
        Tickets + SLA policies. Breaches are logged to ticket_sla_log
        and can be charted via the report builder.
      </p>

      <h2 className="mt-6 text-base font-semibold text-fg">Open Tickets</h2>
      {tickets.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {tickets.isError && (
        <p className="text-sm text-danger">
          Failed to load tickets: {(tickets.error as Error).message}
        </p>
      )}
      {!tickets.isLoading && openTickets.length === 0 && (
        <p className="text-sm italic text-fg-subtle">No open tickets.</p>
      )}
      {openTickets.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Subject</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Priority</TableHead>
              <TableHead>Channel</TableHead>
              <TableHead>Due</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {openTickets.map((r) => {
              const d = r.data as unknown as TicketData;
              const overdue =
                d.sla_resolution_by != null &&
                new Date(d.sla_resolution_by).getTime() < Date.now();
              return (
                <TableRow key={r.id}>
                  <TableCell>
                    <Link
                      to={`/records/${TICKET_KTYPE}/${r.id}`}
                      className="text-accent hover:underline"
                    >
                      {d.subject ?? r.id}
                    </Link>
                  </TableCell>
                  <TableCell>{d.status ?? ""}</TableCell>
                  <TableCell>{d.priority ?? ""}</TableCell>
                  <TableCell>{d.channel ?? ""}</TableCell>
                  <TableCell className={cn(overdue && "text-danger")}>
                    {d.sla_resolution_by?.slice(0, 16).replace("T", " ")}
                    {overdue ? " ⚠" : ""}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}

      <h2 className="mt-8 text-base font-semibold text-fg">SLA Policies</h2>
      <form
        onSubmit={submit}
        className="my-3 flex flex-wrap items-center gap-2"
      >
        <Input
          placeholder="name"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          required
          className="w-40"
        />
        <Select
          className="w-auto"
          value={form.priority}
          onChange={(e) =>
            setForm({ ...form, priority: e.target.value as UpsertSLAPolicyInput["priority"] })
          }
        >
          <option value="low">low</option>
          <option value="medium">medium</option>
          <option value="high">high</option>
          <option value="urgent">urgent</option>
        </Select>
        <Input
          type="number"
          placeholder="response min"
          value={form.response_minutes}
          onChange={(e) =>
            setForm({ ...form, response_minutes: Number(e.target.value) })
          }
          required
          className="w-32"
        />
        <Input
          type="number"
          placeholder="resolution min"
          value={form.resolution_minutes}
          onChange={(e) =>
            setForm({ ...form, resolution_minutes: Number(e.target.value) })
          }
          required
          className="w-36"
        />
        <label className="flex items-center gap-1.5 text-sm text-fg">
          <input
            type="checkbox"
            checked={form.active ?? true}
            onChange={(e) => setForm({ ...form, active: e.target.checked })}
            className="size-4 rounded border-border text-accent focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
          />
          active
        </label>
        <Button type="submit" disabled={upsert.isPending}>
          {upsert.isPending ? "Saving…" : "Save policy"}
        </Button>
      </form>

      {(policies.data?.policies ?? []).length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Priority</TableHead>
              <TableHead className="text-right">Response (min)</TableHead>
              <TableHead className="text-right">Resolution (min)</TableHead>
              <TableHead>Active</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(policies.data?.policies ?? []).map((p) => (
              <TableRow key={p.id}>
                <TableCell>{p.name}</TableCell>
                <TableCell>{p.priority}</TableCell>
                <TableCell className="text-right">{p.response_minutes}</TableCell>
                <TableCell className="text-right">{p.resolution_minutes}</TableCell>
                <TableCell>{p.active ? "yes" : "no"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
