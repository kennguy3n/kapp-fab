import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router-dom";
import { Inbox, Plus } from "lucide-react";
import {
  Badge,
  Button,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { portalApi, type PortalTicket } from "../../lib/portalApi";
import { useFormatter } from "../../lib/i18n";
import { PortalShell } from "./PortalShell";
import { friendlyPortalError, ticketStatusMeta } from "./portalStrings";

/** Prefer the human subject; fall back to a friendly label, never a UUID. */
function ticketSubject(t: PortalTicket): string {
  const subject = (t.data as { subject?: unknown }).subject;
  if (typeof subject === "string" && subject.trim()) return subject.trim();
  return "Untitled request";
}

function ticketStatus(t: PortalTicket): string {
  const status = (t.data as { status?: unknown }).status;
  if (typeof status === "string" && status.trim()) return status;
  return t.status;
}

export function PortalTicketListPage() {
  const { tenant_slug } = useParams<{ tenant_slug: string }>();
  const nav = useNavigate();
  const { dateTime } = useFormatter();
  // Include tenant_slug in the key so a portal user switching
  // tenants in the same browser session does not briefly see the
  // previous tenant's cached tickets before the refetch lands.
  const q = useQuery({
    queryKey: ["portal-tickets", tenant_slug],
    queryFn: () => portalApi.listTickets(),
  });
  const tickets = q.data?.tickets ?? [];

  const newTicketHref = `/portal/${tenant_slug}/tickets/new`;

  return (
    <PortalShell
      title="Your requests"
      description="Track the status of your support requests and add updates."
      actions={
        <Button onClick={() => nav(newTicketHref)} leadingIcon={<Plus />}>
          New request
        </Button>
      }
    >
      {q.isLoading ? (
        <TicketTableFrame>
          {Array.from({ length: 4 }).map((_, i) => (
            <TableRow key={i}>
              <TableCell>
                <Skeleton variant="text" className="w-48" />
              </TableCell>
              <TableCell>
                <Skeleton variant="rect" className="h-5 w-20 rounded-full" />
              </TableCell>
              <TableCell>
                <Skeleton variant="text" className="w-28" />
              </TableCell>
            </TableRow>
          ))}
        </TicketTableFrame>
      ) : q.isError ? (
        <EmptyState
          icon={<Inbox />}
          title="We couldn't load your requests"
          description={friendlyPortalError(q.error)}
          action={
            <Button variant="outline" onClick={() => q.refetch()}>
              Try again
            </Button>
          }
        />
      ) : tickets.length === 0 ? (
        <EmptyState
          icon={<Inbox />}
          title="No requests yet"
          description="When you open a support request, it will appear here so you can follow its progress."
          action={
            <Button onClick={() => nav(newTicketHref)} leadingIcon={<Plus />}>
              New request
            </Button>
          }
        />
      ) : (
        <TicketTableFrame>
          {tickets.map((t) => {
            const status = ticketStatusMeta(ticketStatus(t));
            return (
              <TableRow key={t.id} className="group">
                <TableCell className="font-medium">
                  <Link
                    to={`/portal/${tenant_slug}/tickets/${t.id}`}
                    className="rounded-sm text-fg underline-offset-4 transition-colors hover:text-accent group-hover:underline"
                  >
                    {ticketSubject(t)}
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant={status.variant}>{status.label}</Badge>
                </TableCell>
                <TableCell className="text-fg-muted">
                  {dateTime(new Date(t.updated_at))}
                </TableCell>
              </TableRow>
            );
          })}
        </TicketTableFrame>
      )}
    </PortalShell>
  );
}

function TicketTableFrame({ children }: { children: React.ReactNode }) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-bg-elevated shadow-sm">
      <Table>
        <TableHeader className="sticky top-0">
          <TableRow className="hover:bg-bg-subtle">
            <TableHead>Subject</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Last updated</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>{children}</TableBody>
      </Table>
    </div>
  );
}
