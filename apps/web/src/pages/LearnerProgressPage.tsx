import { useMemo } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * LearnerProgressPage shows per-lesson completion, scores, and overall
 * course progress for a single enrollment.
 *
 * Route shape:
 *   /lms/progress                 — index: list every enrollment with
 *                                   a quick completion percentage.
 *   /lms/progress/:enrollmentId   — detail for one enrollment.
 *
 * The page reads enrollment / course / module / lesson / progress as
 * KRecords via the generic `/records/{ktype}` surface. The dedicated
 * lesson_progress / enrollment_progress tables that back the HR+LMS
 * store are not exposed over HTTP yet, so the MVP hydrates from the
 * KRecord side and gracefully reports "no progress yet" when the
 * lms.progress rows are missing.
 */
export function LearnerProgressPage() {
  const { enrollmentId } = useParams<{ enrollmentId?: string }>();
  if (enrollmentId) {
    return <LearnerProgressDetail enrollmentId={enrollmentId} />;
  }
  return <LearnerProgressIndex />;
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
  const progressQ = useQuery({
    queryKey: ["records", "lms.progress"],
    queryFn: () => api.listRecords("lms.progress"),
  });

  const courseTitleById = useMemo(() => {
    const m = new Map<string, string>();
    (coursesQ.data ?? []).forEach((c) => {
      const d = c.data as Record<string, unknown>;
      const title = typeof d.title === "string" ? d.title : "(untitled)";
      m.set(c.id, title);
    });
    return m;
  }, [coursesQ.data]);

  return (
    <section>
      <h1>Learner Progress</h1>
      <p className="text-fg-muted">
        One row per enrollment. Click through for per-lesson completion
        and scores.
      </p>
      {enrollmentsQ.isLoading && <p>Loading…</p>}
      {enrollmentsQ.isError && (
        <p className="text-danger">
          Failed to load enrollments: {(enrollmentsQ.error as Error).message}
        </p>
      )}
      {enrollmentsQ.data && enrollmentsQ.data.length === 0 && (
        <p className="text-fg-muted">No enrollments yet.</p>
      )}
      {enrollmentsQ.data && enrollmentsQ.data.length > 0 && (
        <Table className="mt-3 text-[13px]">
          <TableHeader>
            <TableRow className="text-left">
              <TableHead>Enrollment</TableHead>
              <TableHead>Course</TableHead>
              <TableHead>Learner</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">Completed</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {enrollmentsQ.data.map((e) => {
              const d = e.data as Record<string, unknown>;
              const courseId =
                typeof d.course_id === "string" ? d.course_id : "";
              const userId = typeof d.user_id === "string" ? d.user_id : "";
              const status = typeof d.status === "string" ? d.status : "";
              const prog = summarizeProgress(
                e.id,
                progressQ.data ?? [],
              );
              return (
                <TableRow key={e.id}>
                  <TableCell>
                    <Link to={`/lms/progress/${e.id}`}>{e.id.slice(0, 8)}…</Link>
                  </TableCell>
                  <TableCell>
                    {courseTitleById.get(courseId) ?? courseId}
                  </TableCell>
                  <TableCell>{userId}</TableCell>
                  <TableCell>{status}</TableCell>
                  <TableCell className="text-right">{prog.completed}</TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

function LearnerProgressDetail({ enrollmentId }: { enrollmentId: string }) {
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

  const enrollment = (enrollmentsQ.data ?? []).find((e) => e.id === enrollmentId);
  const enrollmentData =
    (enrollment?.data as Record<string, unknown> | undefined) ?? {};
  const courseId =
    typeof enrollmentData.course_id === "string" ? enrollmentData.course_id : "";
  const course = (coursesQ.data ?? []).find((c) => c.id === courseId);
  const courseTitle =
    course && typeof (course.data as Record<string, unknown>).title === "string"
      ? ((course.data as Record<string, unknown>).title as string)
      : "(unknown course)";

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

  const allLessons = courseModules.flatMap(
    (m) => lessonsByModule.get(m.id) ?? [],
  );
  const completedCount = allLessons.filter((l) => {
    const prog = progressByLesson.get(l.id);
    const progData = (prog?.data as Record<string, unknown> | undefined) ?? {};
    return progData.status === "completed";
  }).length;
  const totalCount = allLessons.length;
  const percent =
    totalCount > 0 ? Math.round((completedCount / totalCount) * 100) : 0;

  const loading =
    enrollmentsQ.isLoading ||
    coursesQ.isLoading ||
    modulesQ.isLoading ||
    lessonsQ.isLoading ||
    progressQ.isLoading;

  return (
    <section>
      <div className="mb-2">
        <Link to="/lms/progress">← All enrollments</Link>
      </div>
      <h1>Learner Progress</h1>
      {loading && <p>Loading…</p>}
      {!loading && !enrollment && (
        <p className="text-danger">
          Enrollment {enrollmentId} not found.
        </p>
      )}
      {!loading && enrollment && (
        <>
          <dl className="mb-4 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-[13px]">
            <dt className="text-fg-muted">Enrollment</dt>
            <dd className="m-0">{enrollment.id}</dd>
            <dt className="text-fg-muted">Course</dt>
            <dd className="m-0">{courseTitle}</dd>
            <dt className="text-fg-muted">Learner</dt>
            <dd className="m-0">
              {stringOr(enrollmentData.user_id, "(unknown)")}
            </dd>
            <dt className="text-fg-muted">Status</dt>
            <dd className="m-0">
              {stringOr(enrollmentData.status, "")}
            </dd>
          </dl>

          <div className="mb-4">
            <div className="flex justify-between">
              <strong>Overall</strong>
              <span>
                {completedCount} / {totalCount} lessons ({percent}%)
              </span>
            </div>
            <div className="mt-1 h-2 overflow-hidden rounded bg-bg-muted">
              <div
                className="h-full bg-accent"
                style={{ width: `${percent}%` }}
              />
            </div>
          </div>

          {courseModules.length === 0 && (
            <p className="text-fg-muted">
              This course has no modules yet.
            </p>
          )}

          {courseModules.map((m) => {
            const moduleLessons = lessonsByModule.get(m.id) ?? [];
            const moduleTitle =
              typeof (m.data as Record<string, unknown>).title === "string"
                ? ((m.data as Record<string, unknown>).title as string)
                : "(untitled module)";
            return (
              <div key={m.id} className="mb-4">
                <h3 className="my-2">{moduleTitle}</h3>
                {moduleLessons.length === 0 ? (
                  <p className="text-[13px] text-fg-muted">
                    No lessons in this module.
                  </p>
                ) : (
                  <Table className="text-[13px]">
                    <TableHeader>
                      <TableRow className="text-left">
                        <TableHead>Lesson</TableHead>
                        <TableHead>Type</TableHead>
                        <TableHead>Status</TableHead>
                        <TableHead className="text-right">Score</TableHead>
                        <TableHead>Completed</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {moduleLessons.map((l) => {
                        const ld = l.data as Record<string, unknown>;
                        const title = stringOr(ld.title, "(untitled)");
                        const type = stringOr(ld.content_type, "");
                        const prog = progressByLesson.get(l.id);
                        const progData =
                          (prog?.data as Record<string, unknown> | undefined) ??
                          {};
                        return (
                          <TableRow key={l.id}>
                            <TableCell>{title}</TableCell>
                            <TableCell>{type}</TableCell>
                            <TableCell>
                              {stringOr(progData.status, "not_started")}
                            </TableCell>
                            <TableCell className="text-right">
                              {numberOr(progData.score, "—")}
                            </TableCell>
                            <TableCell>
                              {stringOr(progData.completed_at, "")}
                            </TableCell>
                          </TableRow>
                        );
                      })}
                    </TableBody>
                  </Table>
                )}
              </div>
            );
          })}
        </>
      )}
    </section>
  );
}

function orderedModulesForCourse(
  modules: KRecord[],
  courseId: string,
): KRecord[] {
  return modules
    .filter((m) => {
      const d = m.data as Record<string, unknown>;
      return d.course_id === courseId;
    })
    .sort((a, b) => {
      const ao = numericField((a.data as Record<string, unknown>).order);
      const bo = numericField((b.data as Record<string, unknown>).order);
      return ao - bo;
    });
}

function groupLessonsByModule(lessons: KRecord[]): Map<string, KRecord[]> {
  const m = new Map<string, KRecord[]>();
  lessons.forEach((l) => {
    const d = l.data as Record<string, unknown>;
    const moduleId = typeof d.module_id === "string" ? d.module_id : "";
    if (!moduleId) return;
    const bucket = m.get(moduleId) ?? [];
    bucket.push(l);
    m.set(moduleId, bucket);
  });
  m.forEach((list) =>
    list.sort((a, b) => {
      const ao = numericField((a.data as Record<string, unknown>).order);
      const bo = numericField((b.data as Record<string, unknown>).order);
      return ao - bo;
    }),
  );
  return m;
}

function indexProgress(
  progress: KRecord[],
  enrollmentId: string,
): Map<string, KRecord> {
  const m = new Map<string, KRecord>();
  progress.forEach((p) => {
    const d = p.data as Record<string, unknown>;
    if (d.enrollment_id !== enrollmentId) return;
    const lessonId = typeof d.lesson_id === "string" ? d.lesson_id : "";
    if (!lessonId) return;
    m.set(lessonId, p);
  });
  return m;
}

function summarizeProgress(
  enrollmentId: string,
  progress: KRecord[],
): { completed: number } {
  let completed = 0;
  progress.forEach((p) => {
    const d = p.data as Record<string, unknown>;
    if (d.enrollment_id === enrollmentId && d.status === "completed") {
      completed++;
    }
  });
  return { completed };
}

function stringOr(v: unknown, fallback: string): string {
  return typeof v === "string" && v ? v : fallback;
}

function numberOr(v: unknown, fallback: string): string {
  if (typeof v === "number") return String(v);
  if (typeof v === "string" && v !== "") return v;
  return fallback;
}

function numericField(v: unknown): number {
  if (typeof v === "number") return v;
  if (typeof v === "string") {
    const n = Number(v);
    if (!Number.isNaN(n)) return n;
  }
  return Number.MAX_SAFE_INTEGER;
}
