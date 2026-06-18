import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { DiscussionThread, KRecord } from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Button,
  EmptyState,
  Field,
  Input,
  Modal,
  ModalClose,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  Select,
  Skeleton,
  Textarea,
  initials,
  toast,
} from "@kapp/ui";
import {
  AlertTriangle,
  MessageSquare,
  MessagesSquare,
  Pin,
  Plus,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken, statusVariant } from "../lib/ktypeView";
import { LmsPageHeader } from "../components/lms/primitives";

/**
 * DiscussionsPage is a per-course discussion forum. The learner or
 * instructor first selects a course (the discussions list endpoint is
 * scoped by course_id), then reads its threads and can start a new one
 * via a modal composer. Replies live on a thread detail view; this page
 * is the thread list + creation entry point.
 */
type ThreadDraft = { title: string; body: string };

/**
 * Format an ISO timestamp as a localized relative time ("3 hours ago").
 *
 * Each tier (minutes, hours, days, …) is derived directly from `diffSec`
 * rather than from the previously rounded tier. This is intentional: chaining
 * (e.g. deriving hours from already-rounded minutes) would compound rounding
 * error, whereas re-deriving from seconds keeps every boundary correct.
 */
function relativeFromNow(iso: string, fmt: ReturnType<typeof useFormatter>): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const diffSec = Math.round((then - Date.now()) / 1000);
  const abs = Math.abs(diffSec);
  if (abs < 60) return fmt.relativeTime(diffSec, "second");
  const diffMin = Math.round(diffSec / 60);
  if (Math.abs(diffMin) < 60) return fmt.relativeTime(diffMin, "minute");
  const diffHr = Math.round(diffSec / 3600);
  if (Math.abs(diffHr) < 24) return fmt.relativeTime(diffHr, "hour");
  const diffDay = Math.round(diffSec / 86400);
  if (Math.abs(diffDay) < 30) return fmt.relativeTime(diffDay, "day");
  const diffMon = Math.round(diffDay / 30);
  if (Math.abs(diffMon) < 12) return fmt.relativeTime(diffMon, "month");
  return fmt.relativeTime(Math.round(diffDay / 365), "year");
}

