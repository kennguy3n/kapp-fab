import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import type { WebhookDelivery } from "@kapp/client";
import {
  Button,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

// WebhookDeliveryLogPage is the per-webhook delivery audit surface.
// It groups attempts by event_id so the operator can see the full
// retry chain for a single event in one row, and surfaces the
// next_retry_at column directly so a stuck delivery is visible
// without drilling into the row. Polled every 10s so the page
// reflects in-flight retries without a manual refresh.
export function WebhookDeliveryLogPage() {
  const { id } = useParams<{ id: string }>();
  const [filter, setFilter] = useState<"all" | "failed" | "pending" | "delivered">("all");
  const limit = 200;

  const hookQuery = useQuery({
    queryKey: ["webhook", id],
    queryFn: () => api.getWebhook(id!),
    enabled: !!id,
  });

  const deliveriesQuery = useQuery({
    queryKey: ["webhook-deliveries", id, "long"],
    queryFn: () => api.listWebhookDeliveries(id!, limit),
    enabled: !!id,
    refetchInterval: 10_000,
  });

  const grouped = useMemo(() => {
    const rows = deliveriesQuery.data?.deliveries ?? [];
    return groupByEvent(rows);
  }, [deliveriesQuery.data]);

  const visible = useMemo(() => {
    return grouped.filter((g) => {
      if (filter === "all") return true;
      if (filter === "delivered") return g.delivered;
      if (filter === "failed") return !g.delivered && !g.nextRetryAt;
      // pending: not delivered, has a next retry scheduled
      return !g.delivered && !!g.nextRetryAt;
    });
  }, [grouped, filter]);

  if (!id) {
    return <section>No webhook selected.</section>;
  }

  return (
    <section>
      <h1>Webhook delivery log</h1>
      {hookQuery.data && (
        <div className="mb-4 text-[13px] text-fg-muted">
          <div>
            <strong>{hookQuery.data.url}</strong>{" "}
            {hookQuery.data.active ? "(active)" : "(disabled)"}
          </div>
          <div>
            max retries: {hookQuery.data.max_retries} · backoff base:{" "}
            {hookQuery.data.backoff_base_seconds}s
          </div>
        </div>
      )}

      <div className="mb-2 flex items-center gap-2">
        {(["all", "delivered", "pending", "failed"] as const).map((f) => (
          <Button
            key={f}
            size="sm"
            variant={filter === f ? "primary" : "secondary"}
            onClick={() => setFilter(f)}
          >
            {f}
          </Button>
        ))}
        <span className="ml-auto text-xs text-fg-muted">
          {visible.length} of {grouped.length} events shown
        </span>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Event</TableHead>
            <TableHead>Type</TableHead>
            <TableHead>Attempts</TableHead>
            <TableHead>Last status</TableHead>
            <TableHead>Delivered</TableHead>
            <TableHead>Next retry</TableHead>
            <TableHead>Last error</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {visible.map((g) => (
            <EventGroupRow key={g.eventId} group={g} />
          ))}
          {visible.length === 0 && (
            <TableRow>
              <TableCell colSpan={7} className="text-fg-muted">
                No deliveries match the current filter.
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </section>
  );
}

export interface EventGroup {
  eventId: string;
  eventType: string;
  attempts: WebhookDelivery[];
  delivered: boolean;
  // _maxAttempt tracks the highest attempt number observed for this
  // event so an out-of-order earlier retry can never overwrite the
  // canonical newest status.
  _maxAttempt: number;
  lastStatus?: number;
  lastError?: string;
  nextRetryAt?: string;
}

export function groupByEvent(rows: WebhookDelivery[]): EventGroup[] {
  const map = new Map<string, EventGroup>();
  // The list usually arrives newest-first, but the worker can write
  // a lagged retry after a higher-numbered attempt has already been
  // recorded, so we cannot rely on arrival order. Track _maxAttempt
  // per group and only overwrite the surfaced "last status / next
  // retry" when we see a strictly higher attempt.
  for (const r of rows) {
    let g = map.get(r.event_id);
    if (!g) {
      g = {
        eventId: r.event_id,
        eventType: r.event_type,
        attempts: [],
        delivered: false,
        _maxAttempt: 0,
      };
      map.set(r.event_id, g);
    }
    g.attempts.push(r);
    if (r.attempt > g._maxAttempt) {
      g._maxAttempt = r.attempt;
      g.lastStatus = r.status_code ?? undefined;
      g.lastError = r.error ?? undefined;
      g.nextRetryAt = r.next_retry_at ?? undefined;
    }
    if (r.delivered) g.delivered = true;
  }
  return Array.from(map.values());
}

function EventGroupRow({ group }: { group: EventGroup }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <>
      <TableRow className="cursor-pointer" onClick={() => setExpanded((e) => !e)}>
        <TableCell>
          <code className="text-[11px]">{group.eventId.slice(0, 8)}</code>
        </TableCell>
        <TableCell>{group.eventType}</TableCell>
        <TableCell>{group.attempts.length}</TableCell>
        <TableCell>{group.lastStatus ?? "-"}</TableCell>
        <TableCell>{group.delivered ? "yes" : "no"}</TableCell>
        <TableCell>
          {group.nextRetryAt
            ? new Date(group.nextRetryAt).toLocaleString()
            : "-"}
        </TableCell>
        <TableCell>{group.lastError ?? ""}</TableCell>
      </TableRow>
      {expanded && (
        <TableRow>
          <TableCell colSpan={7} className="bg-bg-subtle">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Attempt</TableHead>
                  <TableHead>When</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Delivered</TableHead>
                  <TableHead>Next retry</TableHead>
                  <TableHead>Error</TableHead>
                  <TableHead>Response</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {group.attempts.map((a) => (
                  <TableRow key={a.id}>
                    <TableCell>{a.attempt}</TableCell>
                    <TableCell>
                      {new Date(a.created_at).toLocaleString()}
                    </TableCell>
                    <TableCell>{a.status_code ?? "-"}</TableCell>
                    <TableCell>{a.delivered ? "yes" : "no"}</TableCell>
                    <TableCell>
                      {a.next_retry_at
                        ? new Date(a.next_retry_at).toLocaleString()
                        : "-"}
                    </TableCell>
                    <TableCell>{a.error ?? ""}</TableCell>
                    <TableCell>
                      <code className="text-[11px]">
                        {(a.response_body ?? "").slice(0, 120)}
                      </code>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableCell>
        </TableRow>
      )}
    </>
  );
};
