import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import { portalApi } from "../../lib/portalApi";

export function PortalNewTicketPage() {
  const { tenant_slug } = useParams<{ tenant_slug: string }>();
  const nav = useNavigate();
  const [subject, setSubject] = useState("");
  const [description, setDescription] = useState("");
  const [priority, setPriority] = useState("medium");
  const mut = useMutation({
    mutationFn: () => portalApi.createTicket(subject, description, priority),
    onSuccess: (t) => nav(`/portal/${tenant_slug}/tickets/${t.id}`),
  });
  return (
    <main style={{ maxWidth: 640, margin: "32px auto", padding: 16 }}>
      <h1>New ticket</h1>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!subject.trim()) return;
          mut.mutate();
        }}
        style={{ display: "grid", gap: 8 }}
      >
        <label>
          Subject
          <input
            required
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
            style={inp}
          />
        </label>
        <label>
          Description
          <textarea
            rows={6}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            style={inp}
          />
        </label>
        <label>
          Priority
          <select
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
            style={inp}
          >
            <option value="low">low</option>
            <option value="medium">medium</option>
            <option value="high">high</option>
            <option value="urgent">urgent</option>
          </select>
        </label>
        <button type="submit" disabled={mut.isPending}>
          Submit
        </button>
        {mut.error && (
          <div style={{ color: "#991b1b" }}>{(mut.error as Error).message}</div>
        )}
      </form>
    </main>
  );
}

const inp: React.CSSProperties = {
  padding: 8,
  border: "1px solid #d1d5db",
  borderRadius: 6,
  width: "100%",
};
