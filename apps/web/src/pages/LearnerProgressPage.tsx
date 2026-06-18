import { useMemo } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Button,
  Card,
  EmptyState,
  Skeleton,
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
  ArrowLeft,
  CheckCircle2,
  Circle,
  Clock,
  GraduationCap,
  Inbox,
  PlayCircle,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken, statusVariant } from "../lib/ktypeView";
import {
  CoverArt,
  LmsPageHeader,
  ProgressBar,
  ProgressRing,
  pct,
} from "../components/lms/primitives";

/**
 * LearnerProgressPage shows per-lesson completion, scores, and overall
 * course progress for a single enrollment.
 *
 * Route shape:
 *   /lms/progress                 — index: a card per enrollment with a
 *                                   cover, progress ring, and status.
 *   /lms/progress/:enrollmentId   — detail for one enrollment.
 *
 * The page reads enrollment / course / module / lesson / progress as
 * KRecords via the generic `/records/{ktype}` surface and humanizes the
 * raw metadata (relations, enum tokens, timestamps) into the learner-
 * facing presentation; it gracefully reports an empty state when the
 * lms.progress rows are missing.
 */
export function LearnerProgressPage() {
  const { enrollmentId } = useParams<{ enrollmentId?: string }>();
  if (enrollmentId) {
    return <LearnerProgressDetail enrollmentId={enrollmentId} />;
  }
  return <LearnerProgressIndex />;
}

type Data = Record<string, unknown>;

function asData(record: KRecord | undefined): Data {
  return (record?.data as Data | undefined) ?? {};
}

function stringField(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** Parse a numeric field, returning null when absent or unparseable. */
function numericField(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string" && value.trim() !== "") {
    const n = Number(value);
    if (!Number.isNaN(n)) return n;
  }
  return null;
}

/** Sort key for an optional `order` field: missing values sort last. */
function orderKey(value: unknown): number {
  return numericField(value) ?? Number.MAX_SAFE_INTEGER;
}

interface LessonProgress {
  percent: number;
  completed: boolean;
  started: boolean;
  score?: number;
  completedAt?: string;
}

/**
 * Derive a learner's progress on a lesson from the lms.progress row,
 * tolerating either an explicit `status` enum or a `percent_complete`
 * measure (the two shapes the store exposes), so the UI never depends
 * on a single backing field.
 */
function lessonProgressOf(prog: KRecord | undefined): LessonProgress {
  const d = asData(prog);
  const status = stringField(d.status);
  const rawPercent =
    typeof d.percent_complete === "number"
      ? d.percent_complete
      : typeof d.percent_complete === "string"
        ? Number(d.percent_complete)
        : NaN;
  const completedAt = stringField(d.completed_at) || undefined;
  const completed =
    status === "completed" ||
    (Number.isFinite(rawPercent) && rawPercent >= 100) ||
    !!completedAt;
  const percent = completed
    ? 100
    : Number.isFinite(rawPercent)
      ? pct(rawPercent)
      : status === "in_progress"
        ? 50
        : 0;
  const started = completed || percent > 0 || status === "in_progress";
  const scoreNum =
    typeof d.score === "number"
      ? d.score
      : typeof d.score === "string" && d.score !== ""
        ? Number(d.score)
        : NaN;
  return {
    percent,
    completed,
    started,
    score: Number.isFinite(scoreNum) ? scoreNum : undefined,
    completedAt,
  };
}

function lessonStatusToken(p: LessonProgress): string {
  if (p.completed) return "completed";
  if (p.started) return "in_progress";
  return "not_started";
}

function orderedModulesForCourse(modules: KRecord[], courseId: string): KRecord[] {
  return modules
    .filter((m) => asData(m).course_id === courseId)
    .sort((a, b) => orderKey(asData(a).order) - orderKey(asData(b).order));
}

