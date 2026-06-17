import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  CompleteInterviewInput,
  CreateInterviewInput,
  Interview,
  InterviewStatus,
  JobApplication,
  KRecord,
} from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Modal,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  Select,
  Skeleton,
  Tabs,
  TabsList,
  TabsTrigger,
  Textarea,
  toast,
  type BadgeProps,
} from "@kapp/ui";
import {
  AlertTriangle,
  CalendarClock,
  ExternalLink,
  MapPin,
  Plus,
  RefreshCw,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import { humanizeToken } from "../lib/ktypeView";

const INTERVIEW_TYPES = [
  { value: "phone", label: "Phone" },
  { value: "video", label: "Video" },
  { value: "in_person", label: "In person" },
  { value: "panel", label: "Panel" },
  { value: "technical", label: "Technical" },
  { value: "cultural", label: "Cultural" },
];

const RECOMMENDATIONS = [
  { value: "strong_yes", label: "Strong yes" },
  { value: "yes", label: "Yes" },
  { value: "neutral", label: "Neutral" },
  { value: "no", label: "No" },
  { value: "strong_no", label: "Strong no" },
];

// Interview status → Badge variant. `no_show` isn't in the shared
// statusVariant map, so the domain maps the lifecycle here.
function interviewVariant(status: InterviewStatus): BadgeProps["variant"] {
  switch (status) {
    case "scheduled":
      return "info";
    case "completed":
      return "success";
    case "no_show":
      return "danger";
    case "cancelled":
    default:
      return "neutral";
  }
}

type Tab = "scheduled" | "completed";

interface EmployeeData {
  name?: string;
}

/**
 * InterviewSchedulePage shows interviews split into an upcoming
 * (grouped by day as a lightweight calendar) tab and a past tab.
 * Scheduling creates an interview against an application; completing
 * one captures feedback + a recommendation via the dedicated endpoint.
 */
export function InterviewSchedulePage() {
  const fmt = useFormatter();
  const qc = useQueryClient();
  const [tab, setTab] = useState<Tab>("scheduled");
  const [scheduleOpen, setScheduleOpen] = useState(false);

  const interviewsQ = useQuery<Interview[]>({
    queryKey: ["recruitment", "interviews"],
    queryFn: () => api.listInterviews(),
  });
  const appsQ = useQuery<JobApplication[]>({
    queryKey: ["recruitment", "applications", ""],
    queryFn: () => api.listApplications(),
  });
  const employeesQ = useQuery<KRecord[]>({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const applicantName = useMemo(() => {
    const m = new Map<string, string>();
    (appsQ.data ?? []).forEach((a) => m.set(a.id, a.applicant_name));
    return m;
  }, [appsQ.data]);
  const employeeName = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((r) => {
      const d = r.data as EmployeeData;
      if (d?.name) m.set(r.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const completeMut = useMutation({
    mutationFn: ({ id, input }: { id: string; input: CompleteInterviewInput }) =>
      api.completeInterview(id, input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["recruitment", "interviews"] });
      toast.success("Feedback saved");
    },
    onError: (e) =>
      toast.error("Couldn't save feedback", { description: String(e) }),
  });

  const interviews = useMemo(() => interviewsQ.data ?? [], [interviewsQ.data]);
  const scheduled = useMemo(
    () => interviews.filter((i) => i.status === "scheduled"),
    [interviews],
  );
  const completed = useMemo(
    () => interviews.filter((i) => i.status !== "scheduled"),
    [interviews],
  );

  // byDay buckets scheduled interviews into ISO-date groups, sorted
  // ascending so the soonest day leads — the lightweight "calendar".
  const byDay = useMemo(() => {
    const m = new Map<string, Interview[]>();
    scheduled
      .slice()
      .sort((a, b) => (a.scheduled_at ?? "").localeCompare(b.scheduled_at ?? ""))
      .forEach((i) => {
        const day = i.scheduled_at ? i.scheduled_at.slice(0, 10) : "unscheduled";
        const arr = m.get(day) ?? [];
        arr.push(i);
        m.set(day, arr);
      });
    return m;
  }, [scheduled]);

  const loading = interviewsQ.isLoading;
  const error = interviewsQ.isError;

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Interviews
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Schedule interviews with candidates and capture feedback once
            they're done.
          </p>
        </div>
        <Button
          size="sm"
          leadingIcon={<Plus className="h-4 w-4" />}
          onClick={() => setScheduleOpen(true)}
        >
          Schedule interview
        </Button>
      </header>

      <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)}>
        <TabsList>
          <TabsTrigger value="scheduled">
            Upcoming ({scheduled.length})
          </TabsTrigger>
          <TabsTrigger value="completed">Past ({completed.length})</TabsTrigger>
        </TabsList>
      </Tabs>

      {error && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
        >
          <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
          <span className="min-w-0 flex-1">We couldn't load interviews.</span>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<RefreshCw className="h-4 w-4" />}
            onClick={() => interviewsQ.refetch()}
          >
            Retry
          </Button>
        </div>
      )}

      {loading && <InterviewsSkeleton />}

      {!loading && !error && tab === "scheduled" && (
        scheduled.length === 0 ? (
          <EmptyState
            icon={<CalendarClock />}
            title="No interviews scheduled"
            description="Schedule an interview with a candidate who's reached the interview stage."
            action={
              <Button size="sm" onClick={() => setScheduleOpen(true)}>
                Schedule interview
              </Button>
            }
          />
        ) : (
          <div className="flex flex-col gap-5">
            {[...byDay.entries()].map(([day, items]) => (
              <div key={day} className="flex flex-col gap-2">
                <div className="flex items-center gap-2">
                  <h2 className="text-sm font-semibold text-fg">
                    {day === "unscheduled"
                      ? "Date to be confirmed"
                      : fmt.date(new Date(`${day}T00:00:00`), {
                          weekday: "long",
                          month: "long",
                          day: "numeric",
                        })}
                  </h2>
                  <Badge variant="neutral" size="xs">
                    {items.length}
                  </Badge>
                </div>
                <div className="flex flex-col gap-2">
                  {items.map((i) => (
                    <InterviewRow
                      key={i.id}
                      iv={i}
                      applicant={applicantName.get(i.application_id)}
                      interviewer={
                        i.interviewer_id
                          ? employeeName.get(i.interviewer_id)
                          : undefined
                      }
                      onComplete={(input) =>
                        completeMut.mutate({ id: i.id, input })
                      }
                      completing={completeMut.isPending}
                    />
                  ))}
                </div>
              </div>
            ))}
          </div>
        )
      )}

      {!loading && !error && tab === "completed" && (
        completed.length === 0 ? (
          <EmptyState
            icon={<CalendarClock />}
            title="No past interviews"
            description="Completed and cancelled interviews will appear here."
          />
        ) : (
          <div className="flex flex-col gap-2">
            {completed.map((i) => (
              <InterviewRow
                key={i.id}
                iv={i}
                applicant={applicantName.get(i.application_id)}
                interviewer={
                  i.interviewer_id
                    ? employeeName.get(i.interviewer_id)
                    : undefined
                }
                onComplete={(input) => completeMut.mutate({ id: i.id, input })}
                completing={completeMut.isPending}
              />
            ))}
          </div>
        )
      )}

      <ScheduleInterviewModal
        open={scheduleOpen}
        onOpenChange={setScheduleOpen}
        applications={appsQ.data ?? []}
        employees={employeesQ.data ?? []}
        onScheduled={() =>
          qc.invalidateQueries({ queryKey: ["recruitment", "interviews"] })
        }
      />
    </section>
  );
}

