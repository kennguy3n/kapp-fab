import { useQuery } from "@tanstack/react-query";

// StatusPage is the PUBLIC, unauthenticated status page backed by
// GET /api/v1/health. It deliberately talks to the endpoint with a
// bare fetch (no tenant/auth headers) rather than the shared
// ApiClient: the page is mounted outside the authenticated app shell
// and must render for anonymous visitors, so it cannot depend on a
// tenant id or bearer token being present in localStorage.

type HealthStatus = "operational" | "degraded" | "down";

interface PublicComponent {
  name: string;
  status: HealthStatus;
  latency_ms: number;
}

interface PublicIncident {
  summary: string;
  at: string;
}

interface PublicHealth {
  status: HealthStatus;
  component_availability_percent: number;
  components: PublicComponent[];
  incidents: PublicIncident[];
  checked_at: string;
}

async function fetchPublicHealth(): Promise<PublicHealth> {
  const res = await fetch("/api/v1/health");
  if (!res.ok) {
    throw new Error(`health request failed: ${res.status}`);
  }
  return (await res.json()) as PublicHealth;
}

// STATUS_META centralises the human label + colour for each status so
// the banner, the per-component pills, and any future surface stay
// visually consistent.
const STATUS_META: Record<HealthStatus, { label: string; color: string; bg: string }> = {
  operational: { label: "Operational", color: "#065f46", bg: "#d1fae5" },
  degraded: { label: "Degraded", color: "#92400e", bg: "#fef3c7" },
  down: { label: "Down", color: "#991b1b", bg: "#fee2e2" },
};

// COMPONENT_LABELS maps the public API's generic component names onto
// display strings. The public /api/v1/health endpoint deliberately
// emits technology-agnostic names (database, cache, …) rather than the
// internal probe names (postgres, redis, …) so a scrape can't
// fingerprint the stack; these keys mirror that contract. Unknown
// names fall back to the raw key so a newly added component still
// renders.
const COMPONENT_LABELS: Record<string, string> = {
  database: "Database",
  cache: "Cache",
  event_bus: "Event Bus",
  object_storage: "Object Storage",
  event_delivery: "Event Delivery",
  background_jobs: "Background Jobs",
  service: "Service",
};

function StatusPill({ status }: { status: HealthStatus }) {
  const meta = STATUS_META[status];
  return (
    <span
      style={{
        display: "inline-block",
        padding: "2px 10px",
        borderRadius: 999,
        fontSize: 12,
        fontWeight: 600,
        color: meta.color,
        background: meta.bg,
      }}
    >
      {meta.label}
    </span>
  );
}

export function StatusPage() {
  const healthQuery = useQuery({
    queryKey: ["public-health"],
    queryFn: fetchPublicHealth,
    // Poll so a visitor parked on the page sees recovery without a
    // manual refresh; cheap because the endpoint is time-bounded.
    refetchInterval: 30_000,
  });

  if (healthQuery.isLoading) {
    return <div style={{ padding: 32 }}>Loading status…</div>;
  }
  if (healthQuery.error || !healthQuery.data) {
    return (
      <div style={{ padding: 32, color: "#991b1b" }}>
        Unable to load platform status. Please try again shortly.
      </div>
    );
  }

  const data = healthQuery.data;
  const meta = STATUS_META[data.status];

  return (
    <div style={{ maxWidth: 720, margin: "0 auto", padding: "32px 16px" }}>
      <h1 style={{ fontSize: 24, marginBottom: 4 }}>Platform Status</h1>
      <p style={{ color: "#6b7280", fontSize: 13, marginTop: 0 }}>
        Last checked {new Date(data.checked_at).toLocaleString()}
      </p>

      <div
        style={{
          marginTop: 16,
          padding: 20,
          borderRadius: 10,
          background: meta.bg,
          color: meta.color,
        }}
      >
        <div style={{ fontSize: 18, fontWeight: 700 }}>
          {data.status === "operational"
            ? "All systems operational"
            : data.status === "degraded"
              ? "Some systems degraded"
              : "Major outage"}
        </div>
        <div style={{ fontSize: 13, marginTop: 4 }}>
          {data.component_availability_percent.toFixed(0)}% of components
          operational
        </div>
      </div>

      <section style={{ marginTop: 32 }}>
        <h2 style={{ fontSize: 16 }}>Components</h2>
        <div style={{ marginTop: 8 }}>
          {data.components.map((c, i) => (
            <div
              // Index-suffixed: the public API collapses any unmapped
              // component to the generic "service" label, so names are
              // not guaranteed unique — keying on name alone could
              // collide if two unmapped probes ever co-exist.
              key={`${c.name}-${i}`}
              style={{
                display: "flex",
                alignItems: "center",
                justifyContent: "space-between",
                padding: "10px 0",
                borderBottom: "1px solid #f3f4f6",
              }}
            >
              <span style={{ fontWeight: 500 }}>
                {COMPONENT_LABELS[c.name] ?? c.name}
              </span>
              <span style={{ display: "flex", alignItems: "center", gap: 12 }}>
                <span
                  style={{
                    color: "#9ca3af",
                    fontSize: 12,
                    fontVariantNumeric: "tabular-nums",
                  }}
                >
                  {c.latency_ms.toFixed(0)} ms
                </span>
                <StatusPill status={c.status} />
              </span>
            </div>
          ))}
        </div>
      </section>

      <section style={{ marginTop: 32 }}>
        <h2 style={{ fontSize: 16 }}>Recent capacity changes</h2>
        {data.incidents.length === 0 ? (
          <p style={{ color: "#6b7280", fontSize: 13 }}>
            No capacity changes in the recent window.
          </p>
        ) : (
          <ul style={{ listStyle: "none", padding: 0, marginTop: 8 }}>
            {data.incidents.map((incident, i) => (
              <li
                key={`${incident.at}-${i}`}
                style={{
                  padding: "8px 0",
                  borderBottom: "1px solid #f3f4f6",
                  fontSize: 13,
                }}
              >
                <span style={{ color: "#6b7280", marginRight: 8 }}>
                  {new Date(incident.at).toLocaleString()}
                </span>
                {incident.summary}
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