function groupLessonsByModule(lessons: KRecord[]): Map<string, KRecord[]> {
  const byModule = new Map<string, KRecord[]>();
  lessons.forEach((l) => {
    const moduleId = stringField(asData(l).module_id);
    if (!moduleId) return;
    const bucket = byModule.get(moduleId) ?? [];
    bucket.push(l);
    byModule.set(moduleId, bucket);
  });
  byModule.forEach((list) =>
    list.sort((a, b) => orderKey(asData(a).order) - orderKey(asData(b).order)),
  );
  return byModule;
}

function indexProgress(progress: KRecord[], enrollmentId: string): Map<string, KRecord> {
  const byLesson = new Map<string, KRecord>();
  progress.forEach((p) => {
    const d = asData(p);
    if (d.enrollment_id !== enrollmentId) return;
    const lessonId = stringField(d.lesson_id);
    if (lessonId) byLesson.set(lessonId, p);
  });
  return byLesson;
}

function useLearnerNames() {
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });
  return useMemo(() => {
    const byId = new Map<string, string>();
    (employeesQ.data ?? []).forEach((e) => {
      const name = stringField(asData(e).name);
      if (name) byId.set(e.id, name);
    });
    return byId;
  }, [employeesQ.data]);
}

function learnerIdOf(enrollmentData: Data): string {
  return stringField(enrollmentData.employee_id) || stringField(enrollmentData.user_id);
}

