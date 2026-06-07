import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { Button } from "@kapp/ui";
import { portalApi } from "../../lib/portalApi";

interface Reply {
  from?: string;
  body?: string;
  kind?: string;
}

export function PortalTicketDetailPage() {
  const { tenant_slug, id } = useParams<{ tenant_slug: string; id: string }>();
  const qc = useQueryClient();
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
    mutationFn: () => portalApi.reply(id!, reply),
    onSuccess: () => {
      setReply("");
      qc.invalidateQueries({ queryKey: ["portal-ticket", tenant_slug, id] });
    },
  });

  if (q.isLoading) return <div>Loading…</div>;
  if (!q.data) return <div>Not found.</div>;
  const d = (q.data.data ?? {}) as Record<string, unknown>;
  const replies = Array.isArray(d.replies) ? (d.replies as Reply[]) : [];

  return (
    <main className="mx-auto mt-8 max-w-[720px] p-4">
      <h1>{(d.subject as string) ?? q.data.id}</h1>
      <div className="text-fg-muted">
        Status: {(d.status as string) ?? q.data.status} · priority{" "}
        {(d.priority as string) ?? "medium"}
      </div>
      <p className="mt-3 whitespace-pre-wrap">
        {(d.description as string) ?? ""}
      </p>

      <h2 className="mt-5 text-base">Conversation</h2>
      {replies.map((rep, i) => (
        <div
          key={i}
          className="mb-2 border-l-4 bg-bg-subtle p-2"
          // Data-driven accent: customer replies read as info
          // (blue), agent replies as success (green).
          style={{
            borderLeftColor:
              rep.kind === "customer" ? "var(--info)" : "var(--success)",
          }}
        >
          <div className="text-xs text-fg-muted">
            {rep.from} · {rep.kind}
          </div>
          <div className="whitespace-pre-wrap">{rep.body}</div>
        </div>
      ))}

      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!reply.trim()) return;
          replyMut.mutate();
        }}
        className="mt-4 grid gap-2"
      >
        <textarea
          value={reply}
          onChange={(e) => setReply(e.target.value)}
          rows={4}
          placeholder="Add a reply…"
          className="w-full rounded-md border border-border bg-bg p-2 text-fg"
        />
        <Button type="submit" disabled={replyMut.isPending} className="justify-self-start">
          Send reply
        </Button>
      </form>
    </main>
  );
}
