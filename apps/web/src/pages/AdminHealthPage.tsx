import { useQuery } from "@tanstack/react-query";
import {
  Badge,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
} from "@kapp/ui";

// AdminHealthPage is the operator dashboard backed by the admin-only
// GET /api/v1/admin/health/detailed. Like the public status page it
// uses a bare fetch, but here it MUST forward the bearer token (and
// tenant header, which adminChain ignores after scrubbing) so the
// admin middleware authorises the request.

type HealthStatus = "operational" | "degraded" | "down";

interface ComponentHealth {
  name: string;
  status: HealthStatus;
  latency_ms: number;
  error?: string;
  detail?: Record<string, unknown>;
}

interface SystemHealth {
  status: HealthStatus;
  components: ComponentHealth[];
  checked_at: string;
}

interface CellHealth {
  id: string;
  region: string;
  max_tenants: number;
  tenant_count: number;
  cpu_pct: number;
  mem_pct: number;
  conn_saturation_pct: number;
  utilization_pct: number;
}

interface PoolHealth {
  max_conns: number;
  total_conns: number;
  acquired_conns: number;
  idle_conns: number;
  saturation_percent: number;
}

interface TopTenant {
  tenant_id: string;
  name: string;
  api_calls: number;
}

interface AdminHealth {
  system: SystemHealth;
  cells: CellHealth[];
  pool: PoolHealth;
  top_tenants: TopTenant[];
}

const tenantId = (): string =>
  localStorage.getItem("kapp.tenant") ?? "default";
const token = (): string | null => localStorage.getItem("kapp.token");

async function fetchAdminHealth(): Promise<AdminHealth> {
  const headers: Record<string, string> = { "X-Tenant-ID": tenantId() };
  const t = token();
  if (t) headers.Authorization = `Bearer ${t}`;
  const res = await fetch("/api/v1/admin/health/detailed", { headers });
  if (!res.ok) {
    throw new Error(`admin health request failed: ${res.status}`);
  }
  return (await res.json()) as AdminHealth;
}

// Status maps to the design-system Badge's semantic variants so the
// meaning (ok / warn / fail) is carried by the shared colour scale
// rather than hand-picked hex values.
const STATUS_VARIANT: Record<HealthStatus, "success" | "warning" | "danger"> = {
  operational: "success",
  degraded: "warning",
  down: "danger",
};
const STATUS_LABEL: Record<HealthStatus, string> = {
  operational: "Operational",
  degraded: "Degraded",
  down: "Down",
};

function StatusPill({ status }: { status: HealthStatus }) {
  return <Badge variant={STATUS_VARIANT[status]}>{STATUS_LABEL[status]}</Badge>;
}

