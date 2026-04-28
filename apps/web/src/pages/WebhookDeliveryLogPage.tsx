import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import type { WebhookDelivery } from "@kapp/client";
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
        <div style={{ marginBottom: 16, fontSize: 13, color: "#475569" }}>
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

      <div style={{ marginBottom: 8, display: "flex", gap: 8 }}>
        {(["all", "delivered", "pending", "failed"] as const).map((f) => (
          <button
            key={f}
            onClick={() => setFilter(f)}
            style={{
              padding: "4px 10px",
              background: filter === f ? "#1d4ed8" : "#e5e7eb",
              color: filter === f ? "white" : "black",
              border: "none",
              borderRadius: 4,
            }}
          >
            {f}
          </button>
        ))}
        <span style={{ marginLeft: "auto", color: "#64748b", fontSize: 12 }}>
          {visible.length} of {grouped.length} events shown
        </span>
      </div>

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={th}>Event</th>
            <th style={th}>Type</th>
            <th style={th}>Attempts</th>
            <th style={th}>Last status</th>
            <th style={th}>Delivered</th>
            <th style={th}>Next retry</th>
            <th style={th}>Last error</th>
          </tr>
        </thead>
        <tbody>
          {visible.map((g) => (
            <EventGroupRow key={g.eventId} group={g} />
          ))}
          {visible.length === 0 && (
            <tr>
              <td colSpan={7} style={{ ...td, color: "#64748b" }}>
                No deliveries match the current filter.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </section>
  );
}

interface EventGroup {
  eventId: string;
  eventType: string;
  attempts: WebhookDelivery[];
  delivered: boolean;
  lastStatus?: number;
  lastError?: string;
  nextRetryAt?: string;
}

function groupByEvent(rows: WebhookDelivery[]): EventGroup[] {
  const map = new Map<string, EventGroup>();
  // The list comes newest-first; iterating in order means the
  // latest attempt is the first one we see per event_id, which is
  // the one we want to surface as "last status / next retry".
  for (const r of rows) {
    let g = map.get(r.event_id);
    if (!g) {
      g = {
        eventId: r.event_id,
        eventType: r.event_type,
        attempts: [],
        delivered: false,
      };
      map.set(r.event_id, g);
    }
    g.attempts.push(r);
    // The "last attempt" view is the row with the highest attempt
    // number, regardless of arrival order, so a lagged earlier
    // retry can't overwrite the canonical newest status.
    if (
      g.lastStatus === undefined ||
      r.attempt > (g.attempts[0]?.attempt ?? 0)
    ) {
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
      <tr style={{ cursor: "pointer" }} onClick={() => setExpanded((e) => !e)}>
        <td style={td}>
          <code style={{ fontSize: 11 }}>{group.eventId.slice(0, 8)}</code>
        </td>
        <td style={td}>{group.eventType}</td>
        <td style={td}>{group.attempts.length}</td>
        <td style={td}>{group.lastStatus ?? "-"}</td>
        <td style={td}>{group.delivered ? "yes" : "no"}</td>
        <td style={td}>
          {group.nextRetryAt
            ? new Date(group.nextRetryAt).toLocaleString()
            : "-"}
        </td>
        <td style={td}>{group.lastError ?? ""}</td>
      </tr>
      {expanded && (
        <tr>
          <td colSpan={7} style={{ ...td, background: "#f8fafc" }}>
            <table style={{ width: "100%", borderCollapse: "collapse" }}>
              <thead>
                <tr>
                  <th style={th}>Attempt</th>
                  <th style={th}>When</th>
                  <th style={th}>Status</th>
                  <th style={th}>Delivered</th>
                  <th style={th}>Next retry</th>
                  <th style={th}>Error</th>
                  <th style={th}>Response</th>
                </tr>
              </thead>
              <tbody>
                {group.attempts.map((a) => (
                  <tr key={a.id}>
                    <td style={td}>{a.attempt}</td>
                    <td style={td}>
                      {new Date(a.created_at).toLocaleString()}
                    </td>
                    <td style={td}>{a.status_code ?? "-"}</td>
                    <td style={td}>{a.delivered ? "yes" : "no"}</td>
                    <td style={td}>
                      {a.next_retry_at
                        ? new Date(a.next_retry_at).toLocaleString()
                        : "-"}
                    </td>
                    <td style={td}>{a.error ?? ""}</td>
                    <td style={td}>
                      <code style={{ fontSize: 11 }}>
                        {(a.response_body ?? "").slice(0, 120)}
                      </code>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </td>
        </tr>
      )}
    </>
  );
}

const th: React.CSSProperties = {
  textAlign: "left",
  borderBottom: "1px solid #e5e7eb",
  padding: "4px 6px",
  fontSize: 12,
};
const td: React.CSSProperties = {
  padding: "4px 6px",
  borderBottom: "1px solid #f3f4f6",
  fontSize: 13,
};
