import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { KRecord, SLAPolicy, UpsertSLAPolicyInput } from "@kapp/client";
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
    <section>
      <h1>Helpdesk</h1>
      <p style={{ color: "#6b7280" }}>
        Tickets + SLA policies. Breaches are logged to ticket_sla_log
        and can be charted via the report builder.
      </p>

      <h2 style={{ marginTop: 24, fontSize: 16 }}>Open Tickets</h2>
      {tickets.isLoading && <p>Loading…</p>}
      {tickets.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load tickets: {(tickets.error as Error).message}
        </p>
      )}
      {!tickets.isLoading && openTickets.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No open tickets.
        </p>
      )}
      {openTickets.length > 0 && (
        <table style={{ width: "100%", fontSize: 13, borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th>Subject</th>
              <th>Status</th>
              <th>Priority</th>
              <th>Channel</th>
              <th>Due</th>
            </tr>
          </thead>
          <tbody>
            {openTickets.map((r) => {
              const d = r.data as unknown as TicketData;
              const overdue =
                d.sla_resolution_by != null &&
                new Date(d.sla_resolution_by).getTime() < Date.now();
              return (
                <tr key={r.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td>
                    <Link to={`/records/${TICKET_KTYPE}/${r.id}`}>
                      {d.subject ?? r.id}
                    </Link>
                  </td>
                  <td>{d.status ?? ""}</td>
                  <td>{d.priority ?? ""}</td>
                  <td>{d.channel ?? ""}</td>
                  <td style={{ color: overdue ? "#b91c1c" : "inherit" }}>
                    {d.sla_resolution_by?.slice(0, 16).replace("T", " ")}
                    {overdue ? " ⚠" : ""}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}

      <h2 style={{ marginTop: 32, fontSize: 16 }}>SLA Policies</h2>
      <form
        onSubmit={submit}
        style={{ display: "flex", gap: 8, flexWrap: "wrap", margin: "12px 0", fontSize: 13 }}
      >
        <input
          placeholder="name"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          required
        />
        <select
          value={form.priority}
          onChange={(e) =>
            setForm({ ...form, priority: e.target.value as UpsertSLAPolicyInput["priority"] })
          }
        >
          <option value="low">low</option>
          <option value="medium">medium</option>
          <option value="high">high</option>
          <option value="urgent">urgent</option>
        </select>
        <input
          type="number"
          placeholder="response min"
          value={form.response_minutes}
          onChange={(e) =>
            setForm({ ...form, response_minutes: Number(e.target.value) })
          }
          required
          style={{ width: 120 }}
        />
        <input
          type="number"
          placeholder="resolution min"
          value={form.resolution_minutes}
          onChange={(e) =>
            setForm({ ...form, resolution_minutes: Number(e.target.value) })
          }
          required
          style={{ width: 140 }}
        />
        <label style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <input
            type="checkbox"
            checked={form.active ?? true}
            onChange={(e) => setForm({ ...form, active: e.target.checked })}
          />
          active
        </label>
        <button type="submit" disabled={upsert.isPending}>
          {upsert.isPending ? "Saving…" : "Save policy"}
        </button>
      </form>

      {(policies.data?.policies ?? []).length > 0 && (
        <table style={{ width: "100%", fontSize: 13, borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th>Name</th>
              <th>Priority</th>
              <th style={{ textAlign: "right" }}>Response (min)</th>
              <th style={{ textAlign: "right" }}>Resolution (min)</th>
              <th>Active</th>
            </tr>
          </thead>
          <tbody>
            {(policies.data?.policies ?? []).map((p) => (
              <tr key={p.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                <td>{p.name}</td>
                <td>{p.priority}</td>
                <td style={{ textAlign: "right" }}>{p.response_minutes}</td>
                <td style={{ textAlign: "right" }}>{p.resolution_minutes}</td>
                <td>{p.active ? "yes" : "no"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
