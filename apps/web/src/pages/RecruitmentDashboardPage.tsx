import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { ApplicationStatus, JobOpening } from "@kapp/client";
import { Badge, StatCard } from "@kapp/ui";
import { api } from "../lib/api";

// FUNNEL_STAGES are the forward pipeline stages, in order, used for the
// conversion funnel. Terminal states (rejected/withdrawn) are excluded —
// the funnel measures progression, not attrition.
const FUNNEL_STAGES: Array<{ status: ApplicationStatus; label: string }> = [
  { status: "applied", label: "Applied" },
  { status: "screening", label: "Screening" },
  { status: "shortlisted", label: "Shortlisted" },
  { status: "interview", label: "Interview" },
  { status: "offered", label: "Offered" },
  { status: "hired", label: "Hired" },
];

// STAGE_RANK lets us count an application toward every forward stage it
// has reached or passed. Since the pipeline is linear, an application in
// 'interview' has necessarily cleared applied/screening/shortlisted, so
// the funnel reflects cumulative reach rather than current occupancy.
// Note: only currently-active applications are ranked — rejected and
// withdrawn carry no rank and drop out of every bar, even ones they
// previously cleared. The funnel therefore measures cumulative reach of
// still-live candidates (progression), deliberately not attrition.
const STAGE_RANK: Record<string, number> = {
  applied: 0,
  screening: 1,
  shortlisted: 2,
  interview: 3,
  offered: 4,
  hired: 5,
};

const MS_PER_DAY = 1000 * 60 * 60 * 24;

/**
 * RecruitmentDashboardPage is the recruitment landing surface: KPI
 * cards (open positions, active candidates, offers out, avg
 * time-to-hire), a per-opening application-count table, and a
 * cumulative pipeline funnel. All figures derive from the openings +
 * applications lists, so the dashboard needs no dedicated endpoint.
 */
export function RecruitmentDashboardPage() {
  const openingsQ = useQuery({
    queryKey: ["recruitment", "job-openings", ""],
    queryFn: () => api.listJobOpenings(),
  });
  const appsQ = useQuery({
    queryKey: ["recruitment", "applications", ""],
    queryFn: () => api.listApplications(),
  });

  const openings = openingsQ.data ?? [];
  const apps = appsQ.data ?? [];

  const openPositions = openings.filter(
    (o) => o.status === "open" || o.status === "on_hold",
  ).length;
  const activeCandidates = apps.filter(
    (a) =>
      a.status !== "rejected" &&
      a.status !== "withdrawn" &&
      a.status !== "hired",
  ).length;
  const offersOut = apps.filter((a) => a.status === "offered").length;

  // avgTimeToHire averages applied_at → updated_at (the hire stamp) over
  // hired applications, in whole days. updated_at is the last write,
  // which for a hired application is the advance-to-hired commit.
  const avgTimeToHire = useMemo(() => {
    const hired = apps.filter((a) => a.status === "hired");
    if (hired.length === 0) return null;
    const totalDays = hired.reduce((sum, a) => {
      const start = new Date(a.applied_at).getTime();
      const end = new Date(a.updated_at).getTime();
      const days = Math.max(0, (end - start) / MS_PER_DAY);
      return sum + days;
    }, 0);
    return totalDays / hired.length;
  }, [apps]);

  // countByOpening maps opening id → number of (non-withdrawn)
  // applications, so the table shows live demand per requisition.
  const countByOpening = useMemo(() => {
    const m = new Map<string, number>();
    apps.forEach((a) => {
      if (a.status === "withdrawn") return;
      m.set(a.job_opening_id, (m.get(a.job_opening_id) ?? 0) + 1);
    });
    return m;
  }, [apps]);

  // funnel counts applications that have reached at least each stage.
  const funnel = useMemo(() => {
    return FUNNEL_STAGES.map((stage) => {
      const minRank = STAGE_RANK[stage.status];
      const count = apps.filter((a) => {
        const r = STAGE_RANK[a.status];
        return r !== undefined && r >= minRank;
      }).length;
      return { ...stage, count };
    });
  }, [apps]);

  const funnelMax = funnel[0]?.count ?? 0;
  const loading = openingsQ.isLoading || appsQ.isLoading;

  return (
    <section className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Recruitment
      </h1>

      {loading && <p className="text-sm text-fg-muted">Loading…</p>}
      {(openingsQ.isError || appsQ.isError) && (
        <p className="text-sm text-danger">
          {String(openingsQ.error ?? appsQ.error)}
        </p>
      )}

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Open positions"
          value={openPositions}
          sub={`${openings.length} total openings`}
          renderContainer={({ className, children }) => (
            <Link to="/hr/recruitment/job-openings" className={className}>
              {children}
            </Link>
          )}
        />
        <StatCard
          label="Active candidates"
          value={activeCandidates}
          sub={`${apps.length} total applications`}
          renderContainer={({ className, children }) => (
            <Link to="/hr/recruitment/applications" className={className}>
              {children}
            </Link>
          )}
        />
        <StatCard label="Offers out" value={offersOut} />
        <StatCard
          label="Avg time-to-hire"
          value={avgTimeToHire === null ? "—" : `${avgTimeToHire.toFixed(1)}d`}
          sub="Applied → hired"
        />
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <div className="rounded-lg border border-border bg-bg-subtle p-3">
          <h2 className="mb-2 text-sm font-semibold text-fg">
            Open positions
          </h2>
          {openings.length === 0 ? (
            <p className="text-sm text-fg-muted">No openings yet.</p>
          ) : (
            <ul className="flex flex-col gap-1">
              {openings.map((o: JobOpening) => (
                <li
                  key={o.id}
                  className="flex items-center justify-between gap-2 rounded px-2 py-1 hover:bg-bg-muted"
                >
                  <Link
                    to={`/hr/recruitment/applications?job_opening_id=${o.id}`}
                    className="truncate text-sm text-fg hover:underline"
                  >
                    {o.title}
                  </Link>
                  <span className="flex shrink-0 items-center gap-2">
                    <Badge variant="outline" size="xs">
                      {o.status}
                    </Badge>
                    <span className="text-sm text-fg-muted">
                      {countByOpening.get(o.id) ?? 0} apps ·{" "}
                      {o.positions_filled}/{o.max_positions}
                    </span>
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>

        <div className="rounded-lg border border-border bg-bg-subtle p-3">
          <h2 className="mb-2 text-sm font-semibold text-fg">
            Pipeline funnel
          </h2>
          <div className="flex flex-col gap-1.5">
            {funnel.map((stage) => {
              const pct = funnelMax > 0 ? (stage.count / funnelMax) * 100 : 0;
              return (
                <div key={stage.status} className="flex items-center gap-2">
                  <span className="w-24 shrink-0 text-xs text-fg-muted">
                    {stage.label}
                  </span>
                  <div className="h-5 flex-1 overflow-hidden rounded bg-bg-muted">
                    <div
                      className="flex h-full items-center justify-end rounded bg-accent px-2 text-[11px] font-medium text-accent-fg"
                      style={{ width: `${Math.max(pct, 6)}%` }}
                    >
                      {stage.count}
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      </div>
    </section>
  );
}
