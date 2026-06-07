import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Badge as UIBadge,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
  type BadgeProps,
} from "@kapp/ui";
import { api } from "../lib/api";

const KTYPE_PROJECT = "projects.project";
const KTYPE_MILESTONE = "projects.milestone";

interface ProjectData {
  name?: string;
  code?: string;
  status?: string;
  start_date?: string;
  end_date?: string;
}

interface MilestoneData {
  project_id?: string;
  name?: string;
  due_date?: string;
  weight?: number;
  status?: string;
}

// Map the project / milestone status vocabulary onto the semantic
// design-token palette. `variant` drives the Badge pill; `bar` is the
// Tailwind background class used for the Gantt bar and milestone
// markers so both stay on the oklch token system.
const STATUS_META: Record<string, { variant: BadgeProps["variant"]; bar: string }> = {
  planning: { variant: "default", bar: "bg-fg-subtle" },
  active: { variant: "info", bar: "bg-info" },
  completed: { variant: "success", bar: "bg-success" },
  archived: { variant: "default", bar: "bg-fg-subtle" },
  planned: { variant: "default", bar: "bg-fg-subtle" },
  in_progress: { variant: "info", bar: "bg-info" },
  cancelled: { variant: "danger", bar: "bg-danger" },
};

function statusMeta(status: string): { variant: BadgeProps["variant"]; bar: string } {
  return STATUS_META[status] ?? { variant: "default", bar: "bg-fg-subtle" };
}

/**
 * ProjectGanttPage renders a lightweight Gantt strip per project,
 * using each project's start_date / end_date as the bar extent and
 * milestone due_dates as markers along the bar. The component is
 * intentionally framework-agnostic (no third-party Gantt lib) so
 * the page stays under one network round-trip and a few hundred
 * bytes of JS — sufficient for the Phase M Task 5 acceptance bar.
 *
 * The day grid spans the union of every project's [start, end]
 * window. Projects without a complete window are still rendered
 * (left-justified at the earliest known start) so an operator can
 * see the milestone markers even on freshly-created projects.
 */
export function ProjectGanttPage() {
  const projectsQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_PROJECT],
    queryFn: () => api.listRecords(KTYPE_PROJECT),
  });
  const milestonesQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_MILESTONE],
    queryFn: () => api.listRecords(KTYPE_MILESTONE),
  });

  const { rangeStart, rangeEnd } = useMemo(() => {
    const dates: Date[] = [];
    const projects = projectsQ.data ?? [];
    for (const p of projects) {
      const data = (p.data as ProjectData) ?? {};
      if (data.start_date) dates.push(new Date(data.start_date));
      if (data.end_date) dates.push(new Date(data.end_date));
    }
    if (dates.length === 0) {
      const today = new Date();
      const inAMonth = new Date();
      inAMonth.setDate(today.getDate() + 30);
      return { rangeStart: today, rangeEnd: inAMonth };
    }
    const min = new Date(Math.min(...dates.map((d) => d.getTime())));
    const max = new Date(Math.max(...dates.map((d) => d.getTime())));
    return { rangeStart: min, rangeEnd: max };
  }, [projectsQ.data]);

  if (projectsQ.isLoading || milestonesQ.isLoading)
    return <p className="text-sm text-fg-muted">Loading…</p>;
  if (projectsQ.isError) {
    return (
      <p className="text-sm text-danger">
        Failed to load projects: {(projectsQ.error as Error).message}
      </p>
    );
  }

  const projects = projectsQ.data ?? [];
  const milestones = milestonesQ.data ?? [];
  const milestonesByProject = new Map<string, KRecord[]>();
  for (const m of milestones) {
    const data = (m.data as MilestoneData) ?? {};
    const pid = data.project_id ?? "";
    if (!pid) continue;
    const list = milestonesByProject.get(pid) ?? [];
    list.push(m);
    milestonesByProject.set(pid, list);
  }

  const totalDays = Math.max(
    1,
    Math.ceil(
      (rangeEnd.getTime() - rangeStart.getTime()) / (1000 * 60 * 60 * 24),
    ),
  );

  return (
    <section className="flex flex-col gap-2">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">Projects</h1>
      <p className="text-sm text-fg-muted">
        Gantt strip per project. Bar extent = start_date → end_date; markers
        are milestone due dates coloured by status.
      </p>

      {projects.length === 0 ? (
        <p className="mt-4 text-sm text-fg-muted">
          No projects yet. Create one via{" "}
          <Link
            to="/records/projects.project"
            className="text-accent hover:underline"
          >
            the records list
          </Link>{" "}
          or the KChat <code>/project</code> command.
        </p>
      ) : (
        <div className="mt-4">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Project</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>
                  Timeline ({rangeStart.toISOString().slice(0, 10)} →{" "}
                  {rangeEnd.toISOString().slice(0, 10)})
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {projects.map((p) => {
                const data = (p.data as ProjectData) ?? {};
                const start = data.start_date ? new Date(data.start_date) : rangeStart;
                const end = data.end_date ? new Date(data.end_date) : rangeEnd;
                const left = pct(start, rangeStart, totalDays);
                const width = Math.max(
                  1,
                  pct(end, rangeStart, totalDays) - left,
                );
                const ms = milestonesByProject.get(p.id) ?? [];
                return (
                  <TableRow key={p.id}>
                    <TableCell>
                      <Link
                        to={`/records/${KTYPE_PROJECT}/${p.id}`}
                        className="text-accent hover:underline"
                      >
                        {data.name ?? p.id}
                      </Link>
                      {data.code && (
                        <div className="text-[11px] text-fg-muted">
                          {data.code}
                        </div>
                      )}
                    </TableCell>
                    <TableCell>
                      <UIBadge variant={statusMeta(data.status ?? "planning").variant}>
                        {data.status ?? "planning"}
                      </UIBadge>
                    </TableCell>
                    <TableCell className="py-2">
                      <div className="relative h-[18px] rounded bg-bg-muted">
                        <div
                          className={cn(
                            "absolute top-1 h-2.5 min-w-1 rounded",
                            statusMeta(data.status ?? "planning").bar,
                          )}
                          style={{ left: `${left}%`, width: `${width}%` }}
                          title={`${data.start_date ?? "?"} → ${data.end_date ?? "?"}`}
                        />
                        {ms.map((m) => {
                          const md = (m.data as MilestoneData) ?? {};
                          if (!md.due_date) return null;
                          const x = pct(new Date(md.due_date), rangeStart, totalDays);
                          return (
                            <div
                              key={m.id}
                              className={cn(
                                "absolute top-0 h-[18px] w-1 rounded-[1px]",
                                statusMeta(md.status ?? "planned").bar,
                              )}
                              style={{ left: `${x}%` }}
                              title={`${md.name ?? m.id} — ${md.due_date} (${md.status ?? ""})`}
                            />
                          );
                        })}
                      </div>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  );
}

function pct(d: Date, start: Date, totalDays: number): number {
  const days = Math.max(
    0,
    Math.floor((d.getTime() - start.getTime()) / (1000 * 60 * 60 * 24)),
  );
  return Math.min(100, (days / totalDays) * 100);
}
