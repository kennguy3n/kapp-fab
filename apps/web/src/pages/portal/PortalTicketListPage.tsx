import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { portalApi } from "../../lib/portalApi";

export function PortalTicketListPage() {
  const { tenant_slug } = useParams<{ tenant_slug: string }>();
  // Include tenant_slug in the key so a portal user switching
  // tenants in the same browser session does not briefly see the
  // previous tenant's cached tickets before the refetch lands.
  const q = useQuery({
    queryKey: ["portal-tickets", tenant_slug],
    queryFn: () => portalApi.listTickets(),
  });
  const tickets = q.data?.tickets ?? [];
  return (
    <main style={{ maxWidth: 720, margin: "32px auto", padding: 16 }}>
      <h1>Your tickets</h1>
      <p>
        <Link to={`/portal/${tenant_slug}/tickets/new`}>+ New ticket</Link>
      </p>
      {q.isLoading && <div>Loading…</div>}
      {q.error && <div style={{ color: "#991b1b" }}>{(q.error as Error).message}</div>}
      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={th}>Subject</th>
            <th style={th}>Status</th>
            <th style={th}>Updated</th>
          </tr>
        </thead>
        <tbody>
          {tickets.map((t) => (
            <tr key={t.id}>
              <td style={td}>
                <Link to={`/portal/${tenant_slug}/tickets/${t.id}`}>
                  {(t.data as { subject?: string }).subject ?? t.id}
                </Link>
              </td>
              <td style={td}>{(t.data as { status?: string }).status ?? t.status}</td>
              <td style={td}>{new Date(t.updated_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </main>
  );
}

const th: React.CSSProperties = {
  textAlign: "left",
  borderBottom: "1px solid #e5e7eb",
  padding: "4px 6px",
};
const td: React.CSSProperties = {
  padding: "4px 6px",
  borderBottom: "1px solid #f3f4f6",
};
