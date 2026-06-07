import { useQuery } from "@tanstack/react-query";
import { Badge } from "@kapp/ui";

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

// STATUS_META centralises the human label + design-system colour
// mapping for each status so the banner, the per-component pills, and
// any future surface stay visually consistent. `badge` selects the
// Badge variant; `banner` is the Tailwind token class pair for the
// large headline banner.
const STATUS_META: Record<
  HealthStatus,
  { label: string; badge: "success" | "warning" | "danger"; banner: string }
> = {
  operational: {
    label: "Operational",
    badge: "success",
    banner: "bg-success text-success-fg",
  },
  degraded: {
    label: "Degraded",
    badge: "warning",
    banner: "bg-warning text-warning-fg",
  },
  down: { label: "Down", badge: "danger", banner: "bg-danger text-danger-fg" },
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
  return <Badge variant={meta.badge}>{meta.label}</Badge>;
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
    return <div className="p-8">Loading status…</div>;
  }
  if (healthQuery.error || !healthQuery.data) {
    return (
      <div className="p-8 text-danger">
        Unable to load platform status. Please try again shortly.
      </div>
    );
  }

  const data = healthQuery.data;
  const meta = STATUS_META[data.status];

  return (
    <div className="mx-auto max-w-[720px] px-4 py-8">
      <h1 className="mb-1 text-2xl">Platform Status</h1>
      <p className="mt-0 text-[13px] text-fg-muted">
        Last checked {new Date(data.checked_at).toLocaleString()}
      </p>

      <div className={`mt-4 rounded-[10px] p-5 ${meta.banner}`}>
        <div className="text-lg font-bold">
          {data.status === "operational"
            ? "All systems operational"
            : data.status === "degraded"
              ? "Some systems degraded"
              : "Major outage"}
        </div>
        <div className="mt-1 text-[13px]">
          {data.component_availability_percent.toFixed(0)}% of components
          operational
        </div>
      </div>

      <section className="mt-8">
        <h2 className="text-base">Components</h2>
        <div className="mt-2">
          {data.components.map((c, i) => (
            <div
              // Index-suffixed: the public API collapses any unmapped
              // component to the generic "service" label, so names are
              // not guaranteed unique — keying on name alone could
              // collide if two unmapped probes ever co-exist.
              key={`${c.name}-${i}`}
              className="flex items-center justify-between border-b border-border py-2.5"
            >
              <span className="font-medium">
                {COMPONENT_LABELS[c.name] ?? c.name}
              </span>
              <span className="flex items-center gap-3">
                <span className="text-xs tabular-nums text-fg-subtle">
                  {c.latency_ms.toFixed(0)} ms
                </span>
                <StatusPill status={c.status} />
              </span>
            </div>
          ))}
        </div>
      </section>

      <section className="mt-8">
        <h2 className="text-base">Recent capacity changes</h2>
        {data.incidents.length === 0 ? (
          <p className="text-[13px] text-fg-muted">
            No capacity changes in the recent window.
          </p>
        ) : (
          <ul className="mt-2 list-none p-0">
            {data.incidents.map((incident, i) => (
              <li
                key={`${incident.at}-${i}`}
                className="border-b border-border py-2 text-[13px]"
              >
                <span className="mr-2 text-fg-muted">
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
