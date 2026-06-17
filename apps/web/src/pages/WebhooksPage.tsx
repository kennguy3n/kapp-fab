import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import {
  Plus,
  ScrollText,
  Trash2,
  Webhook as WebhookIcon,
} from "lucide-react";
import type { Webhook, WebhookDelivery } from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  ConfirmDialog,
  EmptyState,
  Field,
  Input,
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Textarea,
  type BadgeProps,
} from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken } from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
  Toggle,
} from "./adminKit";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

// WebhooksPage is the tenant admin surface for outbound webhook
// subscriptions. It lists every endpoint, lets an operator register,
// enable/disable, and remove subscriptions, and surfaces a compact
// recent-delivery preview so failures can be triaged without leaving
// the screen.
export function WebhooksPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const hooksQuery = useQuery({
    queryKey: ["webhooks"],
    queryFn: () => api.listWebhooks(),
  });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [deleteId, setDeleteId] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

  const toggleMut = useMutation({
    mutationFn: (w: Webhook) => api.updateWebhook(w.id, { active: !w.active }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["webhooks"] }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.deleteWebhook(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["webhooks"] });
      if (selectedId === deleteId) setSelectedId(null);
      setDeleteId(null);
    },
    onError: () => setDeleteId(null),
  });

  const hooks = hooksQuery.data?.webhooks ?? [];

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Webhooks"
        description="Send real-time notifications to your own systems when things happen in this workspace. Each endpoint receives a signed payload for the events you choose."
        actions={
          <>
            {hooks.length > 0 && (
              <Badge variant="neutral" size="md">
                {fmt.number(hooks.length)}{" "}
                {hooks.length === 1 ? "endpoint" : "endpoints"}
              </Badge>
            )}
            <Button leadingIcon={<Plus />} onClick={() => setCreateOpen(true)}>
              New webhook
            </Button>
          </>
        }
      />

      {hooksQuery.isLoading ? (
        <AdminTableSkeleton
          columns={["Endpoint", "Events", "Status", "Created", ""]}
        />
      ) : hooksQuery.isError ? (
        <AdminErrorState
          title="Couldn't load webhooks"
          error={hooksQuery.error}
          onRetry={() => hooksQuery.refetch()}
        />
      ) : hooks.length === 0 ? (
        <EmptyState
          icon={<WebhookIcon />}
          title="No webhooks yet"
          description="Register an endpoint to start receiving event notifications in your own systems."
          action={
            <Button leadingIcon={<Plus />} onClick={() => setCreateOpen(true)}>
              New webhook
            </Button>
          }
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Endpoint</TableHead>
              <TableHead>Events</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="text-end">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {hooks.map((h) => (
              <TableRow
                key={h.id}
                onClick={() => setSelectedId(h.id)}
                className={
                  h.id === selectedId ? "cursor-pointer bg-bg-subtle" : "cursor-pointer"
                }
              >
                <TableCell className="max-w-[22rem]">
                  <span className="block truncate font-medium text-fg" title={h.url}>
                    {h.url}
                  </span>
                </TableCell>
                <TableCell>
                  {(h.event_filters ?? []).length === 0 ? (
                    <span className="text-fg-muted">All events</span>
                  ) : (
                    <span className="flex flex-wrap gap-1">
                      {h.event_filters.map((f) => (
                        <Badge key={f} variant="outline" size="xs">
                          {f}
                        </Badge>
                      ))}
                    </span>
                  )}
                </TableCell>
                <TableCell>
                  <span
                    className="flex items-center gap-2"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <Toggle
                      checked={h.active}
                      disabled={toggleMut.isPending}
                      onChange={() => toggleMut.mutate(h)}
                      label={`${h.active ? "Disable" : "Enable"} webhook ${h.url}`}
                    />
                    <Badge variant={h.active ? "success" : "neutral"}>
                      {h.active ? "Active" : "Paused"}
                    </Badge>
                  </span>
                </TableCell>
                <TableCell className="whitespace-nowrap font-tabular text-fg-muted">
                  {fmt.date(new Date(h.created_at))}
                </TableCell>
                <TableCell>
                  <span
                    className="flex items-center justify-end gap-1"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <Button asChild size="sm" variant="ghost">
                      <Link to={`/admin/webhooks/${h.id}/deliveries`}>
                        <ScrollText className="h-4 w-4" />
                        Delivery log
                      </Link>
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Delete webhook ${h.url}`}
                      onClick={() => setDeleteId(h.id)}
                    >
                      <Trash2 className="h-4 w-4" aria-hidden="true" />
                    </Button>
                  </span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      {selectedId && <DeliveryPreview webhookId={selectedId} />}

      <CreateWebhookModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={() => qc.invalidateQueries({ queryKey: ["webhooks"] })}
      />

      <ConfirmDialog
        open={deleteId !== null}
        onOpenChange={(o) => {
          if (!o && !deleteMut.isPending) setDeleteId(null);
        }}
        title="Delete this webhook?"
        description="This permanently removes the webhook subscription and its delivery history."
        confirmLabel="Delete webhook"
        destructive
        loading={deleteMut.isPending}
        onConfirm={() => {
          if (deleteId) deleteMut.mutate(deleteId);
        }}
      />
    </section>
  );
}

function deliveryStatus(d: WebhookDelivery): {
  label: string;
  variant: BadgeVariant;
} {
  if (d.delivered) return { label: "Delivered", variant: "success" };
  if (d.status_code && d.status_code >= 400)
    return { label: `Failed · ${d.status_code}`, variant: "danger" };
  if (d.next_retry_at) return { label: "Retrying", variant: "warning" };
  return { label: "Pending", variant: "neutral" };
}

function DeliveryPreview({ webhookId }: { webhookId: string }) {
  const fmt = useFormatter();
  const delivQuery = useQuery({
    queryKey: ["webhook-deliveries", webhookId],
    queryFn: () => api.listWebhookDeliveries(webhookId, 20),
    refetchInterval: 10_000,
  });
  const rows = delivQuery.data?.deliveries ?? [];

  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-3">
        <CardTitle>Recent deliveries</CardTitle>
        <Button asChild size="sm" variant="outline">
          <Link to={`/admin/webhooks/${webhookId}/deliveries`}>
            <ScrollText className="h-4 w-4" />
            View full log
          </Link>
        </Button>
      </CardHeader>
      <CardContent>
        {delivQuery.isLoading ? (
          <AdminTableSkeleton
            columns={["When", "Event", "Attempt", "Status"]}
            rows={3}
          />
        ) : delivQuery.isError ? (
          <AdminErrorState
            title="Couldn't load deliveries"
            error={delivQuery.error}
            onRetry={() => delivQuery.refetch()}
          />
        ) : rows.length === 0 ? (
          <EmptyState
            icon={<ScrollText />}
            title="No deliveries yet"
            description="Attempts will appear here the next time a matching event fires."
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>When</TableHead>
                <TableHead>Event</TableHead>
                <TableHead className="text-end">Attempt</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Detail</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((d) => {
                const status = deliveryStatus(d);
                return (
                  <TableRow key={d.id}>
                    <TableCell className="whitespace-nowrap font-tabular text-fg-muted">
                      {fmt.dateTime(new Date(d.created_at))}
                    </TableCell>
                    <TableCell className="font-medium text-fg">
                      {humanizeToken(d.event_type)}
                    </TableCell>
                    <TableCell className="text-end font-tabular">
                      {fmt.number(d.attempt)}
                    </TableCell>
                    <TableCell>
                      <Badge variant={status.variant}>{status.label}</Badge>
                    </TableCell>
                    <TableCell className="max-w-[20rem]">
                      {d.error ? (
                        <span className="block truncate text-danger" title={d.error}>
                          {d.error}
                        </span>
                      ) : (
                        <span className="text-fg-subtle">—</span>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function CreateWebhookModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [url, setUrl] = useState("");
  const [secret, setSecret] = useState("");
  const [filters, setFilters] = useState("");
  const [conditions, setConditions] = useState("");
  const [maxRetries, setMaxRetries] = useState("5");
  const [backoffBase, setBackoffBase] = useState("10");
  const [submitted, setSubmitted] = useState(false);

  const reset = () => {
    setUrl("");
    setSecret("");
    setFilters("");
    setConditions("");
    setMaxRetries("5");
    setBackoffBase("10");
    setSubmitted(false);
  };

  const conditionsError = (() => {
    const trimmed = conditions.trim();
    if (!trimmed) return undefined;
    try {
      const parsed = JSON.parse(trimmed);
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed))
        return "Conditions must be a JSON object.";
      return undefined;
    } catch {
      return "Enter valid JSON, e.g. {\"ktype\":\"helpdesk.ticket\"}.";
    }
  })();

  const createMut = useMutation({
    mutationFn: () => {
      const trimmed = conditions.trim();
      const parsedConditions = trimmed
        ? (JSON.parse(trimmed) as Record<string, unknown>)
        : undefined;
      return api.createWebhook({
        url: url.trim(),
        secret: secret.trim(),
        event_filters: filters
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        conditions: parsedConditions,
        max_retries: Number(maxRetries) || 5,
        backoff_base_seconds: Number(backoffBase) || 10,
      });
    },
    onSuccess: () => {
      reset();
      onClose();
      onCreated();
    },
  });

  const urlError =
    submitted && !url.trim() ? "Enter the endpoint URL." : undefined;
  const secretError =
    submitted && !secret.trim() ? "Enter a signing secret." : undefined;

  return (
    <Modal
      open={open}
      onOpenChange={(next) => {
        if (!next && !createMut.isPending) {
          reset();
          onClose();
        }
      }}
    >
      <ModalContent className="max-w-xl">
        <ModalHeader>
          <ModalTitle>New webhook</ModalTitle>
          <ModalDescription>
            We'll POST a signed JSON payload to your endpoint whenever a
            matching event fires.
          </ModalDescription>
        </ModalHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            setSubmitted(true);
            if (!url.trim() || !secret.trim() || conditionsError) return;
            createMut.mutate();
          }}
        >
          <Field label="Endpoint URL" required error={urlError}>
            <Input
              type="url"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="https://example.com/hooks/kapp"
              autoFocus
            />
          </Field>
          <Field
            label="Signing secret"
            required
            error={secretError}
            help="We sign each payload with this secret so you can verify it came from us."
          >
            <Input
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
              placeholder="Shared HMAC secret"
            />
          </Field>
          <Field
            label="Events"
            help="Comma-separated. Leave empty to receive everything; a trailing * matches a prefix, e.g. krecord.*"
          >
            <Input
              value={filters}
              onChange={(e) => setFilters(e.target.value)}
              placeholder="krecord.*, workflow.completed"
            />
          </Field>
          <Field
            label="Delivery conditions"
            error={conditionsError}
            help="Advanced, optional. A JSON matcher against the event payload."
          >
            <Textarea
              value={conditions}
              onChange={(e) => setConditions(e.target.value)}
              rows={3}
              className="font-mono"
              placeholder='{"ktype":"helpdesk.ticket","data.status":{"$in":["open","pending"]}}'
            />
          </Field>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="Max retries" help="How many times to retry a failed delivery.">
              <Input
                type="number"
                min={1}
                max={20}
                value={maxRetries}
                onChange={(e) => setMaxRetries(e.target.value)}
              />
            </Field>
            <Field
              label="Backoff base (seconds)"
              help="Initial wait between retries; it grows on each attempt."
            >
              <Input
                type="number"
                min={1}
                value={backoffBase}
                onChange={(e) => setBackoffBase(e.target.value)}
              />
            </Field>
          </div>
          {createMut.error instanceof Error && (
            <p className="text-sm text-danger">{createMut.error.message}</p>
          )}
          <ModalFooter>
            <Button
              type="button"
              variant="ghost"
              onClick={() => {
                reset();
                onClose();
              }}
              disabled={createMut.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={createMut.isPending}>
              {createMut.isPending ? "Registering…" : "Register webhook"}
            </Button>
          </ModalFooter>
        </form>
      </ModalContent>
    </Modal>
  );
}
