import type { ComponentType } from "react";
import { useQuery } from "@tanstack/react-query";
import { Badge, Button, EmptyState, Eyebrow, Skeleton, cn } from "@kapp/ui";
import {
  AlertTriangle,
  Clock,
  CircleCheck,
  CircleX,
  History,
} from "lucide-react";

// StatusPage is the PUBLIC, unauthenticated status page backed by
// GET /api/v1/health. It deliberately talks to the endpoint with a
// bare fetch (no tenant/auth headers) rather than the shared
// ApiClient: the page is mounted outside the authenticated app shell
// and must render for anonymous visitors, so it cannot depend on a
// tenant id or bearer token being present in localStorage. For the
// same reason it formats dates/numbers with the platform Intl
// defaults rather than the app's locale context, which isn't mounted
// on the public route.

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

// In demo mode the mock layer installs a window.fetch shim that serves
// /api/v1/health from an in-memory fixture. api.ts installs it on boot,
// but this page's first read can fire before that resolves — so ensure
// the (idempotent) shim is in place first, otherwise a cold load races
// the install and 500s through the proxy.
const demoMode = import.meta.env.VITE_DEMO_MODE === "true";
async function ensureDemoFetch(): Promise<void> {
  if (!demoMode) return;
  const { installPortalDemoFetch } = await import("../lib/mock-api");
  installPortalDemoFetch();
}

async function fetchPublicHealth(): Promise<PublicHealth> {
  await ensureDemoFetch();
  const res = await fetch("/api/v1/health");
  if (!res.ok) {
    throw new Error(`health request failed: ${res.status}`);
  }
  return (await res.json()) as PublicHealth;
}

// STATUS_META centralises the human label + design-system colour +
// icon mapping for each status so the banner, the per-component rows,
// and any future surface stay visually consistent. `badge` selects
// the Badge variant; `banner` is the token class pair for the large
// headline banner; `icon`/`tone` drive the inline status glyph.
const STATUS_META: Record<
  HealthStatus,
  {
    label: string;
    badge: "success" | "warning" | "danger";
    banner: string;
    icon: ComponentType<{ className?: string }>;
    tone: string;
  }
> = {
  operational: {
    label: "Operational",
    badge: "success",
    banner: "bg-success text-success-fg",
    icon: CircleCheck,
    tone: "text-success",
  },
  degraded: {
    label: "Degraded",
    badge: "warning",
    banner: "bg-warning text-warning-fg",
    icon: AlertTriangle,
    tone: "text-warning",
  },
  down: {
    label: "Down",
    badge: "danger",
    banner: "bg-danger text-danger-fg",
    icon: CircleX,
    tone: "text-danger",
  },
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

const BANNER_HEADLINE: Record<HealthStatus, string> = {
  operational: "All systems operational",
  degraded: "Some systems degraded",
  down: "Major outage",
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
    return (
      <div className="mx-auto flex max-w-3xl flex-col gap-6 px-4 py-8">
        <div className="flex flex-col gap-2">
          <Skeleton className="h-4 w-20" />
          <Skeleton className="h-8 w-56" />
          <Skeleton className="h-4 w-40" />
        </div>
        <Skeleton className="h-24 w-full rounded-lg" />
        <Skeleton className="h-48 w-full rounded-lg" />
      </div>
    );
  }

  if (healthQuery.error || !healthQuery.data) {
    return (
      <div className="mx-auto max-w-3xl px-4 py-8">
        <EmptyState
          icon={<AlertTriangle className="h-6 w-6" aria-hidden />}
          title="Unable to load platform status"
          description="Please try again shortly."
          action={
            <Button
              variant="secondary"
              onClick={() => void healthQuery.refetch()}
              disabled={healthQuery.isFetching}
            >
              Try again
            </Button>
          }
        />
      </div>
    );
  }

  const data = healthQuery.data;
  const meta = STATUS_META[data.status];
  const BannerIcon = meta.icon;

  return (
    <div className="mx-auto flex max-w-3xl flex-col gap-8 px-4 py-8">
      <header className="flex flex-col gap-1">
        <Eyebrow>System</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Platform status
        </h1>
        <p className="flex items-center gap-1.5 text-sm text-fg-muted">
          <Clock className="h-3.5 w-3.5" aria-hidden />
          Last checked {new Date(data.checked_at).toLocaleString()}
        </p>
      </header>

      <div
        className={cn(
          "flex items-center gap-4 rounded-lg p-5",
          meta.banner,
        )}
      >
        <BannerIcon className="h-8 w-8 shrink-0" aria-hidden />
        <div className="flex flex-col">
          <span className="text-lg font-semibold">
            {BANNER_HEADLINE[data.status]}
          </span>
          <span className="text-sm opacity-90">
            {data.component_availability_percent.toFixed(0)}% of components
            operational
          </span>
        </div>
      </div>

      <section className="flex flex-col gap-3">
        <h2 className="text-base font-semibold text-fg">Components</h2>
        <ul className="overflow-hidden rounded-lg border border-border">
          {data.components.map((c, i) => {
            const cMeta = STATUS_META[c.status];
            const CIcon = cMeta.icon;
            return (
              <li
                // Index-suffixed: the public API collapses any unmapped
                // component to the generic "service" label, so names are
                // not guaranteed unique — keying on name alone could
                // collide if two unmapped probes ever co-exist.
                key={`${c.name}-${i}`}
                className="flex items-center justify-between gap-3 border-b border-border bg-bg px-4 py-3 last:border-b-0"
              >
                <span className="flex items-center gap-2.5">
                  <CIcon className={cn("h-4 w-4 shrink-0", cMeta.tone)} aria-hidden />
                  <span className="font-medium text-fg">
                    {COMPONENT_LABELS[c.name] ?? c.name}
                  </span>
                </span>
                <span className="flex items-center gap-3">
                  <span className="font-tabular text-xs text-fg-subtle">
                    {c.latency_ms.toFixed(0)} ms
                  </span>
                  <StatusPill status={c.status} />
                </span>
              </li>
            );
          })}
        </ul>
      </section>

      <section className="flex flex-col gap-3">
        <h2 className="flex items-center gap-2 text-base font-semibold text-fg">
          <History className="h-4 w-4 text-fg-muted" aria-hidden />
          Recent capacity changes
        </h2>
        {data.incidents.length === 0 ? (
          <p className="rounded-lg border border-dashed border-border px-4 py-6 text-center text-sm text-fg-muted">
            No capacity changes in the recent window.
          </p>
        ) : (
          <ul className="flex flex-col gap-3">
            {data.incidents.map((incident, i) => (
              <li
                key={`${incident.at}-${i}`}
                className="flex flex-col gap-0.5 border-l-2 border-border pl-3"
              >
                <span className="text-xs text-fg-subtle">
                  {new Date(incident.at).toLocaleString()}
                </span>
                <span className="text-sm text-fg">{incident.summary}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
