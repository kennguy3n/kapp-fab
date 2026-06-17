import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  ArrowLeft,
  ChevronDown,
  ChevronRight,
  RefreshCw,
  ScrollText,
} from "lucide-react";
import type { WebhookDelivery } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken } from "../lib/ktypeView";
import {
  AdminErrorState,
  AdminPageHeader,
  AdminTableSkeleton,
  CopyableId,
} from "./adminKit";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

type FilterKey = "all" | "delivered" | "pending" | "failed";

const FILTERS: { key: FilterKey; label: string }[] = [
  { key: "all", label: "All" },
  { key: "delivered", label: "Delivered" },
  { key: "pending", label: "Retrying" },
  { key: "failed", label: "Failed" },
];

// WebhookDeliveryLogPage is the per-webhook delivery audit surface.
// It groups attempts by event_id so the operator can see the full
// retry chain for a single event in one row, and surfaces the
// next_retry_at column directly so a stuck delivery is visible
// without drilling into the row. Polled every 10s so the page
// reflects in-flight retries without a manual refresh.
export function WebhookDeliveryLogPage() {
  const { id } = useParams<{ id: string }>();
  const fmt = useFormatter();
  const [filter, setFilter] = useState<FilterKey>("all");
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

  const grouped = useMemo(
    () => groupByEvent(deliveriesQuery.data?.deliveries ?? []),
    [deliveriesQuery.data],
  );

  const visible = useMemo(() => {
    return grouped.filter((g) => {
      if (filter === "all") return true;
      if (filter === "delivered") return g.delivered;
      if (filter === "failed") return !g.delivered && !g.nextRetryAt;
      return !g.delivered && !!g.nextRetryAt;
    });
  }, [grouped, filter]);

  if (!id) {
    return (
      <section className="flex flex-col gap-6">
        <AdminPageHeader area="Platform" title="Delivery log" />
        <EmptyState
          icon={<ScrollText />}
          title="No webhook selected"
          description="Open a webhook's delivery log from the webhooks list."
          action={
            <Button asChild variant="secondary">
              <Link to="/admin/webhooks">Back to webhooks</Link>
            </Button>
          }
        />
      </section>
    );
  }

  const hook = hookQuery.data;

  return (
    <section className="flex flex-col gap-6">
      <AdminPageHeader
        area="Platform"
        title="Delivery log"
        description={
          hook ? (
            <span className="flex flex-wrap items-center gap-2">
              <span className="truncate font-medium text-fg" title={hook.url}>
                {hook.url}
              </span>
              <Badge variant={hook.active ? "success" : "neutral"}>
                {hook.active ? "Active" : "Paused"}
              </Badge>
              <span className="text-fg-muted">
                Up to {fmt.number(hook.max_retries)} retries · backoff{" "}
                {fmt.number(hook.backoff_base_seconds)}s
              </span>
            </span>
          ) : (
            "Every delivery attempt for this endpoint, grouped by event."
          )
        }
        actions={
          <>
            <Button asChild variant="ghost">
              <Link to="/admin/webhooks">
                <ArrowLeft className="h-4 w-4" />
                Webhooks
              </Link>
            </Button>
            <Button
              variant="outline"
              leadingIcon={<RefreshCw />}
              onClick={() => deliveriesQuery.refetch()}
              disabled={deliveriesQuery.isFetching}
            >
              Refresh
            </Button>
          </>
        }
      />

      <div className="flex flex-wrap items-center gap-2">
        {FILTERS.map((f) => (
          <Button
            key={f.key}
            size="sm"
            variant={filter === f.key ? "primary" : "secondary"}
            onClick={() => setFilter(f.key)}
          >
            {f.label}
          </Button>
        ))}
        {grouped.length > 0 && (
          <span className="ml-auto text-sm text-fg-muted">
            Showing {fmt.number(visible.length)} of {fmt.number(grouped.length)}{" "}
            events
          </span>
        )}
      </div>

      {deliveriesQuery.isLoading ? (
        <AdminTableSkeleton
          columns={["Event", "Attempts", "Status", "Next retry", "Last error"]}
        />
      ) : deliveriesQuery.isError ? (
        <AdminErrorState
          title="Couldn't load deliveries"
          error={deliveriesQuery.error}
          onRetry={() => deliveriesQuery.refetch()}
        />
      ) : grouped.length === 0 ? (
        <EmptyState
          icon={<ScrollText />}
          title="No deliveries yet"
          description="Attempts will appear here the next time a matching event fires for this endpoint."
        />
      ) : visible.length === 0 ? (
        <EmptyState
          icon={<ScrollText />}
          title="No events match this filter"
          description="Try a different status filter to see more deliveries."
          action={
            <Button variant="secondary" size="sm" onClick={() => setFilter("all")}>
              Show all
            </Button>
          }
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-8" />
              <TableHead>Event</TableHead>
              <TableHead className="text-end">Attempts</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Next retry</TableHead>
              <TableHead>Last error</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {visible.map((g) => (
              <EventGroupRow key={g.eventId} group={g} fmt={fmt} />
            ))}
          </TableBody>
        </Table>
      )}
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

function groupStatus(group: EventGroup): {
  label: string;
  variant: BadgeVariant;
} {
  if (group.delivered) return { label: "Delivered", variant: "success" };
  if (group.nextRetryAt) return { label: "Retrying", variant: "warning" };
  if (group.lastStatus && group.lastStatus >= 400)
    return { label: `Failed · ${group.lastStatus}`, variant: "danger" };
  return { label: "Failed", variant: "danger" };
}

function attemptStatus(a: WebhookDelivery): {
  label: string;
  variant: BadgeVariant;
} {
  if (a.delivered)
    return {
      label: a.status_code ? `Delivered · ${a.status_code}` : "Delivered",
      variant: "success",
    };
  if (a.status_code && a.status_code >= 400)
    return { label: `Failed · ${a.status_code}`, variant: "danger" };
  if (a.next_retry_at) return { label: "Retrying", variant: "warning" };
  return { label: "Pending", variant: "neutral" };
}

function EventGroupRow({
  group,
  fmt,
}: {
  group: EventGroup;
  fmt: ReturnType<typeof useFormatter>;
}) {
  const [expanded, setExpanded] = useState(false);
  const status = groupStatus(group);
  return (
    <>
      <TableRow className="cursor-pointer" onClick={() => setExpanded((e) => !e)}>
        <TableCell>
          <button
            type="button"
            aria-label={expanded ? "Collapse attempts" : "Expand attempts"}
            aria-expanded={expanded}
            onClick={(e) => {
              e.stopPropagation();
              setExpanded((v) => !v);
            }}
            className="inline-flex h-6 w-6 items-center justify-center rounded-md text-fg-muted hover:bg-bg-muted hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
          >
            {expanded ? (
              <ChevronDown className="h-4 w-4" aria-hidden="true" />
            ) : (
              <ChevronRight className="h-4 w-4" aria-hidden="true" />
            )}
          </button>
        </TableCell>
        <TableCell>
          <div className="font-medium text-fg">
            {humanizeToken(group.eventType)}
          </div>
          <span onClick={(e) => e.stopPropagation()}>
            <CopyableId value={group.eventId} label="event" />
          </span>
        </TableCell>
        <TableCell className="text-end font-tabular">
          {fmt.number(group.attempts.length)}
        </TableCell>
        <TableCell>
          <Badge variant={status.variant}>{status.label}</Badge>
        </TableCell>
        <TableCell className="whitespace-nowrap font-tabular text-fg-muted">
          {group.nextRetryAt ? fmt.dateTime(new Date(group.nextRetryAt)) : "—"}
        </TableCell>
        <TableCell className="max-w-[18rem]">
          {group.lastError ? (
            <span className="block truncate text-danger" title={group.lastError}>
              {group.lastError}
            </span>
          ) : (
            <span className="text-fg-subtle">—</span>
          )}
        </TableCell>
      </TableRow>
      {expanded && (
        <TableRow>
          <TableCell colSpan={6} className="bg-bg-subtle">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="text-end">Attempt</TableHead>
                  <TableHead>When</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Next retry</TableHead>
                  <TableHead>Error</TableHead>
                  <TableHead>Response</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {[...group.attempts]
                  .sort((a, b) => a.attempt - b.attempt)
                  .map((a) => {
                    const st = attemptStatus(a);
                    return (
                      <TableRow key={a.id}>
                        <TableCell className="text-end font-tabular">
                          {fmt.number(a.attempt)}
                        </TableCell>
                        <TableCell className="whitespace-nowrap font-tabular text-fg-muted">
                          {fmt.dateTime(new Date(a.created_at))}
                        </TableCell>
                        <TableCell>
                          <Badge variant={st.variant}>{st.label}</Badge>
                        </TableCell>
                        <TableCell className="whitespace-nowrap font-tabular text-fg-muted">
                          {a.next_retry_at
                            ? fmt.dateTime(new Date(a.next_retry_at))
                            : "—"}
                        </TableCell>
                        <TableCell className="max-w-[16rem]">
                          {a.error ? (
                            <span
                              className="block truncate text-danger"
                              title={a.error}
                            >
                              {a.error}
                            </span>
                          ) : (
                            <span className="text-fg-subtle">—</span>
                          )}
                        </TableCell>
                        <TableCell className="max-w-[20rem]">
                          {a.response_body ? (
                            <span
                              className="block truncate font-mono text-xs text-fg-muted"
                              title={a.response_body}
                            >
                              {a.response_body}
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
          </TableCell>
        </TableRow>
      )}
    </>
  );
}
