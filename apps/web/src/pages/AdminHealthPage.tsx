import { useQuery } from "@tanstack/react-query";

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

const STATUS_META: Record<HealthStatus, { label: string; color: string; bg: string }> = {
  operational: { label: "Operational", color: "#065f46", bg: "#d1fae5" },
  degraded: { label: "Degraded", color: "#92400e", bg: "#fef3c7" },
  down: { label: "Down", color: "#991b1b", bg: "#fee2e2" },
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

// SaturationBar renders a 0–100% horizontal meter that shifts colour
// as it fills (green → amber → red) so an operator can spot a
// saturating pool/cell at a glance. Values are clamped so a >100%
// reading (more in-flight than the configured max) does not overflow
// the track.
function SaturationBar({ percent }: { percent: number }) {
  const pct = Math.max(0, Math.min(100, percent));
  const color = pct > 90 ? "#dc2626" : pct > 70 ? "#f59e0b" : "#10b981";
  return (
    <div
      style={{
        background: "#f3f4f6",
        height: 12,
        borderRadius: 6,
        overflow: "hidden",
      }}
    >
      <div
        style={{
          width: `${pct}%`,
          height: "100%",
          background: color,
          transition: "width 200ms ease",
        }}
      />
    </div>
  );
}

const th: React.CSSProperties = {
  textAlign: "left",
  borderBottom: "1px solid #e5e7eb",
  padding: "6px 8px",
  fontSize: 12,
  color: "#6b7280",
};
const td: React.CSSProperties = { padding: "6px 8px", fontSize: 13 };

export function AdminHealthPage() {
  const healthQuery = useQuery({
    queryKey: ["admin-health"],
    queryFn: fetchAdminHealth,
    refetchInterval: 15_000,
  });

  if (healthQuery.isLoading) {
    return <section>Loading operator dashboard…</section>;
  }
  if (healthQuery.error || !healthQuery.data) {
    return (
      <section style={{ color: "#991b1b" }}>
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
    <section>
      <h1>System Health</h1>
      <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
        <StatusPill status={data.system.status} />
        <span style={{ color: "#6b7280", fontSize: 13 }}>
          Checked {new Date(data.system.checked_at).toLocaleString()}
        </span>
      </div>

      <section style={{ marginTop: 28 }}>
        <h2 style={{ fontSize: 16 }}>Components</h2>
        <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 8 }}>
          <thead>
            <tr>
              <th style={th}>Component</th>
              <th style={th}>Status</th>
              <th style={th}>Latency</th>
              <th style={th}>Detail</th>
            </tr>
          </thead>
          <tbody>
            {data.system.components.map((c) => (
              <tr key={c.name} style={{ borderBottom: "1px solid #f3f4f6" }}>
                <td style={td}>{c.name}</td>
                <td style={td}>
                  <StatusPill status={c.status} />
                </td>
                <td style={{ ...td, fontVariantNumeric: "tabular-nums" }}>
                  {c.latency_ms.toFixed(1)} ms
                </td>
                <td style={{ ...td, color: c.error ? "#991b1b" : "#6b7280" }}>
                  {c.error
                    ? c.error
                    : c.detail
                      ? JSON.stringify(c.detail)
                      : "—"}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section style={{ marginTop: 28 }}>
        <h2 style={{ fontSize: 16 }}>Database connection pool</h2>
        <div
          style={{
            display: "flex",
            justifyContent: "space-between",
            fontSize: 13,
            marginBottom: 4,
          }}
        >
          <span>
            {data.pool.total_conns} / {data.pool.max_conns} connections (
            {data.pool.acquired_conns} in use, {data.pool.idle_conns} idle)
          </span>
          <span style={{ fontVariantNumeric: "tabular-nums" }}>
            {data.pool.saturation_percent.toFixed(0)}%
          </span>
        </div>
        <SaturationBar percent={data.pool.saturation_percent} />
      </section>

      <section style={{ marginTop: 28 }}>
        <h2 style={{ fontSize: 16 }}>Outbox backlog</h2>
        <p style={{ fontSize: 13, color: "#6b7280", marginTop: 4 }}>
          <strong
            style={{
              fontSize: 20,
              color: outboxBacklog > 0 ? "#92400e" : "#065f46",
            }}
          >
            {outboxBacklog.toLocaleString()}
          </strong>{" "}
          undelivered events awaiting drain.
        </p>
      </section>

      <section style={{ marginTop: 28 }}>
        <h2 style={{ fontSize: 16 }}>Cell utilization</h2>
        {data.cells.length === 0 ? (
          <p style={{ color: "#6b7280", fontSize: 13 }}>No cells registered.</p>
        ) : (
          <div style={{ marginTop: 8 }}>
            {data.cells.map((cell) => (
              <div key={cell.id} style={{ marginBottom: 16 }}>
                <div
                  style={{
                    display: "flex",
                    justifyContent: "space-between",
                    fontSize: 13,
                    marginBottom: 4,
                  }}
                >
                  <strong>
                    {cell.id}{" "}
                    <span style={{ color: "#9ca3af", fontWeight: 400 }}>
                      ({cell.region || "—"})
                    </span>
                  </strong>
                  <span style={{ fontVariantNumeric: "tabular-nums" }}>
                    {cell.tenant_count} / {cell.max_tenants} tenants ·{" "}
                    {cell.utilization_pct.toFixed(0)}%
                  </span>
                </div>
                <SaturationBar percent={cell.utilization_pct} />
                <div
                  style={{
                    display: "flex",
                    gap: 16,
                    fontSize: 11,
                    color: "#9ca3af",
                    marginTop: 2,
                  }}
                >
                  <span>CPU {cell.cpu_pct.toFixed(0)}%</span>
                  <span>Mem {cell.mem_pct.toFixed(0)}%</span>
                  <span>Conn {cell.conn_saturation_pct.toFixed(0)}%</span>
                </div>
              </div>
            ))}
          </div>
        )}
      </section>

      <section style={{ marginTop: 28 }}>
        <h2 style={{ fontSize: 16 }}>Top tenants by API calls</h2>
        {data.top_tenants.length === 0 ? (
          <p style={{ color: "#6b7280", fontSize: 13 }}>
            No API usage recorded this period.
          </p>
        ) : (
          <div style={{ marginTop: 8 }}>
            {data.top_tenants.map((t) => (
              <div key={t.tenant_id} style={{ marginBottom: 12 }}>
                <div
                  style={{
                    display: "flex",
                    justifyContent: "space-between",
                    fontSize: 13,
                    marginBottom: 4,
                  }}
                >
                  <span>{t.name || t.tenant_id}</span>
                  <span style={{ fontVariantNumeric: "tabular-nums" }}>
                    {t.api_calls.toLocaleString()}
                  </span>
                </div>
                <div
                  style={{
                    background: "#f3f4f6",
                    height: 10,
                    borderRadius: 5,
                    overflow: "hidden",
                  }}
                >
                  <div
                    style={{
                      width: `${maxApiCalls > 0 ? (t.api_calls / maxApiCalls) * 100 : 0}%`,
                      height: "100%",
                      background: "#6366f1",
                    }}
                  />
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
    </section>
  );
}
