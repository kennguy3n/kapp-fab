import {
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Skeleton,
  Tooltip,
  TooltipContent,
  TooltipTrigger,
  cn,
  toast,
} from "@kapp/ui";
import { AlertTriangle, Flag, FolderKanban, Move, Plus } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken, recordLabel, statusVariant } from "../lib/ktypeView";

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

type Zoom = "day" | "week" | "month";

// Pixels per calendar day at each zoom level — drives the time-axis
// scale and bar geometry. Wider days at "day" zoom keep single-day
// tasks legible; "month" compresses long programmes onto one screen.
const ZOOM_PX: Record<Zoom, number> = { day: 34, week: 15, month: 6 };
const ZOOMS: readonly Zoom[] = ["day", "week", "month"];
const RAIL_WIDTH = 232;
const ROW_HEIGHT = 52;
const AXIS_HEIGHT = 36;
const DAY_MS = 86_400_000;

function startOfDay(d: Date): Date {
  const x = new Date(d);
  x.setHours(0, 0, 0, 0);
  return x;
}
function addDays(d: Date, n: number): Date {
  const x = new Date(d);
  x.setDate(x.getDate() + n);
  return x;
}
function diffDays(a: Date, b: Date): number {
  return Math.round((startOfDay(a).getTime() - startOfDay(b).getTime()) / DAY_MS);
}
function parseDate(value?: string): Date | null {
  if (!value) return null;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? null : startOfDay(d);
}
function toISODate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}
// Monday-anchored week start (ISO week) for tidy axis gridlines.
function startOfWeek(d: Date): Date {
  const x = startOfDay(d);
  const isoDow = (x.getDay() + 6) % 7;
  return addDays(x, -isoDow);
}

function milestoneTone(status: string): string {
  switch (status) {
    case "completed":
      return "bg-success";
    case "in_progress":
      return "bg-accent";
    default:
      return "bg-fg-subtle";
  }
}

/**
 * ProjectGanttPage renders a horizontally-scrollable timeline: one
 * row per project, bars themed to the KChat violet accent, milestone
 * diamonds along each bar, a "today" marker, and a zoomable time axis
 * (day / week / month). Bars are draggable (pointer) and nudgeable
 * (keyboard arrows) to reschedule, persisted via `updateRecord` with
 * an optimistic cache update and rollback on failure.
 *
 * Dependency links and critical-path emphasis are intentionally NOT
 * drawn: the project/milestone schema carries no predecessor edges,
 * so any connector would be fabricated. Progress fill is derived from
 * real milestone completion (weighted) instead.
 */