interface InterviewRowProps {
  iv: Interview;
  applicant?: string;
  interviewer?: string;
  onComplete: (input: CompleteInterviewInput) => void;
  completing: boolean;
}

function InterviewRow({
  iv,
  applicant,
  interviewer,
  onComplete,
  completing,
}: InterviewRowProps) {
  const fmt = useFormatter();
  const [open, setOpen] = useState(false);
  const [rating, setRating] = useState<number>(iv.rating ?? 3);
  const [recommendation, setRecommendation] = useState(
    iv.recommendation || "neutral",
  );
  const [feedback, setFeedback] = useState(iv.feedback ?? "");

  const recommendationLabel = iv.recommendation
    ? RECOMMENDATIONS.find((r) => r.value === iv.recommendation)?.label ??
      humanizeToken(iv.recommendation)
    : null;

  return (
    <article className="rounded-md border border-border bg-bg-elevated p-3 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium text-fg">
          {applicant ?? "Candidate"}
        </span>
        <Badge variant="outline" size="xs">
          {humanType(iv.interview_type)}
        </Badge>
        <Badge variant={interviewVariant(iv.status)} size="xs">
          {humanizeToken(iv.status)}
        </Badge>
        {iv.scheduled_at && (
          <span className="text-fg-muted">
            {fmt.time(new Date(iv.scheduled_at))}
          </span>
        )}
        {interviewer && <span className="text-fg-muted">· {interviewer}</span>}
        {iv.duration_minutes > 0 && (
          <span className="text-fg-muted">· {iv.duration_minutes} min</span>
        )}
        {iv.status === "scheduled" && (
          <Button
            size="sm"
            variant="outline"
            className="ms-auto"
            onClick={() => setOpen((v) => !v)}
          >
            {open ? "Close" : "Record feedback"}
          </Button>
        )}
      </div>
      {(iv.meeting_link || iv.location) && (
        <div className="mt-1 text-sm">
          {iv.meeting_link ? (
            <a
              href={iv.meeting_link}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-accent hover:underline"
            >
              <ExternalLink className="h-3.5 w-3.5" aria-hidden />
              Join link
            </a>
          ) : (
            <span className="inline-flex items-center gap-1 text-fg-muted">
              <MapPin className="h-3.5 w-3.5" aria-hidden />
              {iv.location}
            </span>
          )}
        </div>
      )}
      {iv.status !== "scheduled" && iv.feedback && (
        <div className="mt-2 rounded-md bg-bg-subtle p-2">
          {recommendationLabel && (
            <Badge variant="neutral" size="xs">
              {recommendationLabel}
            </Badge>
          )}
          <p className="mt-1 whitespace-pre-wrap text-fg-muted">
            {iv.feedback}
          </p>
        </div>
      )}
      {open && iv.status === "scheduled" && (
        <form
          className="mt-3 flex flex-col gap-3 rounded-md bg-bg-subtle p-3"
          onSubmit={(e) => {
            e.preventDefault();
            onComplete({ rating, recommendation, feedback: feedback.trim() });
            setOpen(false);
          }}
        >
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field label="Rating" help="How strong was this interview?">
              <Select
                value={String(rating)}
                onChange={(e) => setRating(Number(e.target.value))}
              >
                {[1, 2, 3, 4, 5].map((n) => (
                  <option key={n} value={n}>
                    {n} of 5
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Recommendation">
              <Select
                value={recommendation}
                onChange={(e) => setRecommendation(e.target.value)}
              >
                {RECOMMENDATIONS.map((r) => (
                  <option key={r.value} value={r.value}>
                    {r.label}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
          <Field label="Feedback">
            <Textarea
              rows={3}
              placeholder="What stood out? Strengths, concerns, next steps."
              value={feedback}
              onChange={(e) => setFeedback(e.target.value)}
            />
          </Field>
          <div className="flex justify-end gap-2">
            <Button
              size="sm"
              type="button"
              variant="ghost"
              onClick={() => setOpen(false)}
            >
              Cancel
            </Button>
            <Button size="sm" type="submit" disabled={completing}>
              {completing ? "Saving…" : "Complete interview"}
            </Button>
          </div>
        </form>
      )}
    </article>
  );
}

function ScheduleInterviewModal({
  open,
  onOpenChange,
  applications,
  employees,
  onScheduled,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  applications: JobApplication[];
  employees: KRecord[];
  onScheduled: () => void;
}) {
  const empty: CreateInterviewInput = {
    application_id: "",
    interview_type: "video",
    duration_minutes: 60,
  };
  const [form, setForm] = useState<CreateInterviewInput>(empty);
  const [submitted, setSubmitted] = useState(false);
  const [seedOpen, setSeedOpen] = useState(open);
  if (seedOpen !== open) {
    setSeedOpen(open);
    if (open) {
      setForm(empty);
      setSubmitted(false);
    }
  }

  // Only applications still in the pipeline can sensibly be interviewed;
  // terminal states are filtered out of the picker.
  const interviewable = applications.filter(
    (a) =>
      a.status !== "rejected" &&
      a.status !== "withdrawn" &&
      a.status !== "hired",
  );

  const createMut = useMutation({
    mutationFn: (input: CreateInterviewInput) => api.createInterview(input),
    onSuccess: () => {
      toast.success("Interview scheduled");
      onOpenChange(false);
      onScheduled();
    },
  });

  const appError = submitted && !form.application_id;
  const isInPerson = form.interview_type === "in_person";

  function patch(next: Partial<CreateInterviewInput>) {
    setForm((f) => ({ ...f, ...next }));
  }

  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalContent className="max-w-2xl">
        <ModalHeader>
          <ModalTitle>Schedule an interview</ModalTitle>
          <ModalDescription>
            Pick a candidate and the format. You can add timing and feedback
            details now or later.
          </ModalDescription>
        </ModalHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault();
            setSubmitted(true);
            if (!form.application_id) return;
            createMut.mutate({
              ...form,
              // datetime-local yields "YYYY-MM-DDTHH:mm"; widen to RFC3339
              // by converting to an ISO UTC instant the server accepts.
              scheduled_at: form.scheduled_at
                ? new Date(form.scheduled_at).toISOString()
                : undefined,
            });
          }}
        >
          <Field
            label="Candidate"
            required
            error={appError ? "Choose a candidate to interview." : undefined}
            help={
              interviewable.length === 0
                ? "No candidates are in the interview stage yet."
                : undefined
            }
          >
            <Select
              value={form.application_id}
              onChange={(e) => patch({ application_id: e.target.value })}
              invalid={appError || undefined}
            >
              <option value="">Select a candidate…</option>
              {interviewable.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.applicant_name} — {humanizeToken(a.status)}
                </option>
              ))}
            </Select>
          </Field>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="Interview type">
              <Select
                value={form.interview_type}
                onChange={(e) => {
                  const interview_type = e.target.value;
                  // In-person carries a physical location; remote carries a
                  // meeting link. Clear the field that no longer applies so
                  // we never persist both.
                  patch({
                    interview_type,
                    ...(interview_type === "in_person"
                      ? { meeting_link: undefined }
                      : { location: undefined }),
                  });
                }}
              >
                {INTERVIEW_TYPES.map((t) => (
                  <option key={t.value} value={t.value}>
                    {t.label}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Interviewer">
              <Select
                value={form.interviewer_id ?? ""}
                onChange={(e) =>
                  patch({ interviewer_id: e.target.value || undefined })
                }
              >
                <option value="">Unassigned</option>
                {employees.map((emp) => (
                  <option key={emp.id} value={emp.id}>
                    {(emp.data as EmployeeData)?.name ?? "Unnamed"}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Date & time" help="Leave blank to confirm later.">
              <Input
                type="datetime-local"
                value={form.scheduled_at ?? ""}
                onChange={(e) => patch({ scheduled_at: e.target.value })}
              />
            </Field>
            <Field label="Duration" help="In minutes.">
              <Input
                type="number"
                min={15}
                step={15}
                value={form.duration_minutes ?? 60}
                onChange={(e) =>
                  patch({ duration_minutes: Number(e.target.value) || 60 })
                }
              />
            </Field>
          </div>

          {isInPerson ? (
            <Field label="Location">
              <Input
                value={form.location ?? ""}
                onChange={(e) => patch({ location: e.target.value })}
                placeholder="e.g. Room 4B, 2nd floor"
              />
            </Field>
          ) : (
            <Field label="Meeting link">
              <Input
                type="url"
                value={form.meeting_link ?? ""}
                onChange={(e) => patch({ meeting_link: e.target.value })}
                placeholder="https://…"
              />
            </Field>
          )}

          {createMut.isError && (
            <p className="text-sm text-danger">
              Couldn't schedule the interview: {String(createMut.error)}
            </p>
          )}

          <ModalFooter>
            <Button
              type="button"
              variant="ghost"
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={createMut.isPending}>
              {createMut.isPending ? "Scheduling…" : "Schedule interview"}
            </Button>
          </ModalFooter>
        </form>
      </ModalContent>
    </Modal>
  );
}

function InterviewsSkeleton() {
  return (
    <div className="flex flex-col gap-3">
      {Array.from({ length: 4 }).map((_, i) => (
        <Skeleton key={i} className="h-16 w-full rounded-md" />
      ))}
    </div>
  );
}

function humanType(t: string): string {
  return INTERVIEW_TYPES.find((x) => x.value === t)?.label ?? humanizeToken(t);
}
