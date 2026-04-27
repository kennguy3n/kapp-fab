import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
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

const STATUS_COLOR: Record<string, string> = {
  planning: "#9ca3af",
  active: "#2563eb",
  completed: "#16a34a",
  archived: "#6b7280",
  planned: "#9ca3af",
  in_progress: "#2563eb",
  cancelled: "#dc2626",
};

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

  if (projectsQ.isLoading || milestonesQ.isLoading) return <p>Loading…</p>;
  if (projectsQ.isError) {
    return (
      <p style={{ color: "#b91c1c" }}>
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
    <section>
      <h1>Projects</h1>
      <p style={{ color: "#6b7280" }}>
        Gantt strip per project. Bar extent = start_date → end_date; markers
        are milestone due dates coloured by status.
      </p>

      {projects.length === 0 ? (
        <p style={{ color: "#6b7280", marginTop: 16 }}>
          No projects yet. Create one via{" "}
          <Link to="/records/projects.project">the records list</Link> or the
          KChat <code>/project</code> command.
        </p>
      ) : (
        <table style={{ width: "100%", marginTop: 16, borderCollapse: "collapse" }}>
          <thead>
            <tr>
              <th style={th()}>Project</th>
              <th style={th()}>Status</th>
              <th style={th()}>
                Timeline ({rangeStart.toISOString().slice(0, 10)} →{" "}
                {rangeEnd.toISOString().slice(0, 10)})
              </th>
            </tr>
          </thead>
          <tbody>
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
                <tr key={p.id} style={{ borderTop: "1px solid #e5e7eb" }}>
                  <td style={td()}>
                    <Link to={`/records/${KTYPE_PROJECT}/${p.id}`}>
                      {data.name ?? p.id}
                    </Link>
                    {data.code && (
                      <div style={{ fontSize: 11, color: "#6b7280" }}>
                        {data.code}
                      </div>
                    )}
                  </td>
                  <td style={td()}>
                    <Badge status={data.status ?? "planning"} />
                  </td>
                  <td style={{ ...td(), padding: "8px 0" }}>
                    <div style={ganttRow()}>
                      <div
                        style={{
                          ...ganttBar(),
                          left: `${left}%`,
                          width: `${width}%`,
                          background:
                            STATUS_COLOR[data.status ?? "planning"] ?? "#9ca3af",
                        }}
                        title={`${data.start_date ?? "?"} → ${data.end_date ?? "?"}`}
                      />
                      {ms.map((m) => {
                        const md = (m.data as MilestoneData) ?? {};
                        if (!md.due_date) return null;
                        const x = pct(new Date(md.due_date), rangeStart, totalDays);
                        return (
                          <div
                            key={m.id}
                            style={{
                              ...ganttMarker(),
                              left: `${x}%`,
                              background:
                                STATUS_COLOR[md.status ?? "planned"] ?? "#9ca3af",
                            }}
                            title={`${md.name ?? m.id} — ${md.due_date} (${md.status ?? ""})`}
                          />
                        );
                      })}
                    </div>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
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

function Badge({ status }: { status: string }) {
  return (
    <span
      style={{
        display: "inline-block",
        padding: "2px 8px",
        borderRadius: 4,
        fontSize: 11,
        background: STATUS_COLOR[status] ?? "#9ca3af",
        color: "white",
      }}
    >
      {status}
    </span>
  );
}

function th(): React.CSSProperties {
  return {
    textAlign: "left",
    padding: 8,
    fontSize: 12,
    color: "#6b7280",
    textTransform: "uppercase",
    borderBottom: "1px solid #e5e7eb",
  };
}

function td(): React.CSSProperties {
  return { padding: 8, verticalAlign: "middle" };
}

function ganttRow(): React.CSSProperties {
  return {
    position: "relative",
    height: 18,
    background: "#f3f4f6",
    borderRadius: 4,
  };
}

function ganttBar(): React.CSSProperties {
  return {
    position: "absolute",
    top: 4,
    height: 10,
    borderRadius: 4,
    minWidth: 4,
  };
}

function ganttMarker(): React.CSSProperties {
  return {
    position: "absolute",
    top: 0,
    width: 4,
    height: 18,
    borderRadius: 1,
  };
}
