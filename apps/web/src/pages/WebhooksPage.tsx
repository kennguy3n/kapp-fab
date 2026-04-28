import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { Webhook } from "@kapp/client";
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
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["webhooks"] });
      setSelectedId(null);
    },
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
        style={{ marginBottom: 24, display: "grid", gap: 8, maxWidth: 520 }}
      >
        <label>
          URL
          <input
            type="url"
            required
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/hooks/kapp"
            style={{ width: "100%", padding: 6 }}
          />
        </label>
        <label>
          Signing secret
          <input
            type="text"
            required
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            placeholder="shared HMAC secret"
            style={{ width: "100%", padding: 6 }}
          />
        </label>
        <label>
          Event filters (comma-separated, trailing * = prefix)
          <input
            type="text"
            value={filters}
            onChange={(e) => setFilters(e.target.value)}
            placeholder="krecord.*, workflow.completed"
            style={{ width: "100%", padding: 6 }}
          />
        </label>
        <label>
          Conditions (JSON; matches against event payload — see docs)
          <textarea
            value={conditions}
            onChange={(e) => setConditions(e.target.value)}
            placeholder='{"ktype":"helpdesk.ticket","data.status":{"$in":["open","pending"]}}'
            style={{ width: "100%", padding: 6, fontFamily: "monospace", minHeight: 64 }}
          />
        </label>
        <div style={{ display: "flex", gap: 12 }}>
          <label style={{ flex: 1 }}>
            Max retries
            <input
              type="number"
              min={1}
              max={20}
              value={maxRetries}
              onChange={(e) => setMaxRetries(parseInt(e.target.value, 10) || 5)}
              style={{ width: "100%", padding: 6 }}
            />
          </label>
          <label style={{ flex: 1 }}>
            Backoff base (seconds)
            <input
              type="number"
              min={1}
              value={backoffBase}
              onChange={(e) => setBackoffBase(parseInt(e.target.value, 10) || 10)}
              style={{ width: "100%", padding: 6 }}
            />
          </label>
        </div>
        <button type="submit" disabled={createMut.isPending}>
          Register webhook
        </button>
        {createMut.error instanceof Error && (
          <div style={{ color: "#b91c1c", fontSize: 12 }}>
            {createMut.error.message}
          </div>
        )}
      </form>

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr>
            <th style={th}>URL</th>
            <th style={th}>Filters</th>
            <th style={th}>Active</th>
            <th style={th}>Created</th>
            <th style={th}></th>
          </tr>
        </thead>
        <tbody>
          {hooks.map((h) => (
            <tr
              key={h.id}
              onClick={() => setSelectedId(h.id)}
              style={{
                cursor: "pointer",
                background: h.id === selectedId ? "#eef2ff" : undefined,
              }}
            >
              <td style={td}>{h.url}</td>
              <td style={td}>
                {(h.event_filters ?? []).join(", ") || <em>all</em>}
              </td>
              <td style={td}>
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    toggleMut.mutate(h);
                  }}
                >
                  {h.active ? "on" : "off"}
                </button>
              </td>
              <td style={td}>{new Date(h.created_at).toLocaleString()}</td>
              <td style={td}>
                <Link
                  to={`/admin/webhooks/${h.id}/deliveries`}
                  onClick={(e) => e.stopPropagation()}
                >
                  log
                </Link>{" "}
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    if (window.confirm("Delete webhook?")) {
                      deleteMut.mutate(h.id);
                    }
                  }}
                >
                  delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {selectedId && <DeliveryLog webhookId={selectedId} />}
    </section>
  );
}

const th: React.CSSProperties = {
  textAlign: "left",
  borderBottom: "1px solid #e5e7eb",
  padding: "4px 6px",
};
const td: React.CSSProperties = {
  padding: "4px 6px",
  borderBottom: "1px solid #f3f4f6",
};

function DeliveryLog({ webhookId }: { webhookId: string }) {
  const delivQuery = useQuery({
    queryKey: ["webhook-deliveries", webhookId],
    queryFn: () => api.listWebhookDeliveries(webhookId, 100),
    refetchInterval: 10_000,
  });
  const rows = delivQuery.data?.deliveries ?? [];
  return (
    <div style={{ marginTop: 24 }}>
      <h2 style={{ fontSize: 16 }}>Delivery log</h2>
      {rows.length === 0 && <div>No deliveries yet.</div>}
      {rows.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr>
              <th style={th}>When</th>
              <th style={th}>Event</th>
              <th style={th}>Attempt</th>
              <th style={th}>Status</th>
              <th style={th}>Delivered</th>
              <th style={th}>Error</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((d) => (
              <tr key={d.id}>
                <td style={td}>{new Date(d.created_at).toLocaleString()}</td>
                <td style={td}>{d.event_type}</td>
                <td style={td}>{d.attempt}</td>
                <td style={td}>{d.status_code ?? "-"}</td>
                <td style={td}>{d.delivered ? "yes" : "no"}</td>
                <td style={td}>{d.error ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