function LearnerProgressIndex() {
  const enrollmentsQ = useQuery({
    queryKey: ["records", "lms.enrollment"],
    queryFn: () => api.listRecords("lms.enrollment"),
  });
  const coursesQ = useQuery({
    queryKey: ["records", "lms.course"],
    queryFn: () => api.listRecords("lms.course"),
  });
  const modulesQ = useQuery({
    queryKey: ["records", "lms.module"],
    queryFn: () => api.listRecords("lms.module"),
  });
  const lessonsQ = useQuery({
    queryKey: ["records", "lms.lesson"],
    queryFn: () => api.listRecords("lms.lesson"),
  });
  const progressQ = useQuery({
    queryKey: ["records", "lms.progress"],
    queryFn: () => api.listRecords("lms.progress"),
  });
  const learnerNames = useLearnerNames();

  const courseById = useMemo(() => {
    const byId = new Map<string, KRecord>();
    (coursesQ.data ?? []).forEach((c) => byId.set(c.id, c));
    return byId;
  }, [coursesQ.data]);

  // Build all enrollment summaries in a single pass so the index grid stays
  // O(enrollments + records) instead of recomputing the lesson/progress maps
  // for every card on each render.
  const summaries = useMemo(() => {
    const modules = modulesQ.data ?? [];
    const lessons = lessonsQ.data ?? [];
    const progress = progressQ.data ?? [];

    const lessonsByModule = groupLessonsByModule(lessons);
    const lessonsByCourse = new Map<string, KRecord[]>();
    const courseLessons = (courseId: string): KRecord[] => {
      let list = lessonsByCourse.get(courseId);
      if (!list) {
        list = orderedModulesForCourse(modules, courseId).flatMap(
          (m) => lessonsByModule.get(m.id) ?? [],
        );
        lessonsByCourse.set(courseId, list);
      }
      return list;
    };

    const progressByEnrollment = new Map<string, Map<string, KRecord>>();
    progress.forEach((p) => {
      const d = asData(p);
      const enrollmentId = stringField(d.enrollment_id);
      const lessonId = stringField(d.lesson_id);
      if (!enrollmentId || !lessonId) return;
      let byLesson = progressByEnrollment.get(enrollmentId);
      if (!byLesson) {
        byLesson = new Map<string, KRecord>();
        progressByEnrollment.set(enrollmentId, byLesson);
      }
      byLesson.set(lessonId, p);
    });

    const byEnrollment = new Map<
      string,
      { completed: number; total: number; percent: number }
    >();
    (enrollmentsQ.data ?? []).forEach((e) => {
      const courseId = stringField(asData(e).course_id);
      const list = courseLessons(courseId);
      const byLesson = progressByEnrollment.get(e.id);
      const completed = list.filter(
        (l) => lessonProgressOf(byLesson?.get(l.id)).completed,
      ).length;
      const total = list.length;
      byEnrollment.set(e.id, {
        completed,
        total,
        percent: total > 0 ? pct((completed / total) * 100) : 0,
      });
    });
    return byEnrollment;
  }, [enrollmentsQ.data, modulesQ.data, lessonsQ.data, progressQ.data]);

  const enrollments = enrollmentsQ.data ?? [];

  return (
    <section className="flex flex-col gap-6">
      <LmsPageHeader
        area="Learning"
        title="Learner Progress"
        description="Track every enrollment at a glance, then open one to see per-lesson completion and scores."
      />

      {enrollmentsQ.isLoading ? (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <Card key={i} className="overflow-hidden">
              <Skeleton variant="rect" className="aspect-[16/9] w-full" />
              <div className="flex flex-col gap-3 p-4">
                <Skeleton variant="text" className="h-5 w-3/4" />
                <Skeleton variant="text" className="h-4 w-1/2" />
                <Skeleton variant="rect" className="h-10 w-full" />
              </div>
            </Card>
          ))}
        </div>
      ) : enrollmentsQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load enrollments"
          description={(enrollmentsQ.error as Error).message}
          action={
            <Button variant="secondary" onClick={() => enrollmentsQ.refetch()}>
              Try again
            </Button>
          }
        />
      ) : enrollments.length === 0 ? (
        <EmptyState
          icon={<Inbox />}
          title="No enrollments yet"
          description="When learners enroll in a course, their progress will show up here."
        />
      ) : (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {enrollments.map((e) => {
            const d = asData(e);
            const courseId = stringField(d.course_id);
            const course = courseById.get(courseId);
            const courseTitle = stringField(asData(course).title) || "Untitled course";
            const courseCode = stringField(asData(course).code);
            const learnerId = learnerIdOf(d);
            const learnerName = learnerNames.get(learnerId) || "Unassigned learner";
            const status = stringField(d.status);
            const summary = summaries.get(e.id) ?? {
              completed: 0,
              total: 0,
              percent: 0,
            };
            return (
              <Card key={e.id} className="flex flex-col overflow-hidden">
                <CoverArt seed={courseCode || courseTitle} icon={GraduationCap} />
                <div className="flex flex-1 flex-col gap-4 p-4">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <h3 className="truncate text-base font-semibold text-fg">
                        {courseTitle}
                      </h3>
                      {courseCode ? (
                        <p className="mt-0.5 font-tabular text-xs text-fg-subtle">
                          {courseCode}
                        </p>
                      ) : null}
                    </div>
                    <ProgressRing
                      value={summary.percent}
                      size={52}
                      label={`${courseTitle}: ${summary.percent}% complete`}
                    />
                  </div>

                  <div className="flex items-center gap-2">
                    <Avatar size="sm">
                      <AvatarFallback>{initials(learnerName)}</AvatarFallback>
                    </Avatar>
                    <span className="min-w-0 truncate text-sm text-fg-muted">
                      {learnerName}
                    </span>
                  </div>

                  <div className="mt-auto flex items-center justify-between gap-3">
                    {status ? (
                      <Badge variant={statusVariant(status)}>
                        {humanizeToken(status)}
                      </Badge>
                    ) : (
                      <span className="text-xs text-fg-subtle">
                        {summary.completed} of {summary.total} lessons
                      </span>
                    )}
                    <Button asChild variant="secondary" size="sm">
                      <Link to={`/lms/progress/${e.id}`}>View progress</Link>
                    </Button>
                  </div>
                </div>
              </Card>
            );
          })}
        </div>
      )}
    </section>
  );
}

