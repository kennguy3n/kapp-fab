import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { CourseAnalytics, KRecord } from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Field,
  Select,
  Skeleton,
  StatCard,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  initials,
} from "@kapp/ui";
import {
  AlertTriangle,
  BarChart3,
  CheckCircle2,
  Percent,
  Trophy,
  Users,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken, statusVariant } from "../lib/ktypeView";
import {
  BarChart,
  LmsPageHeader,
  ProgressBar,
  SegmentBar,
  pct,
  type BarDatum,
} from "../components/lms/primitives";

/**
 * InstructorDashboardPage lets an instructor pick a course and review
 * its aggregate analytics from `/api/v1/lms/courses/{id}/analytics`:
 * headline KPIs, a completion funnel, per-lesson drop-off, a score
 * distribution, at-risk learners, and a per-learner progress table.
 * The course list comes from the generic KRecord surface so the page
 * works even before any analytics rows exist.
 */
const SCORE_BUCKETS: { label: string; min: number; max: number }[] = [
  { label: "0–59", min: 0, max: 60 },
  { label: "60–69", min: 60, max: 70 },
  { label: "70–79", min: 70, max: 80 },
  { label: "80–89", min: 80, max: 90 },
  { label: "90–100", min: 90, max: 101 },
];

export function InstructorDashboardPage() {
  const fmt = useFormatter();
  const [courseId, setCourseId] = useState<string>("");

  const coursesQ = useQuery({
    queryKey: ["records", "lms.course"],
    queryFn: () => api.listRecords("lms.course"),
  });
  const lessonsQ = useQuery({
    queryKey: ["records", "lms.lesson"],
    queryFn: () => api.listRecords("lms.lesson"),
  });
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const analyticsQ = useQuery({
    queryKey: ["lms", "analytics", courseId],
    queryFn: () => api.getCourseAnalytics(courseId),
    enabled: !!courseId,
  });

  const courseTitleById = useMemo(() => {
    const m = new Map<string, string>();
    (coursesQ.data ?? []).forEach((c) => {
      const d = c.data as Record<string, unknown>;
      m.set(c.id, typeof d.title === "string" ? d.title : "Untitled course");
    });
    return m;
  }, [coursesQ.data]);

  const lessonTitleById = useMemo(() => {
    const m = new Map<string, string>();
    (lessonsQ.data ?? []).forEach((l) => {
      const d = l.data as Record<string, unknown>;
      if (typeof d.title === "string") m.set(l.id, d.title);
    });
    return m;
  }, [lessonsQ.data]);

  const employeeNameById = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((e: KRecord) => {
      const d = e.data as Record<string, unknown>;
      if (typeof d.name === "string") m.set(e.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const a: CourseAnalytics | undefined = analyticsQ.data ?? undefined;

  const selector = (
    <Field label="Course" className="w-full max-w-sm">
      <Select value={courseId} onChange={(e) => setCourseId(e.target.value)}>
        <option value="">Select a course…</option>
        {(coursesQ.data ?? []).map((c) => (
          <option key={c.id} value={c.id}>
            {courseTitleById.get(c.id)}
          </option>
        ))}
      </Select>
    </Field>
  );

  return (
    <section className="flex flex-col gap-6">
      <LmsPageHeader
        area="Instructor"
        title="Instructor Dashboard"
        description="Course-level analytics: enrollment, completion, drop-off, and per-learner progress."
      />

      {selector}

      {!courseId ? (
        <EmptyState
          icon={<BarChart3 />}
          title="Choose a course"
          description="Pick a course above to see its completion funnel, drop-off, and learner progress."
        />
      ) : analyticsQ.isLoading ? (
        <DashboardSkeleton />
      ) : analyticsQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load analytics"
          description={(analyticsQ.error as Error).message}
          action={
            <Button variant="secondary" onClick={() => analyticsQ.refetch()}>
              Try again
            </Button>
          }
        />
      ) : a ? (
        <CourseAnalyticsView
          analytics={a}
          lessonTitleById={lessonTitleById}
          employeeNameById={employeeNameById}
          fmt={fmt}
        />
      ) : (
        <EmptyState
          icon={<BarChart3 />}
          title="No analytics yet"
          description="This course doesn't have any learner activity to report yet."
        />
      )}
    </section>
  );
}

function CourseAnalyticsView({
  analytics: a,
  lessonTitleById,
  employeeNameById,
  fmt,
}: {
  analytics: CourseAnalytics;
  lessonTitleById: Map<string, string>;
  employeeNameById: Map<string, string>;
  fmt: ReturnType<typeof useFormatter>;
}) {
  // All per-course derivations are memoized so they don't recompute when an
  // unrelated parent re-render occurs; they only depend on the analytics
  // payload, the formatter, and the resolved employee names.
  const { funnel, scored, scoreDistribution, maxReached, atRisk, learnerName } =
    useMemo(() => {
      const inProgress = Math.max(0, a.enrollment_count - a.completed_count);
      const funnel: BarDatum[] = [
        {
          label: "Enrolled",
          value: a.enrollment_count,
          tone: "accent",
          hint: fmt.number(a.enrollment_count),
        },
        {
          label: "In progress",
          value: inProgress,
          tone: "info",
          hint: fmt.number(inProgress),
        },
        {
          label: "Completed",
          value: a.completed_count,
          tone: "success",
          hint: fmt.number(a.completed_count),
        },
      ];

      const scored = a.per_learner.filter(
        (l): l is typeof l & { average_score: number } =>
          l.average_score != null,
      );
      const scoreDistribution: BarDatum[] = SCORE_BUCKETS.map((bucket) => {
        const count = scored.filter(
          (l) => l.average_score >= bucket.min && l.average_score < bucket.max,
        ).length;
        return { label: bucket.label, value: count, hint: fmt.number(count) };
      });

      const maxReached = Math.max(
        1,
        ...a.lesson_drop_off.map((l) => l.reached),
      );

      const atRisk = a.per_learner.filter((l) => {
        const ratio =
          l.lessons_total > 0 ? l.lessons_completed / l.lessons_total : 0;
        const lowScore = l.average_score != null && l.average_score < 60;
        return l.status !== "completed" && (ratio < 0.5 || lowScore);
      });

      // Assign a stable fallback number to each distinct UNNAMED learner by
      // their first appearance in the full per-learner list, so the same
      // unnamed learner shows the same "Learner N" in both the at-risk card and
      // the full table. The counter only increments for unnamed learners, so
      // the numbering stays contiguous even when some learners resolve to a
      // real name.
      const nameByUser = new Map<string, string>();
      let unknownCount = 0;
      a.per_learner.forEach((l) => {
        if (nameByUser.has(l.user_id)) return;
        const known = employeeNameById.get(l.user_id);
        if (known) {
          nameByUser.set(l.user_id, known);
        } else {
          unknownCount += 1;
          nameByUser.set(l.user_id, `Learner ${unknownCount}`);
        }
      });
      const learnerName = (userId: string) =>
        nameByUser.get(userId) ?? "Learner";

      return {
        funnel,
        scored,
        scoreDistribution,
        maxReached,
        atRisk,
        learnerName,
      };
    }, [a, fmt, employeeNameById]);

  return (
    <div className="flex flex-col gap-6">
      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <StatCard
          label="Enrollments"
          value={fmt.number(a.enrollment_count)}
          icon={<Users />}
        />
        <StatCard
          label="Completed"
          value={fmt.number(a.completed_count)}
          icon={<CheckCircle2 />}
        />
        <StatCard
          label="Completion rate"
          value={fmt.number(a.completion_rate, {
            style: "percent",
            maximumFractionDigits: 1,
          })}
          icon={<Percent />}
        />
        <StatCard
          label="Average score"
          value={a.average_score != null ? fmt.number(a.average_score) : "—"}
          icon={<Trophy />}
        />
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Completion funnel</CardTitle>
          </CardHeader>
          <CardContent>
            <BarChart data={funnel} max={a.enrollment_count || 1} />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Score distribution</CardTitle>
          </CardHeader>
          <CardContent>
            {scored.length === 0 ? (
              <p className="text-sm text-fg-muted">No graded learners yet.</p>
            ) : (
              <BarChart data={scoreDistribution} />
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Lesson drop-off</CardTitle>
        </CardHeader>
        <CardContent>
          {a.lesson_drop_off.length === 0 ? (
            <p className="text-sm text-fg-muted">No lesson progress yet.</p>
          ) : (
            <ul className="flex flex-col gap-4">
              {a.lesson_drop_off.map((l) => {
                const dropped = Math.max(0, l.reached - l.completed);
                const high = l.drop_off_rate >= 0.3;
                return (
                  <li key={l.lesson_id} className="flex flex-col gap-1.5">
                    <div className="flex items-center justify-between gap-3">
                      <span className="min-w-0 truncate text-sm font-medium text-fg">
                        {lessonTitleById.get(l.lesson_id) ?? "Untitled lesson"}
                      </span>
                      <Badge variant={high ? "warning" : "neutral"} size="sm">
                        {fmt.number(l.drop_off_rate, {
                          style: "percent",
                          maximumFractionDigits: 0,
                        })}{" "}
                        drop-off
                      </Badge>
                    </div>
                    <SegmentBar
                      max={maxReached}
                      segments={[
                        { value: l.completed, tone: "success" },
                        { value: dropped, tone: "warning" },
                      ]}
                    />
                    <span className="font-tabular text-xs text-fg-muted">
                      {fmt.number(l.reached)} reached · {fmt.number(l.completed)}{" "}
                      completed
                    </span>
                  </li>
                );
              })}
            </ul>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>At-risk learners</CardTitle>
        </CardHeader>
        <CardContent>
          {atRisk.length === 0 ? (
            <div className="flex items-center gap-2 text-sm text-fg-muted">
              <CheckCircle2 className="h-4 w-4 text-success" aria-hidden />
              No at-risk learners right now.
            </div>
          ) : (
            <ul className="flex flex-col gap-3">
              {atRisk.map((l) => {
                const name = learnerName(l.user_id);
                const ratio =
                  l.lessons_total > 0
                    ? (l.lessons_completed / l.lessons_total) * 100
                    : 0;
                return (
                  <li
                    key={l.enrollment_id}
                    className="flex items-center gap-3 rounded-md border border-border bg-bg-subtle p-3"
                  >
                    <Avatar size="sm">
                      <AvatarFallback>{initials(name)}</AvatarFallback>
                    </Avatar>
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center justify-between gap-2">
                        <span className="truncate text-sm font-medium text-fg">
                          {name}
                        </span>
                        <span className="font-tabular text-xs text-fg-muted">
                          {l.lessons_completed}/{l.lessons_total}
                        </span>
                      </div>
                      <ProgressBar
                        value={ratio}
                        tone="warning"
                        className="mt-1.5"
                        label={`${name}: ${pct(ratio)}% complete`}
                      />
                    </div>
                    {l.average_score != null ? (
                      <Badge
                        variant={l.average_score < 60 ? "danger" : "neutral"}
                        size="sm"
                      >
                        {fmt.number(l.average_score)}
                      </Badge>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Per-learner progress</CardTitle>
        </CardHeader>
        <CardContent>
          {a.per_learner.length === 0 ? (
            <p className="text-sm text-fg-muted">No learners enrolled yet.</p>
          ) : (
            <div className="overflow-hidden rounded-lg border border-border">
              <Table>
                <TableHeader className="sticky top-0 bg-bg-subtle">
                  <TableRow>
                    <TableHead>Learner</TableHead>
                    <TableHead className="w-32">Status</TableHead>
                    <TableHead className="w-48">Lessons</TableHead>
                    <TableHead className="w-24 text-end">Avg score</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {a.per_learner.map((l) => {
                    const name = learnerName(l.user_id);
                    const ratio =
                      l.lessons_total > 0
                        ? (l.lessons_completed / l.lessons_total) * 100
                        : 0;
                    return (
                      <TableRow key={l.enrollment_id}>
                        <TableCell>
                          <span className="flex items-center gap-2">
                            <Avatar size="xs">
                              <AvatarFallback>{initials(name)}</AvatarFallback>
                            </Avatar>
                            <span className="font-medium text-fg">{name}</span>
                          </span>
                        </TableCell>
                        <TableCell>
                          <Badge variant={statusVariant(l.status)} size="sm">
                            {humanizeToken(l.status)}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-2">
                            <ProgressBar
                              value={ratio}
                              tone={
                                l.status === "completed" ? "success" : "accent"
                              }
                              className="w-24"
                              label={`${name}: ${pct(ratio)}% complete`}
                            />
                            <span className="font-tabular text-xs text-fg-muted">
                              {l.lessons_completed}/{l.lessons_total}
                            </span>
                          </div>
                        </TableCell>
                        <TableCell className="text-end font-tabular">
                          {l.average_score != null
                            ? fmt.number(l.average_score)
                            : "—"}
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function DashboardSkeleton() {
  return (
    <div className="flex flex-col gap-6">
      <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <Skeleton key={i} variant="rect" className="h-24 w-full" />
        ))}
      </div>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Skeleton variant="rect" className="h-48 w-full" />
        <Skeleton variant="rect" className="h-48 w-full" />
      </div>
      <Skeleton variant="rect" className="h-64 w-full" />
    </div>
  );
}

export default InstructorDashboardPage;