// SaturationBar renders a 0–100% horizontal meter that shifts colour
// as it fills (green → amber → red) so an operator can spot a
// saturating pool/cell at a glance. Values are clamped so a >100%
// reading (more in-flight than the configured max) does not overflow
// the track. The fill width is the one genuinely dynamic value, so it
// stays as an inline style; every colour comes from a token class.
function SaturationBar({ percent }: { percent: number }) {
  const pct = Math.max(0, Math.min(100, percent));
  const color =
    pct > 90 ? "bg-danger" : pct > 70 ? "bg-warning" : "bg-success";
  return (
    <div className="h-3 overflow-hidden rounded-full bg-bg-muted">
      <div
        className={cn(
          "h-full rounded-full transition-[width] duration-200",
          color,
        )}
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

export function AdminHealthPage() {
  const healthQuery = useQuery({
    queryKey: ["admin-health"],
    queryFn: fetchAdminHealth,
    refetchInterval: 15_000,
  });

  if (healthQuery.isLoading) {
    return (
      <section className="text-sm text-fg-muted">
        Loading operator dashboard…
      </section>
    );
  }
  if (healthQuery.error || !healthQuery.data) {
    return (
      <section className="text-sm text-danger">
        Error loading operator dashboard.
      </section>
    );
  }

  const data = healthQuery.data;
  const outbox = data.system.components.find((c) => c.name === "outbox");
  const outboxBacklog =
    outbox && typeof outbox.detail?.undelivered_events === "number"
      ? (outbox.detail.undelivered_events as number)
      : 0;
  const maxApiCalls = data.top_tenants.reduce(
    (m, t) => Math.max(m, t.api_calls),
    0,
  );

  return (
    <section className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          System Health
        </h1>
        <div className="flex items-center gap-3">
          <StatusPill status={data.system.status} />
          <span className="text-sm text-fg-muted">
            Checked {new Date(data.system.checked_at).toLocaleString()}
          </span>
        </div>
      </header>

      <Card>
        <CardHeader>
          <CardTitle>Components</CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Component</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Latency</TableHead>
                <TableHead>Detail</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.system.components.map((c) => (
                <TableRow key={c.name}>
                  <TableCell className="font-medium text-fg">
                    {c.name}
                  </TableCell>
                  <TableCell>
                    <StatusPill status={c.status} />
                  </TableCell>
                  <TableCell className="tabular-nums">
                    {c.latency_ms.toFixed(1)} ms
                  </TableCell>
                  <TableCell
                    className={cn(
                      "max-w-xs truncate",
                      c.error ? "text-danger" : "text-fg-muted",
                    )}
                  >
                    {c.error
                      ? c.error
                      : c.detail
                        ? JSON.stringify(c.detail)
                        : "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Database connection pool</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-1">
          <div className="flex justify-between text-sm text-fg">
            <span>
              {data.pool.total_conns} / {data.pool.max_conns} connections (
              {data.pool.acquired_conns} in use, {data.pool.idle_conns} idle)
            </span>
            <span className="tabular-nums text-fg-muted">
              {data.pool.saturation_percent.toFixed(0)}%
            </span>
          </div>
          <SaturationBar percent={data.pool.saturation_percent} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Outbox backlog</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-fg-muted">
            <strong
              className={cn(
                "text-xl",
                outboxBacklog > 0 ? "text-warning" : "text-success",
              )}
            >
              {outboxBacklog.toLocaleString()}
            </strong>{" "}
            undelivered events awaiting drain.
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Cell utilization</CardTitle>
        </CardHeader>
        <CardContent>
          {data.cells.length === 0 ? (
            <p className="text-sm text-fg-muted">No cells registered.</p>
          ) : (
            <div className="flex flex-col gap-4">
              {data.cells.map((cell) => (
                <div key={cell.id} className="flex flex-col gap-1">
                  <div className="flex justify-between text-sm text-fg">
                    <strong className="font-semibold">
                      {cell.id}{" "}
                      <span className="font-normal text-fg-subtle">
                        ({cell.region || "—"})
                      </span>
                    </strong>
                    <span className="tabular-nums">
                      {cell.tenant_count} / {cell.max_tenants} tenants ·{" "}
                      {cell.utilization_pct.toFixed(0)}%
                    </span>
                  </div>
                  <SaturationBar percent={cell.utilization_pct} />
                  <div className="flex gap-4 text-xs text-fg-subtle">
                    <span>CPU {cell.cpu_pct.toFixed(0)}%</span>
                    <span>Mem {cell.mem_pct.toFixed(0)}%</span>
                    <span>Conn {cell.conn_saturation_pct.toFixed(0)}%</span>
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Top tenants by API calls</CardTitle>
        </CardHeader>
        <CardContent>
          {data.top_tenants.length === 0 ? (
            <p className="text-sm text-fg-muted">
              No API usage recorded this period.
            </p>
          ) : (
            <div className="flex flex-col gap-3">
              {data.top_tenants.map((t) => (
                <div key={t.tenant_id} className="flex flex-col gap-1">
                  <div className="flex justify-between text-sm text-fg">
                    <span>{t.name || t.tenant_id}</span>
                    <span className="tabular-nums">
                      {t.api_calls.toLocaleString()}
                    </span>
                  </div>
                  <div className="h-2.5 overflow-hidden rounded-full bg-bg-muted">
                    <div
                      className="h-full rounded-full bg-accent"
                      style={{
                        width: `${maxApiCalls > 0 ? (t.api_calls / maxApiCalls) * 100 : 0}%`,
                      }}
                    />
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </section>
  );
}