function LearnerProgressDetail({ enrollmentId }: { enrollmentId: string }) {
  const fmt = useFormatter();
  const enrollmentsQ = useQuery({
    queryKey: ["records", "lms.enrollment"],
    queryFn: () => api.listRecords("lms.enrollment"),
  });
  const coursesQ = useQuery({
    queryKey: ["records", "lms.course"],
    queryFn: () => api.listRecords("lms.course"),
  });
  const modulesQ = useQuery({
    queryKey: ["records", "lms.module"],
    queryFn: () => api.listRecords("lms.module"),
  });
  const lessonsQ = useQuery({
    queryKey: ["records", "lms.lesson"],
    queryFn: () => api.listRecords("lms.lesson"),
  });
  const progressQ = useQuery({
    queryKey: ["records", "lms.progress"],
    queryFn: () => api.listRecords("lms.progress"),
  });
  const learnerNames = useLearnerNames();

  const enrollment = (enrollmentsQ.data ?? []).find((e) => e.id === enrollmentId);
  const enrollmentData = asData(enrollment);
  const courseId = stringField(enrollmentData.course_id);
  const course = (coursesQ.data ?? []).find((c) => c.id === courseId);
  const courseTitle = stringField(asData(course).title) || "Untitled course";
  const courseCode = stringField(asData(course).code);
  const status = stringField(enrollmentData.status);
  const learnerName =
    learnerNames.get(learnerIdOf(enrollmentData)) || "Unassigned learner";

  const courseModules = useMemo(
    () => orderedModulesForCourse(modulesQ.data ?? [], courseId),
    [modulesQ.data, courseId],
  );
  const lessonsByModule = useMemo(
    () => groupLessonsByModule(lessonsQ.data ?? []),
    [lessonsQ.data],
  );
  const progressByLesson = useMemo(
    () => indexProgress(progressQ.data ?? [], enrollmentId),
    [progressQ.data, enrollmentId],
  );

  const allLessons = courseModules.flatMap((m) => lessonsByModule.get(m.id) ?? []);
  const completedCount = allLessons.filter(
    (l) => lessonProgressOf(progressByLesson.get(l.id)).completed,
  ).length;
  const totalCount = allLessons.length;
  const percent = totalCount > 0 ? pct((completedCount / totalCount) * 100) : 0;

  const loading =
    enrollmentsQ.isLoading ||
    coursesQ.isLoading ||
    modulesQ.isLoading ||
    lessonsQ.isLoading ||
    progressQ.isLoading;

  const backButton = (
    <Button asChild variant="ghost" size="sm">
      <Link to="/lms/progress">
        <ArrowLeft className="h-4 w-4" aria-hidden />
        All enrollments
      </Link>
    </Button>
  );

  if (loading) {
    return (
      <section className="flex flex-col gap-6">
        {backButton}
        <Skeleton variant="text" className="h-8 w-64" />
        <Skeleton variant="rect" className="h-32 w-full" />
        <Skeleton variant="rect" className="h-48 w-full" />
      </section>
    );
  }

  if (!enrollment) {
    return (
      <section className="flex flex-col gap-6">
        {backButton}
        <EmptyState
          icon={<AlertTriangle />}
          title="Enrollment not found"
          description="This enrollment may have been removed, or the link is out of date."
          action={
            <Button asChild variant="secondary">
              <Link to="/lms/progress">Back to all enrollments</Link>
            </Button>
          }
        />
      </section>
    );
  }

  return (
    <section className="flex flex-col gap-6">
      {backButton}
      <LmsPageHeader area="Learning" title={courseTitle} />

      <Card className="flex flex-col gap-4 p-5 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-4">
          <div className="hidden w-32 shrink-0 sm:block">
            <CoverArt seed={courseCode || courseTitle} icon={GraduationCap} />
          </div>
          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-2">
              <Avatar size="sm">
                <AvatarFallback>{initials(learnerName)}</AvatarFallback>
              </Avatar>
              <span className="text-sm font-medium text-fg">{learnerName}</span>
            </div>
            {courseCode ? (
              <p className="font-tabular text-xs text-fg-subtle">{courseCode}</p>
            ) : null}
            {status ? (
              <Badge variant={statusVariant(status)} className="w-fit">
                {humanizeToken(status)}
              </Badge>
            ) : null}
          </div>
        </div>
        <div className="flex items-center gap-4">
          <ProgressRing
            value={percent}
            size={84}
            strokeWidth={8}
            label={`Overall progress: ${percent}%`}
          />
          <div className="text-sm">
            <p className="font-medium text-fg">Overall progress</p>
            <p className="text-fg-muted">
              {completedCount} of {totalCount}{" "}
              {totalCount === 1 ? "lesson" : "lessons"} complete
            </p>
          </div>
        </div>
      </Card>

      {courseModules.length === 0 ? (
        <EmptyState
          icon={<Inbox />}
          title="No lessons yet"
          description="This course doesn't have any modules or lessons to track yet."
        />
      ) : (
        courseModules.map((m) => {
          const moduleLessons = lessonsByModule.get(m.id) ?? [];
          const moduleTitle = stringField(asData(m).title) || "Untitled module";
          const moduleDone = moduleLessons.filter(
            (l) => lessonProgressOf(progressByLesson.get(l.id)).completed,
          ).length;
          return (
            <div key={m.id} className="flex flex-col gap-3">
              <div className="flex items-center justify-between gap-3">
                <h2 className="text-lg font-semibold text-fg">{moduleTitle}</h2>
                <span className="font-tabular text-sm text-fg-muted">
                  {moduleDone}/{moduleLessons.length}
                </span>
              </div>
              {moduleLessons.length === 0 ? (
                <p className="text-sm text-fg-muted">No lessons in this module yet.</p>
              ) : (
                <div className="overflow-hidden rounded-lg border border-border">
                  <Table>
                    <TableHeader className="sticky top-0 bg-bg-subtle">
                      <TableRow>
                        <TableHead>Lesson</TableHead>
                        <TableHead className="w-28">Duration</TableHead>
                        <TableHead className="w-40">Progress</TableHead>
                        <TableHead className="w-32">Status</TableHead>
                        <TableHead className="w-20 text-end">Score</TableHead>
                        <TableHead className="w-32">Completed</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {moduleLessons.map((l) => {
                        const ld = asData(l);
                        const title = stringField(ld.title) || "Untitled lesson";
                        const duration = numericField(ld.duration_minutes);
                        const lp = lessonProgressOf(progressByLesson.get(l.id));
                        const token = lessonStatusToken(lp);
                        return (
                          <TableRow key={l.id}>
                            <TableCell className="font-medium text-fg">
                              <span className="flex items-center gap-2">
                                <LessonStatusIcon token={token} />
                                {title}
                              </span>
                            </TableCell>
                            <TableCell className="text-fg-muted">
                              {duration != null ? (
                                <span className="inline-flex items-center gap-1">
                                  <Clock className="h-3.5 w-3.5" aria-hidden />
                                  {fmt.number(duration)} min
                                </span>
                              ) : (
                                "—"
                              )}
                            </TableCell>
                            <TableCell>
                              <div className="flex items-center gap-2">
                                <ProgressBar
                                  value={lp.percent}
                                  tone={lp.completed ? "success" : "accent"}
                                  className="w-24"
                                  label={`${title}: ${lp.percent}% complete`}
                                />
                                <span className="font-tabular text-xs text-fg-muted">
                                  {lp.percent}%
                                </span>
                              </div>
                            </TableCell>
                            <TableCell>
                              <Badge variant={statusVariant(token)} size="sm">
                                {humanizeToken(token)}
                              </Badge>
                            </TableCell>
                            <TableCell className="text-end font-tabular">
                              {lp.score != null ? fmt.number(lp.score) : "—"}
                            </TableCell>
                            <TableCell className="text-fg-muted">
                              {lp.completedAt
                                ? fmt.date(new Date(lp.completedAt))
                                : "—"}
                            </TableCell>
                          </TableRow>
                        );
                      })}
                    </TableBody>
                  </Table>
                </div>
              )}
            </div>
          );
        })
      )}
    </section>
  );
}

function LessonStatusIcon({ token }: { token: string }) {
  if (token === "completed") {
    return <CheckCircle2 className="h-4 w-4 shrink-0 text-success" aria-hidden />;
  }
  if (token === "in_progress") {
    return <PlayCircle className="h-4 w-4 shrink-0 text-info" aria-hidden />;
  }
  return <Circle className="h-4 w-4 shrink-0 text-fg-subtle" aria-hidden />;
}

export default LearnerProgressPage;
