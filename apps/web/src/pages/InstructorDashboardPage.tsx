import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { CourseAnalytics } from "@kapp/client";
import {
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * InstructorDashboardPage (Session 17, Deliverable 12).
 *
 * Pick a course, then render its aggregate analytics from
 * `/api/v1/lms/courses/{id}/analytics`: enrollment + completion KPIs,
 * per-lesson drop-off, and a per-learner progress table. The course
 * list itself comes from the generic KRecord surface so the page works
 * even before any analytics rows exist.
 */
export function InstructorDashboardPage() {
  const [courseId, setCourseId] = useState<string>("");

  const coursesQ = useQuery({
    queryKey: ["records", "lms.course"],
    queryFn: () => api.listRecords("lms.course"),
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
      m.set(c.id, typeof d.title === "string" ? d.title : c.id);
    });
    return m;
  }, [coursesQ.data]);

  const a: CourseAnalytics | undefined = analyticsQ.data;

  return (
    <section>
      <h1>Instructor Dashboard</h1>
      <p className="text-fg-muted">
        Course-level analytics: enrollment, completion, drop-off, and
        per-learner progress.
      </p>

      <label className="mt-4 flex max-w-md flex-col text-[12px] text-fg-muted">
        Course
        <Select value={courseId} onChange={(e) => setCourseId(e.target.value)}>
          <option value="">Select a course…</option>
          {(coursesQ.data ?? []).map((c) => (
            <option key={c.id} value={c.id}>
              {courseTitleById.get(c.id)}
            </option>
          ))}
        </Select>
      </label>

      {!courseId && (
        <p className="mt-4 text-fg-muted">Choose a course to see analytics.</p>
      )}
      {courseId && analyticsQ.isLoading && <p className="mt-4">Loading…</p>}
      {courseId && analyticsQ.isError && (
        <p className="mt-4 text-danger">
          Failed to load analytics: {(analyticsQ.error as Error).message}
        </p>
      )}

      {a && (
        <>
          <div className="mt-4 flex flex-wrap gap-4">
            <Kpi label="Enrollments" value={String(a.enrollment_count)} />
            <Kpi label="Completed" value={String(a.completed_count)} />
            <Kpi
              label="Completion rate"
              value={`${(a.completion_rate * 100).toFixed(1)}%`}
            />
            <Kpi
              label="Average score"
              value={a.average_score ?? "—"}
            />
          </div>

          <h2 className="mt-6">Lesson drop-off</h2>
          {a.lesson_drop_off.length === 0 ? (
            <p className="text-fg-muted">No lesson progress yet.</p>
          ) : (
            <Table className="mt-2 text-[13px]">
              <TableHeader>
                <TableRow className="text-left">
                  <TableHead>Lesson</TableHead>
                  <TableHead className="text-right">Reached</TableHead>
                  <TableHead className="text-right">Completed</TableHead>
                  <TableHead className="text-right">Drop-off</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {a.lesson_drop_off.map((l) => (
                  <TableRow key={l.lesson_id}>
                    <TableCell>{l.lesson_id.slice(0, 8)}…</TableCell>
                    <TableCell className="text-right">{l.reached}</TableCell>
                    <TableCell className="text-right">{l.completed}</TableCell>
                    <TableCell className="text-right">
                      {(l.drop_off_rate * 100).toFixed(1)}%
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}

          <h2 className="mt-6">Per-learner progress</h2>
          {a.per_learner.length === 0 ? (
            <p className="text-fg-muted">No learners enrolled yet.</p>
          ) : (
            <Table className="mt-2 text-[13px]">
              <TableHeader>
                <TableRow className="text-left">
                  <TableHead>Learner</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Lessons</TableHead>
                  <TableHead className="text-right">Avg score</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {a.per_learner.map((l) => (
                  <TableRow key={l.enrollment_id}>
                    <TableCell>{l.user_id.slice(0, 8)}…</TableCell>
                    <TableCell>{l.status}</TableCell>
                    <TableCell className="text-right">
                      {l.lessons_completed}/{l.lessons_total}
                    </TableCell>
                    <TableCell className="text-right">
                      {l.average_score ?? "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}
    </section>
  );
}

function Kpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border bg-bg-elevated p-4">
      <div className="text-[12px] text-fg-muted">{label}</div>
      <div className="text-2xl font-semibold">{value}</div>
    </div>
  );
}

export default InstructorDashboardPage;
