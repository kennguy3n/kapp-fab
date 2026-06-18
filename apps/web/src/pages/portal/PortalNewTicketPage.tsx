import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useNavigate, useParams } from "react-router-dom";
import {
  Button,
  Card,
  CardContent,
  Field,
  Input,
  Select,
  Textarea,
} from "@kapp/ui";
import { portalApi } from "../../lib/portalApi";
import { AuthAlert } from "../auth/AuthScaffold";
import { PortalShell } from "./PortalShell";
import { friendlyPortalError, PRIORITY_OPTIONS } from "./portalStrings";

export function PortalNewTicketPage() {
  const { tenant_slug } = useParams<{ tenant_slug: string }>();
  const nav = useNavigate();
  const [subject, setSubject] = useState("");
  const [description, setDescription] = useState("");
  const [priority, setPriority] = useState("medium");
  // Surface a required-field hint only after a submit attempt so the
  // form doesn't shout at the customer before they've typed anything.
  const [showErrors, setShowErrors] = useState(false);
  const ticketsHref = `/portal/${tenant_slug}/tickets`;

  const mut = useMutation({
    mutationFn: () => portalApi.createTicket(subject, description, priority),
    onSuccess: (t) => nav(`/portal/${tenant_slug}/tickets/${t.id}`),
  });

  const subjectMissing = showErrors && !subject.trim();

  return (
    <PortalShell
      title="New request"
      description="Tell us what you need help with and we'll get back to you."
      backTo={ticketsHref}
      backLabel="Back to requests"
      width="md"
    >
      <Card>
        <CardContent className="p-6">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (!subject.trim()) {
                setShowErrors(true);
                return;
              }
              mut.mutate();
            }}
            className="flex flex-col gap-5"
            noValidate
          >
            <Field
              label="Subject"
              required
              error={subjectMissing ? "Please enter a short subject." : undefined}
              help={
                subjectMissing
                  ? undefined
                  : "A short summary, e.g. \u201cCan't access my invoices\u201d."
              }
            >
              <Input
                value={subject}
                onChange={(e) => setSubject(e.target.value)}
                placeholder="Brief summary of your request"
                autoFocus
              />
            </Field>

            <Field
              label="Description"
              help="Include any details that will help us help you faster."
            >
              <Textarea
                rows={6}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="Describe the issue, what you expected, and any steps to reproduce it."
              />
            </Field>

            <Field label="Priority" help="How urgent is this for you?">
              <Select
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              >
                {PRIORITY_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </Select>
            </Field>

            {mut.isError && (
              <AuthAlert tone="danger">
                {friendlyPortalError(
                  mut.error,
                  "We couldn't submit your request. Please try again.",
                )}
              </AuthAlert>
            )}

            <div className="flex flex-wrap items-center justify-end gap-2 border-t border-border pt-4">
              <Button
                type="button"
                variant="ghost"
                onClick={() => nav(ticketsHref)}
              >
                Cancel
              </Button>
              <Button type="submit" disabled={mut.isPending}>
                {mut.isPending ? "Submitting…" : "Submit request"}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </PortalShell>
  );
}