export function DiscussionsPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
  const [courseId, setCourseId] = useState("");
  const [draft, setDraft] = useState<ThreadDraft>({ title: "", body: "" });
  const [createOpen, setCreateOpen] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  const coursesQ = useQuery({
    queryKey: ["records", "lms.course"],
    queryFn: () => api.listRecords("lms.course"),
  });
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
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
      setSubmitted(false);
      setCreateOpen(false);
      qc.invalidateQueries({ queryKey: ["lms", "discussions", courseId] });
      toast.success("Discussion posted");
    },
    onError: (err) => {
      toast.error("Couldn't post discussion", {
        description: (err as Error).message,
      });
    },
  });

  const courseTitleById = useMemo(() => {
    const m = new Map<string, string>();
    (coursesQ.data ?? []).forEach((c) => {
      const d = c.data as Record<string, unknown>;
      m.set(c.id, typeof d.title === "string" ? d.title : "Untitled course");
    });
    return m;
  }, [coursesQ.data]);

  const authorNameById = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((e: KRecord) => {
      const d = e.data as Record<string, unknown>;
      if (typeof d.name === "string") m.set(e.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const threads: DiscussionThread[] = useMemo(() => {
    const list = [...(threadsQ.data?.threads ?? [])];
    return list.sort((x, y) => {
      if (x.pinned !== y.pinned) return x.pinned ? -1 : 1;
      return (
        new Date(y.updated_at).getTime() - new Date(x.updated_at).getTime()
      );
    });
  }, [threadsQ.data]);

  const titleError =
    submitted && !draft.title.trim() ? "Please add a title." : undefined;
  const bodyError =
    submitted && !draft.body.trim() ? "Please write your question." : undefined;

  function submitCreate() {
    setSubmitted(true);
    if (draft.title.trim() && draft.body.trim()) createThread.mutate();
  }

  const newThreadButton = (
    <Button
      leadingIcon={<Plus className="h-4 w-4" />}
      onClick={() => setCreateOpen(true)}
    >
      New thread
    </Button>
  );

  return (
    <section className="flex flex-col gap-6">
      <LmsPageHeader
        area="Community"
        title="Discussions"
        description="Per-course Q&A threads. Select a course to read or start a discussion."
        actions={courseId ? newThreadButton : undefined}
      />

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

      {!courseId ? (
        <EmptyState
          icon={<MessagesSquare />}
          title="Select a course"
          description="Choose a course above to read its discussions or start a new thread."
        />
      ) : threadsQ.isLoading ? (
        <ul className="flex flex-col gap-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <li
              key={i}
              className="flex items-start gap-3 rounded-lg border border-border bg-bg-elevated p-4"
            >
              <Skeleton variant="circle" className="h-7 w-7" />
              <div className="flex-1">
                <Skeleton variant="text" className="h-4 w-1/2" />
                <Skeleton variant="text" className="mt-2 h-4 w-full" />
              </div>
            </li>
          ))}
        </ul>
      ) : threadsQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load discussions"
          description={(threadsQ.error as Error).message}
          action={
            <Button variant="secondary" onClick={() => threadsQ.refetch()}>
              Try again
            </Button>
          }
        />
      ) : threads.length === 0 ? (
        <EmptyState
          icon={<MessagesSquare />}
          title="No discussions yet"
          description="Be the first to ask a question or share something with this cohort."
          action={newThreadButton}
        />
      ) : (
        <ul className="flex flex-col gap-3">
          {threads.map((t) => {
            const author = authorNameById.get(t.author_id) || "Member";
            const replies = t.reply_count;
            return (
              <li key={t.id}>
                <article className="flex items-start gap-3 rounded-lg border border-border bg-bg-elevated p-4 transition-colors hover:border-border-strong hover:bg-bg-subtle">
                  <Avatar size="sm">
                    <AvatarFallback>{initials(author)}</AvatarFallback>
                  </Avatar>
                  <div className="min-w-0 flex-1">
                    <div className="flex items-start justify-between gap-3">
                      <div className="flex min-w-0 items-center gap-1.5">
                        {t.pinned ? (
                          <Pin
                            className="h-3.5 w-3.5 shrink-0 text-accent"
                            aria-label="Pinned"
                          />
                        ) : null}
                        <h3 className="truncate text-sm font-semibold text-fg">
                          {t.title}
                        </h3>
                      </div>
                      <Badge variant={statusVariant(t.status)} size="sm">
                        {humanizeToken(t.status)}
                      </Badge>
                    </div>
                    {t.body ? (
                      <p className="mt-1 line-clamp-2 text-sm text-fg-muted">
                        {t.body}
                      </p>
                    ) : null}
                    <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-fg-subtle">
                      <span className="font-medium text-fg-muted">{author}</span>
                      <span aria-hidden>·</span>
                      <span>{relativeFromNow(t.created_at, fmt)}</span>
                      <span aria-hidden>·</span>
                      <span className="inline-flex items-center gap-1">
                        <MessageSquare className="h-3.5 w-3.5" aria-hidden />
                        {replies} {replies === 1 ? "reply" : "replies"}
                      </span>
                    </div>
                  </div>
                </article>
              </li>
            );
          })}
        </ul>
      )}

      <Modal
        open={createOpen}
        onOpenChange={(next) => {
          setCreateOpen(next);
          if (!next) setSubmitted(false);
        }}
      >
        <ModalContent>
          <ModalHeader>
            <ModalTitle>Start a discussion</ModalTitle>
            <ModalDescription>
              Ask a question or share something with{" "}
              {courseTitleById.get(courseId) ?? "this course"}.
            </ModalDescription>
          </ModalHeader>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              submitCreate();
            }}
          >
            <Field label="Title" required error={titleError}>
              <Input
                value={draft.title}
                onChange={(e) => setDraft({ ...draft, title: e.target.value })}
                placeholder="What's your question?"
                autoFocus
              />
            </Field>
            <Field label="Message" required error={bodyError}>
              <Textarea
                value={draft.body}
                onChange={(e) => setDraft({ ...draft, body: e.target.value })}
                placeholder="Add the details that will help others answer."
                rows={5}
              />
            </Field>
            <ModalFooter>
              <ModalClose asChild>
                <Button type="button" variant="ghost">
                  Cancel
                </Button>
              </ModalClose>
              <Button type="submit" disabled={createThread.isPending}>
                {createThread.isPending ? "Posting…" : "Post discussion"}
              </Button>
            </ModalFooter>
          </form>
        </ModalContent>
      </Modal>
    </section>
  );
}

export default DiscussionsPage;
