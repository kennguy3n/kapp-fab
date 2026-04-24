import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { portalApi } from "../../lib/portalApi";

interface Reply {
  from?: string;
  body?: string;
  kind?: string;
}

export function PortalTicketDetailPage() {
  const { id } = useParams<{ id: string }>();
  const qc = useQueryClient();
  const q = useQuery({
    queryKey: ["portal-ticket", id],
    queryFn: () => portalApi.getTicket(id!),
    enabled: !!id,
  });
  const [reply, setReply] = useState("");
  const replyMut = useMutation({
    mutationFn: () => portalApi.reply(id!, reply),
    onSuccess: () => {
      setReply("");
      qc.invalidateQueries({ queryKey: ["portal-ticket", id] });
    },
  });

  if (q.isLoading) return <div>Loading…</div>;
  if (!q.data) return <div>Not found.</div>;
  const d = (q.data.data ?? {}) as Record<string, unknown>;
  const replies = Array.isArray(d.replies) ? (d.replies as Reply[]) : [];

  return (
    <main style={{ maxWidth: 720, margin: "32px auto", padding: 16 }}>
      <h1>{(d.subject as string) ?? q.data.id}</h1>
      <div style={{ color: "#555" }}>
        Status: {(d.status as string) ?? q.data.status} · priority{" "}
        {(d.priority as string) ?? "medium"}
      </div>
      <p style={{ whiteSpace: "pre-wrap", marginTop: 12 }}>
        {(d.description as string) ?? ""}
      </p>

      <h2 style={{ fontSize: 16, marginTop: 20 }}>Conversation</h2>
      {replies.map((rep, i) => (
        <div
          key={i}
          style={{
            marginBottom: 8,
            padding: 8,
            borderLeft: `4px solid ${
              rep.kind === "customer" ? "#3b82f6" : "#16a34a"
            }`,
            background: "#f9fafb",
          }}
        >
          <div style={{ fontSize: 12, color: "#6b7280" }}>
            {rep.from} · {rep.kind}
          </div>
          <div style={{ whiteSpace: "pre-wrap" }}>{rep.body}</div>
        </div>
      ))}

      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!reply.trim()) return;
          replyMut.mutate();
        }}
        style={{ marginTop: 16, display: "grid", gap: 8 }}
      >
        <textarea
          value={reply}
          onChange={(e) => setReply(e.target.value)}
          rows={4}
          placeholder="Add a reply…"
          style={{ padding: 8, border: "1px solid #d1d5db", borderRadius: 6 }}
        />
        <button type="submit" disabled={replyMut.isPending}>
          Send reply
        </button>
      </form>
    </main>
  );
}
