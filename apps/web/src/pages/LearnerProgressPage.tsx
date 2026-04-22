import { useMemo } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
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
      <p style={{ color: "#6b7280" }}>
        One row per enrollment. Click through for per-lesson completion
        and scores.
      </p>
      {enrollmentsQ.isLoading && <p>Loading…</p>}
      {enrollmentsQ.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load enrollments: {(enrollmentsQ.error as Error).message}
        </p>
      )}
      {enrollmentsQ.data && enrollmentsQ.data.length === 0 && (
        <p style={{ color: "#6b7280" }}>No enrollments yet.</p>
      )}
      {enrollmentsQ.data && enrollmentsQ.data.length > 0 && (
        <table
          style={{
            width: "100%",
            borderCollapse: "collapse",
            fontSize: 13,
            marginTop: 12,
          }}
        >
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th style={{ padding: "6px 8px" }}>Enrollment</th>
              <th style={{ padding: "6px 8px" }}>Course</th>
              <th style={{ padding: "6px 8px" }}>Learner</th>
              <th style={{ padding: "6px 8px" }}>Status</th>
              <th style={{ padding: "6px 8px", textAlign: "right" }}>
                Completed
              </th>
            </tr>
          </thead>
          <tbody>
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
                <tr
                  key={e.id}
                  style={{ borderBottom: "1px solid #f3f4f6" }}
                >
                  <td style={{ padding: "6px 8px" }}>
                    <Link to={`/lms/progress/${e.id}`}>{e.id.slice(0, 8)}…</Link>
                  </td>
                  <td style={{ padding: "6px 8px" }}>
                    {courseTitleById.get(courseId) ?? courseId}
                  </td>
                  <td style={{ padding: "6px 8px" }}>{userId}</td>
                  <td style={{ padding: "6px 8px" }}>{status}</td>
                  <td style={{ padding: "6px 8px", textAlign: "right" }}>
                    {prog.completed}
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
      <div style={{ marginBottom: 8 }}>
        <Link to="/lms/progress">← All enrollments</Link>
      </div>
      <h1>Learner Progress</h1>
      {loading && <p>Loading…</p>}
      {!loading && !enrollment && (
        <p style={{ color: "#b91c1c" }}>
          Enrollment {enrollmentId} not found.
        </p>
      )}
      {!loading && enrollment && (
        <>
          <dl
            style={{
              display: "grid",
              gridTemplateColumns: "max-content 1fr",
              gap: "4px 16px",
              fontSize: 13,
              marginBottom: 16,
            }}
          >
            <dt style={{ color: "#6b7280" }}>Enrollment</dt>
            <dd style={{ margin: 0 }}>{enrollment.id}</dd>
            <dt style={{ color: "#6b7280" }}>Course</dt>
            <dd style={{ margin: 0 }}>{courseTitle}</dd>
            <dt style={{ color: "#6b7280" }}>Learner</dt>
            <dd style={{ margin: 0 }}>
              {stringOr(enrollmentData.user_id, "(unknown)")}
            </dd>
            <dt style={{ color: "#6b7280" }}>Status</dt>
            <dd style={{ margin: 0 }}>
              {stringOr(enrollmentData.status, "")}
            </dd>
          </dl>

          <div style={{ marginBottom: 16 }}>
            <div style={{ display: "flex", justifyContent: "space-between" }}>
              <strong>Overall</strong>
              <span>
                {completedCount} / {totalCount} lessons ({percent}%)
              </span>
            </div>
            <div
              style={{
                height: 8,
                background: "#e5e7eb",
                borderRadius: 4,
                marginTop: 4,
                overflow: "hidden",
              }}
            >
              <div
                style={{
                  width: `${percent}%`,
                  height: "100%",
                  background: "#2563eb",
                }}
              />
            </div>
          </div>

          {courseModules.length === 0 && (
            <p style={{ color: "#6b7280" }}>
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
              <div key={m.id} style={{ marginBottom: 16 }}>
                <h3 style={{ margin: "8px 0" }}>{moduleTitle}</h3>
                {moduleLessons.length === 0 ? (
                  <p style={{ color: "#6b7280", fontSize: 13 }}>
                    No lessons in this module.
                  </p>
                ) : (
                  <table
                    style={{
                      width: "100%",
                      borderCollapse: "collapse",
                      fontSize: 13,
                    }}
                  >
                    <thead>
                      <tr
                        style={{
                          textAlign: "left",
                          borderBottom: "1px solid #e5e7eb",
                        }}
                      >
                        <th style={{ padding: "6px 8px" }}>Lesson</th>
                        <th style={{ padding: "6px 8px" }}>Type</th>
                        <th style={{ padding: "6px 8px" }}>Status</th>
                        <th style={{ padding: "6px 8px", textAlign: "right" }}>
                          Score
                        </th>
                        <th style={{ padding: "6px 8px" }}>Completed</th>
                      </tr>
                    </thead>
                    <tbody>
                      {moduleLessons.map((l) => {
                        const ld = l.data as Record<string, unknown>;
                        const title = stringOr(ld.title, "(untitled)");
                        const type = stringOr(ld.content_type, "");
                        const prog = progressByLesson.get(l.id);
                        const progData =
                          (prog?.data as Record<string, unknown> | undefined) ??
                          {};
                        return (
                          <tr
                            key={l.id}
                            style={{ borderBottom: "1px solid #f3f4f6" }}
                          >
                            <td style={{ padding: "6px 8px" }}>{title}</td>
                            <td style={{ padding: "6px 8px" }}>{type}</td>
                            <td style={{ padding: "6px 8px" }}>
                              {stringOr(progData.status, "not_started")}
                            </td>
                            <td
                              style={{ padding: "6px 8px", textAlign: "right" }}
                            >
                              {numberOr(progData.score, "—")}
                            </td>
                            <td style={{ padding: "6px 8px" }}>
                              {stringOr(progData.completed_at, "")}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
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