export function ProjectGanttPage() {
  const fmt = useFormatter();
  const qc = useQueryClient();
  const [zoom, setZoom] = useState<Zoom>("week");
  const [drag, setDrag] = useState<{ id: string; deltaDays: number } | null>(
    null,
  );
  const dragOrigin = useRef<{ x: number; id: string } | null>(null);

  const projectsQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_PROJECT],
    queryFn: () => api.listRecords(KTYPE_PROJECT),
  });
  const milestonesQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_MILESTONE],
    queryFn: () => api.listRecords(KTYPE_MILESTONE),
  });

  const pxPerDay = ZOOM_PX[zoom];

  const { rangeStart, totalDays } = useMemo(() => {
    const projects = projectsQ.data ?? [];
    const times: number[] = [];
    for (const p of projects) {
      const data = p.data as ProjectData;
      const s = parseDate(data.start_date);
      const e = parseDate(data.end_date);
      if (s) times.push(s.getTime());
      if (e) times.push(e.getTime());
    }
    const today = startOfDay(new Date());
    times.push(today.getTime());
    const min = new Date(Math.min(...times));
    const max = new Date(Math.max(...times));
    const start = startOfWeek(addDays(min, -3));
    const end = addDays(startOfWeek(addDays(max, 10)), 6);
    return { rangeStart: start, totalDays: Math.max(7, diffDays(end, start) + 1) };
  }, [projectsQ.data]);

  const timelineWidth = totalDays * pxPerDay;

  const ticks = useMemo(() => {
    const out: { left: number; label: string; major: boolean }[] = [];
    for (let i = 0; i < totalDays; i++) {
      const d = addDays(rangeStart, i);
      const left = i * pxPerDay;
      if (zoom === "day") {
        out.push({ left, label: String(d.getDate()), major: d.getDate() === 1 });
      } else if (zoom === "week") {
        if ((d.getDay() + 6) % 7 === 0) {
          out.push({
            left,
            label: fmt.date(d, { month: "short", day: "numeric" }),
            major: d.getDate() <= 7,
          });
        }
      } else if (d.getDate() === 1) {
        out.push({
          left,
          label: fmt.date(d, { month: "short", year: "numeric" }),
          major: true,
        });
      }
    }
    return out;
  }, [rangeStart, totalDays, pxPerDay, zoom, fmt]);

  const todayLeft = useMemo(() => {
    const i = diffDays(new Date(), rangeStart);
    return i >= 0 && i < totalDays ? i * pxPerDay : null;
  }, [rangeStart, totalDays, pxPerDay]);

  const milestonesByProject = useMemo(() => {
    const map = new Map<string, KRecord[]>();
    for (const m of milestonesQ.data ?? []) {
      const pid = (m.data as MilestoneData).project_id;
      if (!pid) continue;
      const list = map.get(pid) ?? [];
      list.push(m);
      map.set(pid, list);
    }
    return map;
  }, [milestonesQ.data]);

  function progressPct(project: KRecord): number {
    const data = project.data as ProjectData;
    if (data.status === "completed") return 100;
    const ms = milestonesByProject.get(project.id) ?? [];
    if (ms.length === 0) return 0;
    let done = 0;
    let total = 0;
    for (const m of ms) {
      const md = m.data as MilestoneData;
      const w = typeof md.weight === "number" && md.weight > 0 ? md.weight : 1;
      total += w;
      if (md.status === "completed") done += w;
    }
    return total === 0 ? 0 : Math.round((done / total) * 100);
  }

  function canSchedule(project: KRecord): boolean {
    const data = project.data as ProjectData;
    return parseDate(data.start_date) !== null && parseDate(data.end_date) !== null;
  }

  async function commitReschedule(
    project: KRecord,
    deltaDays: number,
    withToast: boolean,
  ) {
    if (deltaDays === 0) return;
    const data = project.data as ProjectData;
    const s = parseDate(data.start_date);
    const e = parseDate(data.end_date);
    if (!s || !e) return;
    const ns = addDays(s, deltaDays);
    const ne = addDays(e, deltaDays);
    const patch = { start_date: toISODate(ns), end_date: toISODate(ne) };
    const key = ["records", KTYPE_PROJECT];
    const prev = qc.getQueryData<KRecord[]>(key);
    qc.setQueryData<KRecord[]>(key, (old) =>
      old?.map((r) =>
        r.id === project.id ? { ...r, data: { ...r.data, ...patch } } : r,
      ) ?? old,
    );
    try {
      const saved = await api.updateRecord(KTYPE_PROJECT, project.id, patch);
      qc.setQueryData<KRecord[]>(key, (old) =>
        old?.map((r) => (r.id === project.id ? saved : r)) ?? old,
      );
      if (withToast) {
        toast.success(
          `Moved “${recordLabel(project)}” to ${fmt.date(ns, {
            month: "short",
            day: "numeric",
          })} – ${fmt.date(ne, { month: "short", day: "numeric", year: "numeric" })}`,
        );
      }
    } catch (err) {
      if (prev) qc.setQueryData(key, prev);
      toast.error(`Couldn't reschedule: ${(err as Error).message}`);
    }
  }

  function onBarPointerDown(e: ReactPointerEvent<HTMLDivElement>, p: KRecord) {
    if (!canSchedule(p)) return;
    e.currentTarget.setPointerCapture(e.pointerId);
    dragOrigin.current = { x: e.clientX, id: p.id };
    setDrag({ id: p.id, deltaDays: 0 });
    e.preventDefault();
  }
  function onBarPointerMove(e: ReactPointerEvent<HTMLDivElement>) {
    const origin = dragOrigin.current;
    if (!origin) return;
    const deltaDays = Math.round((e.clientX - origin.x) / pxPerDay);
    setDrag((cur) =>
      cur && cur.id === origin.id ? { ...cur, deltaDays } : cur,
    );
  }
  function onBarPointerUp(e: ReactPointerEvent<HTMLDivElement>, p: KRecord) {
    const origin = dragOrigin.current;
    const current = drag;
    dragOrigin.current = null;
    setDrag(null);
    if (e.currentTarget.hasPointerCapture(e.pointerId))
      e.currentTarget.releasePointerCapture(e.pointerId);
    if (origin && current && current.id === p.id && current.deltaDays !== 0)
      void commitReschedule(p, current.deltaDays, true);
  }
  function onBarKeyDown(e: ReactKeyboardEvent<HTMLDivElement>, p: KRecord) {
    if (!canSchedule(p)) return;
    let delta = 0;
    if (e.key === "ArrowRight") delta = e.shiftKey ? 7 : 1;
    else if (e.key === "ArrowLeft") delta = e.shiftKey ? -7 : -1;
    else return;
    e.preventDefault();
    void commitReschedule(p, delta, false);
  }

  const isLoading = projectsQ.isLoading || milestonesQ.isLoading;
  const isError = projectsQ.isError || milestonesQ.isError;
  const projects = projectsQ.data ?? [];

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-1">
          <Eyebrow>Projects</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">
            Project timeline
          </h1>
          <p className="max-w-prose text-sm text-fg-muted">
            See every project on one schedule. Drag a bar — or focus it and use
            the arrow keys — to move its dates.
          </p>
        </div>
        <div className="flex items-center gap-3">
          <div
            role="group"
            aria-label="Timeline zoom"
            className="flex items-center gap-1 rounded-pill bg-bg-muted p-1"
          >
            {ZOOMS.map((z) => (
              <Button
                key={z}
                size="sm"
                variant={zoom === z ? "primary" : "ghost"}
                aria-pressed={zoom === z}
                onClick={() => setZoom(z)}
              >
                {humanizeToken(z)}
              </Button>
            ))}
          </div>
          <Button asChild variant="secondary" size="sm">
            <Link to={`/records/${KTYPE_PROJECT}/new`}>
              <Plus className="h-4 w-4" aria-hidden />
              New project
            </Link>
          </Button>
        </div>
      </header>

      {isLoading ? (
        <GanttSkeleton />
      ) : isError ? (
        <EmptyState
          icon={<AlertTriangle className="h-6 w-6" aria-hidden />}
          title="Couldn't load the project timeline"
          description={
            (projectsQ.error as Error | null)?.message ??
            (milestonesQ.error as Error | null)?.message ??
            "Something went wrong while loading projects."
          }
          action={
            <Button
              variant="secondary"
              onClick={() => {
                void projectsQ.refetch();
                void milestonesQ.refetch();
              }}
              disabled={projectsQ.isFetching || milestonesQ.isFetching}
            >
              Retry
            </Button>
          }
        />
      ) : projects.length === 0 ? (
        <EmptyState
          icon={<FolderKanban className="h-6 w-6" aria-hidden />}
          title="No projects yet"
          description="Projects you create will appear here as a schedule — with milestones, progress, and a today marker — so you can see what's on track at a glance."
          action={
            <Button asChild>
              <Link to={`/records/${KTYPE_PROJECT}/new`}>
                <Plus className="h-4 w-4" aria-hidden />
                New project
              </Link>
            </Button>
          }
        />
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-fg-muted">
            <span className="font-medium text-fg-subtle">
              {projects.length}{" "}
              {projects.length === 1 ? "project" : "projects"}
            </span>
            <LegendSwatch className="bg-accent rounded-pill h-2.5 w-5">
              Project span
            </LegendSwatch>
            <LegendSwatch className="bg-success h-2.5 w-2.5 rotate-45">
              Completed milestone
            </LegendSwatch>
            <LegendSwatch className="bg-accent h-2.5 w-2.5 rotate-45">
              In progress
            </LegendSwatch>
            <LegendSwatch className="bg-fg-subtle h-2.5 w-2.5 rotate-45">
              Planned
            </LegendSwatch>
            <span className="inline-flex items-center gap-1.5">
              <Move className="h-3.5 w-3.5" aria-hidden />
              Drag to reschedule
            </span>
          </div>

          <div className="overflow-x-auto rounded-lg border border-border bg-bg">
            <div style={{ minWidth: RAIL_WIDTH + timelineWidth }}>
              {/* Time axis */}
              <div className="flex border-b border-border bg-bg-subtle">
                <div
                  className="sticky left-0 z-30 flex shrink-0 items-center border-r border-border bg-bg-subtle px-3"
                  style={{ width: RAIL_WIDTH, height: AXIS_HEIGHT }}
                >
                  <span className="text-xs font-semibold uppercase tracking-wide text-fg-subtle">
                    Project
                  </span>
                </div>
                <div
                  className="relative shrink-0"
                  style={{ width: timelineWidth, height: AXIS_HEIGHT }}
                >
                  {ticks.map((t) => (
                    <div
                      key={t.left}
                      className={cn(
                        "absolute top-0 h-full border-l",
                        t.major ? "border-border-strong" : "border-border",
                      )}
                      style={{ left: t.left }}
                    >
                      <span
                        className={cn(
                          "absolute left-1 top-1.5 whitespace-nowrap text-xs",
                          t.major ? "font-medium text-fg" : "text-fg-subtle",
                        )}
                      >
                        {t.label}
                      </span>
                    </div>
                  ))}
                  {todayLeft !== null && (
                    <div
                      className="absolute top-0 z-10 -translate-x-1/2"
                      style={{ left: todayLeft }}
                    >
                      <span className="rounded-pill bg-danger px-1.5 py-0.5 text-xs font-semibold uppercase tracking-wide text-danger-fg">
                        Today
                      </span>
                    </div>
                  )}
                </div>
              </div>

              {/* Rows */}
              {projects.map((p) => {
                const data = p.data as ProjectData;
                const status = data.status ?? "planning";
                const start = parseDate(data.start_date);
                const end = parseDate(data.end_date);
                const schedulable = start !== null && end !== null;
                const barLeft = start ? diffDays(start, rangeStart) * pxPerDay : 0;
                const spanDays =
                  start && end ? Math.max(1, diffDays(end, start) + 1) : 0;
                const barWidth = Math.max(pxPerDay, spanDays * pxPerDay);
                const dragShift =
                  drag && drag.id === p.id ? drag.deltaDays * pxPerDay : 0;
                const ms = milestonesByProject.get(p.id) ?? [];
                const pct = progressPct(p);
                const name = recordLabel(p);
                return (
                  <div
                    key={p.id}
                    className="group flex border-b border-border last:border-b-0"
                  >
                    <div
                      className="sticky left-0 z-20 flex shrink-0 flex-col justify-center gap-1 border-r border-border bg-bg px-3 group-hover:bg-bg-subtle"
                      style={{ width: RAIL_WIDTH, height: ROW_HEIGHT }}
                    >
                      <Link
                        to={`/records/${KTYPE_PROJECT}/${p.id}`}
                        className="truncate text-sm font-medium text-fg hover:text-accent"
                        title={name}
                      >
                        {name}
                      </Link>
                      <div className="flex items-center gap-2">
                        <Badge variant={statusVariant(status)} size="sm">
                          {humanizeToken(status)}
                        </Badge>
                        {data.code && (
                          <span className="truncate text-xs text-fg-subtle">
                            {data.code}
                          </span>
                        )}
                      </div>
                    </div>

                    <div
                      className="relative shrink-0 group-hover:bg-bg-subtle"
                      style={{ width: timelineWidth, height: ROW_HEIGHT }}
                    >
                      {todayLeft !== null && (
                        <div
                          className="pointer-events-none absolute inset-y-0 z-0 w-px bg-danger/70"
                          style={{ left: todayLeft }}
                          aria-hidden
                        />
                      )}

                      {schedulable ? (
                        <Tooltip>
                          <TooltipTrigger asChild>
                            <div
                              role="button"
                              tabIndex={0}
                              aria-label={`${name}: ${fmt.date(start!, {
                                month: "long",
                                day: "numeric",
                                year: "numeric",
                              })} to ${fmt.date(end!, {
                                month: "long",
                                day: "numeric",
                                year: "numeric",
                              })}, ${pct}% complete. Use arrow keys to reschedule.`}
                              onPointerDown={(e) => onBarPointerDown(e, p)}
                              onPointerMove={onBarPointerMove}
                              onPointerUp={(e) => onBarPointerUp(e, p)}
                              onKeyDown={(e) => onBarKeyDown(e, p)}
                              className={cn(
                                "absolute top-1/2 z-10 h-5 -translate-y-1/2 touch-none rounded-pill bg-accent/20",
                                "cursor-grab outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-1 focus-visible:ring-offset-(--bg)",
                                drag?.id === p.id &&
                                  "cursor-grabbing ring-2 ring-(--focus-ring)",
                              )}
                              style={{
                                left: barLeft + dragShift,
                                width: barWidth,
                              }}
                            >
                              <div
                                className="absolute inset-y-0 left-0 rounded-pill bg-accent"
                                style={{ width: `${pct}%` }}
                              />
                              {ms.map((m) => {
                                const md = m.data as MilestoneData;
                                const due = parseDate(md.due_date);
                                if (!due) return null;
                                const offset = diffDays(due, start!);
                                const left = offset * pxPerDay;
                                const within = offset >= 0 && offset <= spanDays;
                                if (!within) return null;
                                return (
                                  <span
                                    key={m.id}
                                    aria-hidden
                                    className={cn(
                                      "absolute top-1/2 h-2.5 w-2.5 -translate-x-1/2 -translate-y-1/2 rotate-45 rounded-xs border border-bg",
                                      milestoneTone(md.status ?? "planned"),
                                    )}
                                    style={{ left }}
                                  />
                                );
                              })}
                            </div>
                          </TooltipTrigger>
                          <TooltipContent className="max-w-xs">
                            <p className="font-medium">{name}</p>
                            <p className="text-fg-muted">
                              {fmt.date(start!, {
                                month: "short",
                                day: "numeric",
                              })}{" "}
                              –{" "}
                              {fmt.date(end!, {
                                month: "short",
                                day: "numeric",
                                year: "numeric",
                              })}{" "}
                              · {pct}% complete
                            </p>
                            {ms.length > 0 && (
                              <ul className="mt-1 flex flex-col gap-0.5">
                                {ms.map((m) => {
                                  const md = m.data as MilestoneData;
                                  const due = parseDate(md.due_date);
                                  return (
                                    <li
                                      key={m.id}
                                      className="flex items-center gap-1.5 text-fg-muted"
                                    >
                                      <Flag
                                        className="h-3 w-3 shrink-0"
                                        aria-hidden
                                      />
                                      <span className="truncate">
                                        {recordLabel(m)}
                                      </span>
                                      {due && (
                                        <span className="shrink-0 text-fg-subtle">
                                          ·{" "}
                                          {fmt.date(due, {
                                            month: "short",
                                            day: "numeric",
                                          })}
                                        </span>
                                      )}
                                    </li>
                                  );
                                })}
                              </ul>
                            )}
                          </TooltipContent>
                        </Tooltip>
                      ) : (
                        <span className="absolute left-3 top-1/2 -translate-y-1/2 text-xs italic text-fg-subtle">
                          No dates set
                        </span>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        </>
      )}
    </section>
  );
}

function LegendSwatch({
  className,
  children,
}: {
  className: string;
  children: React.ReactNode;
}) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={cn("inline-block shrink-0", className)} aria-hidden />
      {children}
    </span>
  );
}

function GanttSkeleton() {
  return (
    <div className="flex flex-col gap-3">
      <Skeleton className="h-4 w-40" />
      <div className="rounded-lg border border-border">
        <Skeleton className="h-9 w-full rounded-none rounded-t-lg" />
        {Array.from({ length: 5 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-3 border-t border-border px-3 py-3"
          >
            <Skeleton className="h-8 w-44 shrink-0" />
            <Skeleton
              className="h-5"
              style={{ width: `${30 + ((i * 17) % 50)}%` }}
            />
          </div>
        ))}
      </div>
    </div>
  );
}
