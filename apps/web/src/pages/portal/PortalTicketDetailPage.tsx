import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { Inbox, MessageSquare } from "lucide-react";
import {
  Badge,
  Button,
  Card,
  CardContent,
  EmptyState,
  Field,
  Skeleton,
  Textarea,
  cn,
} from "@kapp/ui";
import { portalApi } from "../../lib/portalApi";
import { AuthAlert } from "../auth/AuthScaffold";
import { PortalShell } from "./PortalShell";
import {
  friendlyPortalError,
  replyKindLabel,
  ticketPriorityMeta,
  ticketStatusMeta,
} from "./portalStrings";

interface Reply {
  from?: string;
  body?: string;
  kind?: string;
}

/** Read a string field from the JSONB ticket payload, ignoring blanks. */
function str(v: unknown): string | undefined {
  return typeof v === "string" && v.trim() ? v : undefined;
}

export function PortalTicketDetailPage() {
  const { tenant_slug, id } = useParams<{ tenant_slug: string; id: string }>();
  const qc = useQueryClient();
  const ticketsHref = `/portal/${tenant_slug}/tickets`;
  // Scope by tenant_slug alongside the ticket id so a portal user
  // who signs in to a second tenant in the same browser session
  // does not see a flash of the previous tenant's cached ticket
  // that happens to share the same UUID space.
  const q = useQuery({
    queryKey: ["portal-ticket", tenant_slug, id],
    queryFn: () => portalApi.getTicket(id!),
    enabled: !!id,
  });
  const [reply, setReply] = useState("");
  const replyMut = useMutation({
    mutationFn: (body: string) => portalApi.reply(id!, body),
    onSuccess: () => {
      setReply("");
      qc.invalidateQueries({ queryKey: ["portal-ticket", tenant_slug, id] });
    },
  });

  if (q.isLoading) {
    return (
      <PortalShell
        title="Request"
        backTo={ticketsHref}
        backLabel="Back to requests"
        width="md"
      >
        <Card>
          <CardContent className="flex flex-col gap-4 p-6">
            <Skeleton variant="text" className="h-6 w-2/3" />
            <Skeleton variant="text" className="w-40" />
            <Skeleton variant="rect" className="h-24 w-full" />
          </CardContent>
        </Card>
      </PortalShell>
    );
  }

  if (q.isError) {
    return (
      <PortalShell
        title="Request"
        backTo={ticketsHref}
        backLabel="Back to requests"
        width="md"
      >
        <EmptyState
          icon={<Inbox />}
          title="We couldn't load this request"
          description={friendlyPortalError(q.error)}
          action={
            <Button variant="outline" onClick={() => q.refetch()}>
              Try again
            </Button>
          }
        />
      </PortalShell>
    );
  }

  if (!q.data) {
    return (
      <PortalShell
        title="Request not found"
        backTo={ticketsHref}
        backLabel="Back to requests"
        width="md"
      >
        <EmptyState
          icon={<Inbox />}
          title="This request isn't available"
          description="It may have been closed or you may not have access to it."
          action={
            <Button asChild variant="outline">
              <a href={ticketsHref}>Back to requests</a>
            </Button>
          }
        />
      </PortalShell>
    );
  }

  const d = (q.data.data ?? {}) as Record<string, unknown>;
  const subject = str(d.subject) ?? "Untitled request";
  const status = ticketStatusMeta(str(d.status) ?? q.data.status);
  const priority = ticketPriorityMeta(str(d.priority));
  const description = str(d.description);
  const replies = Array.isArray(d.replies) ? (d.replies as Reply[]) : [];

  return (
    <PortalShell
      title={subject}
      backTo={ticketsHref}
      backLabel="Back to requests"
      width="md"
    >
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant={status.variant}>{status.label}</Badge>
        <span className="text-fg-subtle" aria-hidden="true">
          ·
        </span>
        <span className="text-sm text-fg-muted">Priority</span>
        <Badge variant={priority.variant}>{priority.label}</Badge>
      </div>

      <Card>
        <CardContent className="flex flex-col gap-2 p-6">
          <h2 className="text-sm font-medium text-fg-muted">Request details</h2>
          {description ? (
            <p className="whitespace-pre-wrap text-sm text-fg">{description}</p>
          ) : (
            <p className="text-sm text-fg-subtle">No description provided.</p>
          )}
        </CardContent>
      </Card>

      <section className="flex flex-col gap-3">
        <h2 className="text-base font-medium text-fg">Conversation</h2>
        {replies.length === 0 ? (
          <Card>
            <EmptyState
              icon={<MessageSquare />}
              title="No replies yet"
              description="Add a reply below and our support team will get back to you."
            />
          </Card>
        ) : (
          <ul className="flex flex-col gap-3">
            {replies.map((rep, i) => {
              const fromCustomer = (rep.kind ?? "").toLowerCase() === "customer";
              return (
                <li
                  key={i}
                  className={cn(
                    "rounded-md border border-border border-s-4 bg-bg-subtle p-3",
                    fromCustomer ? "border-s-info" : "border-s-success",
                  )}
                >
                  <div className="mb-1 flex flex-wrap items-center gap-2 text-xs text-fg-muted">
                    <span className="font-medium text-fg">
                      {replyKindLabel(rep.kind)}
                    </span>
                    {str(rep.from) && <span>· {str(rep.from)}</span>}
                  </div>
                  <div className="whitespace-pre-wrap text-sm text-fg">
                    {rep.body}
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </section>

      <Card>
        <CardContent className="p-6">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (!reply.trim() || replyMut.isPending) return;
              replyMut.mutate(reply);
            }}
            className="flex flex-col gap-4"
          >
            <Field label="Add a reply">
              <Textarea
                value={reply}
                onChange={(e) => setReply(e.target.value)}
                rows={4}
                placeholder="Type your reply…"
              />
            </Field>
            {replyMut.isError && (
              <AuthAlert tone="danger">
                {friendlyPortalError(
                  replyMut.error,
                  "We couldn't send your reply. Please try again.",
                )}
              </AuthAlert>
            )}
            <div className="flex justify-end">
              <Button
                type="submit"
                disabled={replyMut.isPending || !reply.trim()}
              >
                {replyMut.isPending ? "Sending…" : "Send reply"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </PortalShell>
  );
}
