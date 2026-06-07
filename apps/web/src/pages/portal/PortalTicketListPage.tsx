import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
    <main className="mx-auto mt-8 max-w-[720px] p-4">
      <h1>Your tickets</h1>
      <p>
        <Link to={`/portal/${tenant_slug}/tickets/new`}>+ New ticket</Link>
      </p>
      {q.isLoading && <div>Loading…</div>}
      {q.error && (
        <div className="text-danger">{(q.error as Error).message}</div>
      )}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Subject</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Updated</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tickets.map((t) => (
            <TableRow key={t.id}>
              <TableCell>
                <Link to={`/portal/${tenant_slug}/tickets/${t.id}`}>
                  {(t.data as { subject?: string }).subject ?? t.id}
                </Link>
              </TableCell>
              <TableCell>
                {(t.data as { status?: string }).status ?? t.status}
              </TableCell>
              <TableCell>{new Date(t.updated_at).toLocaleString()}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </main>
  );
}
