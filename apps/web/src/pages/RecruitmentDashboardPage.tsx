import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type {
  ApplicationStatus,
  JobApplication,
  JobOpening,
} from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Skeleton,
  StatCard,
} from "@kapp/ui";
import { AlertTriangle, Briefcase, RefreshCw } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import { humanizeToken } from "../lib/ktypeView";
import { openingVariant } from "../lib/recruitmentStatus";

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
// Rejected and withdrawn carry no rank and drop out of every bar, so the
// funnel measures cumulative reach of still-live candidates.
const STAGE_RANK: Record<string, number> = {
  applied: 0,
  screening: 1,
  shortlisted: 2,
  interview: 3,
  offered: 4,
  hired: 5,
};

const MS_PER_DAY = 1000 * 60 * 60 * 24;

// Forward-stage open status tokens that count as a live requisition.
const OPEN_STATUSES = new Set(["open", "on_hold"]);

/**
 * RecruitmentDashboardPage is the hiring landing surface: headline KPIs
 * (open roles, active candidates, offers out, average time-to-hire), a
 * per-opening demand list, and a conversion funnel. Every figure derives
 * from the openings + applications lists, so no dedicated endpoint is
 * needed.
 */
export function RecruitmentDashboardPage() {
  const fmt = useFormatter();
  const openingsQ = useQuery<JobOpening[]>({
    queryKey: ["recruitment", "job-openings", ""],
    queryFn: () => api.listJobOpenings(),
  });
  const appsQ = useQuery<JobApplication[]>({
    queryKey: ["recruitment", "applications", ""],
    queryFn: () => api.listApplications(),
  });

  const openings = openingsQ.data ?? [];
  const apps = appsQ.data ?? [];

  const openPositions = openings.filter((o) => OPEN_STATUSES.has(o.status)).length;
  const activeCandidates = apps.filter(
    (a) =>
      a.status !== "rejected" &&
      a.status !== "withdrawn" &&
      a.status !== "hired",
  ).length;
  const offersOut = apps.filter((a) => a.status === "offered").length;

  // avgTimeToHire averages applied_at → updated_at (the hire stamp) over
  // hired applications, in whole days.
  const avgTimeToHire = useMemo(() => {
    const hired = apps.filter((a) => a.status === "hired");
    if (hired.length === 0) return null;
    const totalDays = hired.reduce((sum, a) => {
      const start = new Date(a.applied_at).getTime();
      const end = new Date(a.updated_at).getTime();
      return sum + Math.max(0, (end - start) / MS_PER_DAY);
    }, 0);
    return totalDays / hired.length;
  }, [apps]);

  const countByOpening = useMemo(() => {
    const m = new Map<string, number>();
    apps.forEach((a) => {
      if (a.status === "withdrawn") return;
      m.set(a.job_opening_id, (m.get(a.job_opening_id) ?? 0) + 1);
    });
    return m;
  }, [apps]);

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
  const error = openingsQ.isError || appsQ.isError;

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Recruitment
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Your hiring pipeline at a glance — open roles, candidates in
            play, and how fast you're hiring.
          </p>
        </div>
        <Button size="sm" variant="outline" asChild>
          <Link to="/hr/recruitment/job-openings">
            <Briefcase className="h-4 w-4" />
            View job openings
          </Link>
        </Button>
      </header>

      {error && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
        >
          <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
          <span className="min-w-0 flex-1">
            We couldn't load your recruitment data.
          </span>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<RefreshCw className="h-4 w-4" />}
            onClick={() => {
              openingsQ.refetch();
              appsQ.refetch();
            }}
          >
            Retry
          </Button>
        </div>
      )}

      {loading && <DashboardSkeleton />}

      {!loading && !error && openings.length === 0 && apps.length === 0 && (
        <EmptyState
          icon={<Briefcase />}
          title="No hiring activity yet"
          description="Post your first job opening to start tracking candidates through the pipeline."
          action={
            <Button asChild size="sm">
              <Link to="/hr/recruitment/job-openings">Post a job opening</Link>
            </Button>
          }
        />
      )}

      {!loading && !error && (openings.length > 0 || apps.length > 0) && (
        <>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <StatCard
              label="Open positions"
              value={fmt.number(openPositions)}
              sub={`${fmt.number(openings.length)} total openings`}
              renderContainer={({ className, children }) => (
                <Link to="/hr/recruitment/job-openings" className={className}>
                  {children}
                </Link>
              )}
            />
            <StatCard
              label="Active candidates"
              value={fmt.number(activeCandidates)}
              sub={`${fmt.number(apps.length)} total applications`}
              renderContainer={({ className, children }) => (
                <Link to="/hr/recruitment/applications" className={className}>
                  {children}
                </Link>
              )}
            />
            <StatCard label="Offers out" value={fmt.number(offersOut)} />
            <StatCard
              label="Avg. time to hire"
              value={
                avgTimeToHire === null
                  ? "—"
                  : `${fmt.number(avgTimeToHire, { maximumFractionDigits: 1 })} days`
              }
              sub="From application to hire"
            />
          </div>

          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            <div className="flex flex-col rounded-lg border border-border bg-bg-elevated">
              <div className="flex items-center justify-between border-b border-border px-4 py-3">
                <h2 className="text-sm font-semibold text-fg">Open positions</h2>
                <Badge variant="neutral" size="sm">
                  {fmt.number(openings.length)}
                </Badge>
              </div>
              {openings.length === 0 ? (
                <p className="px-4 py-6 text-sm text-fg-muted">
                  No openings yet.
                </p>
              ) : (
                <ul className="divide-y divide-border">
                  {openings.map((o) => (
                    <li key={o.id}>
                      <Link
                        to={`/hr/recruitment/applications?job_opening_id=${o.id}`}
                        className="flex items-center justify-between gap-3 px-4 py-2.5 transition-colors hover:bg-bg-subtle"
                      >
                        <span className="flex min-w-0 items-center gap-2">
                          <span className="truncate text-sm font-medium text-fg">
                            {o.title}
                          </span>
                          <Badge variant={openingVariant(o.status)} size="xs">
                            {humanizeToken(o.status)}
                          </Badge>
                        </span>
                        <span className="shrink-0 text-xs tabular-nums text-fg-muted">
                          {fmt.number(countByOpening.get(o.id) ?? 0)} applicants ·{" "}
                          {fmt.number(o.positions_filled)}/
                          {fmt.number(o.max_positions)} filled
                        </span>
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            <div className="flex flex-col rounded-lg border border-border bg-bg-elevated">
              <div className="flex items-center justify-between border-b border-border px-4 py-3">
                <h2 className="text-sm font-semibold text-fg">Pipeline funnel</h2>
                <span className="text-xs text-fg-muted">
                  {fmt.number(funnelMax)} in pipeline
                </span>
              </div>
              <div className="flex flex-col gap-2.5 p-4">
                {funnelMax === 0 ? (
                  <p className="text-sm text-fg-muted">
                    No candidates in the pipeline yet.
                  </p>
                ) : (
                  funnel.map((stage) => {
                    const pct = funnelMax > 0 ? (stage.count / funnelMax) * 100 : 0;
                    return (
                      <div key={stage.status} className="flex items-center gap-3">
                        <span className="w-24 shrink-0 text-xs font-medium text-fg-muted">
                          {stage.label}
                        </span>
                        <div className="h-6 flex-1 overflow-hidden rounded-md bg-bg-muted">
                          <div
                            className="flex h-full items-center justify-end rounded-md bg-accent px-2 text-xs font-medium text-accent-fg transition-[width]"
                            style={{ width: `${Math.max(pct, 8)}%` }}
                          >
                            {fmt.number(stage.count)}
                          </div>
                        </div>
                        <span className="w-10 shrink-0 text-end text-xs tabular-nums text-fg-subtle">
                          {Math.round(pct)}%
                        </span>
                      </div>
                    );
                  })
                )}
              </div>
            </div>
          </div>
        </>
      )}
    </section>
  );
}

function DashboardSkeleton() {
  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} className="h-24 w-full rounded-lg" />
        ))}
      </div>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Skeleton className="h-64 w-full rounded-lg" />
        <Skeleton className="h-64 w-full rounded-lg" />
      </div>
    </div>
  );
}
