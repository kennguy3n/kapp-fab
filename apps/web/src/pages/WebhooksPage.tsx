import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { Webhook } from "@kapp/client";
import {
  Button,
  ConfirmDialog,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

// WebhooksPage is the tenant admin surface for outbound webhook
// subscriptions. It renders the CRUD form + the delivery log table
// for the currently-selected row so operators can audit failed
// attempts without hopping between screens.
export function WebhooksPage() {
  const qc = useQueryClient();
  const hooksQuery = useQuery({
    queryKey: ["webhooks"],
    queryFn: () => api.listWebhooks(),
  });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [deleteId, setDeleteId] = useState<string | null>(null);

  const [url, setUrl] = useState("");
  const [secret, setSecret] = useState("");
  const [filters, setFilters] = useState("");
  const [conditions, setConditions] = useState("");
  const [maxRetries, setMaxRetries] = useState<number>(5);
  const [backoffBase, setBackoffBase] = useState<number>(10);

  const createMut = useMutation({
    mutationFn: () => {
      let parsedConditions: Record<string, unknown> | undefined;
      const trimmed = conditions.trim();
      if (trimmed) {
        try {
          parsedConditions = JSON.parse(trimmed) as Record<string, unknown>;
        } catch {
          throw new Error(
            "conditions must be valid JSON (object, e.g. {\"ktype\":\"helpdesk.ticket\"})"
          );
        }
      }
      return api.createWebhook({
        url,
        secret,
        event_filters: filters
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        conditions: parsedConditions,
        max_retries: maxRetries,
        backoff_base_seconds: backoffBase,
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["webhooks"] });
      setUrl("");
      setSecret("");
      setFilters("");
      setConditions("");
      setMaxRetries(5);
      setBackoffBase(10);
    },
  });

  const toggleMut = useMutation({
    mutationFn: async (w: Webhook) =>
      api.updateWebhook(w.id, { active: !w.active }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["webhooks"] }),
  });

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.deleteWebhook(id),
    // Keep the confirm dialog open (showing its `loading` state) until
    // the delete settles, then close it — matching the await-mutation
    // pattern used by RecordListPage so destructive actions give
    // consistent "Working…" feedback across the app.
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["webhooks"] });
      setSelectedId(null);
      setDeleteId(null);
    },
    onError: () => setDeleteId(null),
  });

  const hooks = hooksQuery.data?.webhooks ?? [];

  return (
    <section>
      <h1>Webhooks</h1>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!url || !secret) return;
          createMut.mutate();
        }}
        className="mb-6 grid max-w-[520px] gap-2"
      >
        <label className="grid gap-1 text-sm">
          URL
          <Input
            type="url"
            required
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/hooks/kapp"
          />
        </label>
        <label className="grid gap-1 text-sm">
          Signing secret
          <Input
            type="text"
            required
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            placeholder="shared HMAC secret"
          />
        </label>
        <label className="grid gap-1 text-sm">
          Event filters (comma-separated, trailing * = prefix)
          <Input
            type="text"
            value={filters}
            onChange={(e) => setFilters(e.target.value)}
            placeholder="krecord.*, workflow.completed"
          />
        </label>
        <label className="grid gap-1 text-sm">
          Conditions (JSON; matches against event payload — see docs)
          <textarea
            value={conditions}
            onChange={(e) => setConditions(e.target.value)}
            placeholder='{"ktype":"helpdesk.ticket","data.status":{"$in":["open","pending"]}}'
            className="min-h-16 w-full rounded-md border border-border bg-bg-elevated p-1.5 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
          />
        </label>
        <div className="flex gap-3">
          <label className="grid flex-1 gap-1 text-sm">
            Max retries
            <Input
              type="number"
              min={1}
              max={20}
              value={maxRetries}
              onChange={(e) => setMaxRetries(parseInt(e.target.value, 10) || 5)}
            />
          </label>
          <label className="grid flex-1 gap-1 text-sm">
            Backoff base (seconds)
            <Input
              type="number"
              min={1}
              value={backoffBase}
              onChange={(e) => setBackoffBase(parseInt(e.target.value, 10) || 10)}
            />
          </label>
        </div>
        <div>
          <Button type="submit" disabled={createMut.isPending}>
            Register webhook
          </Button>
        </div>
        {createMut.error instanceof Error && (
          <div className="text-xs text-danger">{createMut.error.message}</div>
        )}
      </form>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>URL</TableHead>
            <TableHead>Filters</TableHead>
            <TableHead>Active</TableHead>
            <TableHead>Created</TableHead>
            <TableHead />
          </TableRow>
        </TableHeader>
        <TableBody>
          {hooks.map((h) => (
            <TableRow
              key={h.id}
              onClick={() => setSelectedId(h.id)}
              className={
                h.id === selectedId ? "cursor-pointer bg-bg-muted" : "cursor-pointer"
              }
            >
              <TableCell>{h.url}</TableCell>
              <TableCell>
                {(h.event_filters ?? []).join(", ") || <em>all</em>}
              </TableCell>
              <TableCell>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={(e) => {
                    e.stopPropagation();
                    toggleMut.mutate(h);
                  }}
                >
                  {h.active ? "on" : "off"}
                </Button>
              </TableCell>
              <TableCell>{new Date(h.created_at).toLocaleString()}</TableCell>
              <TableCell>
                <Link
                  to={`/admin/webhooks/${h.id}/deliveries`}
                  onClick={(e) => e.stopPropagation()}
                >
                  log
                </Link>{" "}
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={(e) => {
                    e.stopPropagation();
                    setDeleteId(h.id);
                  }}
                >
                  delete
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      {selectedId && <DeliveryLog webhookId={selectedId} />}

      <ConfirmDialog
        open={deleteId !== null}
        onOpenChange={(o) => {
          if (!o && !deleteMut.isPending) setDeleteId(null);
        }}
        title="Delete webhook?"
        description="This permanently removes the webhook subscription and its delivery history."
        confirmLabel="Delete"
        destructive
        loading={deleteMut.isPending}
        onConfirm={() => {
          if (deleteId) deleteMut.mutate(deleteId);
        }}
      />
    </section>
  );
}

function DeliveryLog({ webhookId }: { webhookId: string }) {
  const delivQuery = useQuery({
    queryKey: ["webhook-deliveries", webhookId],
    queryFn: () => api.listWebhookDeliveries(webhookId, 100),
    refetchInterval: 10_000,
  });
  const rows = delivQuery.data?.deliveries ?? [];
  return (
    <div className="mt-6">
      <h2 className="text-base">Delivery log</h2>
      {rows.length === 0 && <div>No deliveries yet.</div>}
      {rows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>When</TableHead>
              <TableHead>Event</TableHead>
              <TableHead>Attempt</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Delivered</TableHead>
              <TableHead>Error</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((d) => (
              <TableRow key={d.id}>
                <TableCell>{new Date(d.created_at).toLocaleString()}</TableCell>
                <TableCell>{d.event_type}</TableCell>
                <TableCell>{d.attempt}</TableCell>
                <TableCell>{d.status_code ?? "-"}</TableCell>
                <TableCell>{d.delivered ? "yes" : "no"}</TableCell>
                <TableCell>{d.error ?? ""}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
