import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { DiscussionThread } from "@kapp/client";
import {
  Badge,
  Button,
  Input,
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
 * DiscussionsPage (Session 17, Deliverable 12).
 *
 * Per-course discussion forum. The instructor/learner first selects a
 * course (the `/api/v1/lms/discussions` list endpoint is scoped by
 * course_id), then sees its threads and can start a new one. Replies
 * are handled on a thread detail view; this page focuses on the
 * thread list + creation, which is the common entry point.
 */
type ThreadDraft = { title: string; body: string };

export function DiscussionsPage() {
  const qc = useQueryClient();
  const [courseId, setCourseId] = useState("");
  const [draft, setDraft] = useState<ThreadDraft>({ title: "", body: "" });

  const coursesQ = useQuery({
    queryKey: ["records", "lms.course"],
    queryFn: () => api.listRecords("lms.course"),
  });

  const threadsQ = useQuery({
    queryKey: ["lms", "discussions", courseId],
    queryFn: () => api.listDiscussions(courseId),
    enabled: !!courseId,
  });

  const createThread = useMutation({
    mutationFn: () =>
      api.createDiscussion({
        course_id: courseId,
        title: draft.title.trim(),
        body: draft.body.trim(),
      }),
    onSuccess: () => {
      setDraft({ title: "", body: "" });
      qc.invalidateQueries({ queryKey: ["lms", "discussions", courseId] });
    },
  });

  const courseTitleById = useMemo(() => {
    const m = new Map<string, string>();
    (coursesQ.data ?? []).forEach((c) => {
      const d = c.data as Record<string, unknown>;
      m.set(c.id, typeof d.title === "string" ? d.title : c.id);
    });
    return m;
  }, [coursesQ.data]);

  const threads: DiscussionThread[] = threadsQ.data?.threads ?? [];

  return (
    <section>
      <h1>Discussions</h1>
      <p className="text-fg-muted">
        Per-course Q&amp;A threads. Select a course to view or start a
        discussion.
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

      {courseId && (
        <form
          className="mt-4 flex flex-wrap items-end gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (draft.title.trim() && draft.body.trim()) createThread.mutate();
          }}
        >
          <label className="flex flex-col text-[12px] text-fg-muted">
            Title
            <Input
              value={draft.title}
              onChange={(e) => setDraft({ ...draft, title: e.target.value })}
              required
            />
          </label>
          <label className="flex flex-1 flex-col text-[12px] text-fg-muted">
            Body
            <Input
              value={draft.body}
              onChange={(e) => setDraft({ ...draft, body: e.target.value })}
              required
            />
          </label>
          <Button
            type="submit"
            disabled={
              !draft.title.trim() || !draft.body.trim() || createThread.isPending
            }
          >
            {createThread.isPending ? "Posting…" : "+ New thread"}
          </Button>
        </form>
      )}
      {createThread.isError && (
        <p className="mt-2 text-danger">
          Failed to post: {(createThread.error as Error).message}
        </p>
      )}

      {!courseId && (
        <p className="mt-4 text-fg-muted">Choose a course to see threads.</p>
      )}
      {courseId && threadsQ.isLoading && <p className="mt-4">Loading…</p>}
      {courseId && threadsQ.isError && (
        <p className="mt-4 text-danger">
          Failed to load threads: {(threadsQ.error as Error).message}
        </p>
      )}
      {courseId && threadsQ.data && threads.length === 0 && (
        <p className="mt-4 text-fg-muted">No threads yet for this course.</p>
      )}
      {threads.length > 0 && (
        <Table className="mt-4 text-[13px]">
          <TableHeader>
            <TableRow className="text-left">
              <TableHead>Title</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">Replies</TableHead>
              <TableHead>Pinned</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {threads.map((t) => (
              <TableRow key={t.id}>
                <TableCell>{t.title}</TableCell>
                <TableCell>
                  <Badge>{t.status}</Badge>
                </TableCell>
                <TableCell className="text-right">{t.reply_count}</TableCell>
                <TableCell>{t.pinned ? "Yes" : ""}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

export default DiscussionsPage;
