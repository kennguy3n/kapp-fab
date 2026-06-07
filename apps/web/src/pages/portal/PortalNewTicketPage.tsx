import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import { Button, Input, Select } from "@kapp/ui";
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
    <main className="mx-auto mt-8 max-w-[640px] p-4">
      <h1>New ticket</h1>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!subject.trim()) return;
          mut.mutate();
        }}
        className="grid gap-2"
      >
        <label className="flex flex-col gap-1">
          Subject
          <Input
            required
            value={subject}
            onChange={(e) => setSubject(e.target.value)}
          />
        </label>
        <label className="flex flex-col gap-1">
          Description
          <textarea
            rows={6}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className="w-full rounded-md border border-border bg-bg p-2 text-fg"
          />
        </label>
        <label className="flex flex-col gap-1">
          Priority
          <Select
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
          >
            <option value="low">low</option>
            <option value="medium">medium</option>
            <option value="high">high</option>
            <option value="urgent">urgent</option>
          </Select>
        </label>
        <Button type="submit" disabled={mut.isPending} className="justify-self-start">
          Submit
        </Button>
        {mut.error && (
          <div className="text-danger">{(mut.error as Error).message}</div>
        )}
      </form>
    </main>
  );
}
