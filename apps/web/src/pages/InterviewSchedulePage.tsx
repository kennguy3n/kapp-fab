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
import { Badge, Button, Input, Select } from "@kapp/ui";
import { api } from "../lib/api";

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

const STATUS_BADGE: Record<
  InterviewStatus,
  "default" | "success" | "warning" | "danger"
> = {
  scheduled: "warning",
  completed: "success",
  cancelled: "default",
  no_show: "danger",
};

type Tab = "scheduled" | "completed";

interface EmployeeData {
  name?: string;
}

/**
 * InterviewSchedulePage shows interviews split into a Scheduled
 * (upcoming, grouped by day as a lightweight calendar) tab and a
 * Completed tab. Scheduling creates an interview against an
 * application; completing one captures the feedback + recommendation
 * via the dedicated complete endpoint.
 */
export function InterviewSchedulePage() {
  const qc = useQueryClient();
  const [tab, setTab] = useState<Tab>("scheduled");

  const interviewsQ = useQuery({
    queryKey: ["recruitment", "interviews"],
    queryFn: () => api.listInterviews(),
  });
  const appsQ = useQuery({
    queryKey: ["recruitment", "applications", ""],
    queryFn: () => api.listApplications(),
  });
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const applicantName = useMemo(() => {
    const m = new Map<string, string>();
    (appsQ.data ?? []).forEach((a: JobApplication) =>
      m.set(a.id, a.applicant_name),
    );
    return m;
  }, [appsQ.data]);
  const employeeName = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((r: KRecord) => {
      const d = r.data as EmployeeData;
      if (d?.name) m.set(r.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const completeMut = useMutation({
    mutationFn: ({ id, input }: { id: string; input: CompleteInterviewInput }) =>
      api.completeInterview(id, input),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["recruitment", "interviews"] }),
  });

  // Memoize so scheduled/completed (and the byDay calendar below) keep a
  // stable reference across unrelated re-renders — toggling the feedback
  // form or switching tabs no longer recomputes the sort + group-by-day.
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
        const day = i.scheduled_at ? i.scheduled_at.slice(0, 10) : "Unscheduled";
        const arr = m.get(day) ?? [];
        arr.push(i);
        m.set(day, arr);
      });
    return m;
  }, [scheduled]);

  return (
    <section className="flex flex-col gap-3">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Interviews
      </h1>
      <p className="text-sm text-fg-muted">
        Schedule interviews against applications and capture feedback. Shortlist
        a candidate first to move them into the interview stage.
      </p>

      <ScheduleInterviewForm
        applications={appsQ.data ?? []}
        employees={employeesQ.data ?? []}
        onScheduled={() =>
          qc.invalidateQueries({ queryKey: ["recruitment", "interviews"] })
        }
      />

      <div className="flex items-center gap-2">
        <Button
          size="sm"
          variant={tab === "scheduled" ? "primary" : "outline"}
          aria-pressed={tab === "scheduled"}
          onClick={() => setTab("scheduled")}
        >
          Scheduled ({scheduled.length})
        </Button>
        <Button
          size="sm"
          variant={tab === "completed" ? "primary" : "outline"}
          aria-pressed={tab === "completed"}
          onClick={() => setTab("completed")}
        >
          Completed ({completed.length})
        </Button>
      </div>

      {interviewsQ.isLoading && (
        <p className="text-sm text-fg-muted">Loading…</p>
      )}
      {interviewsQ.isError && (
        <p className="text-sm text-danger">{String(interviewsQ.error)}</p>
      )}

      {tab === "scheduled" ? (
        scheduled.length === 0 ? (
          <p className="text-sm text-fg-muted">No scheduled interviews.</p>
        ) : (
          <div className="flex flex-col gap-4">
            {[...byDay.entries()].map(([day, items]) => (
              <div key={day}>
                <h2 className="mb-1 text-sm font-semibold text-fg-muted">
                  {formatDay(day)}
                </h2>
                <div className="flex flex-col gap-2">
                  {items.map((i) => (
                    <InterviewRow
                      key={i.id}
                      iv={i}
                      applicant={applicantName.get(i.application_id) ?? ""}
                      interviewer={
                        i.interviewer_id
                          ? employeeName.get(i.interviewer_id) ?? ""
                          : ""
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
      ) : completed.length === 0 ? (
        <p className="text-sm text-fg-muted">No completed interviews.</p>
      ) : (
        <div className="flex flex-col gap-2">
          {completed.map((i) => (
            <InterviewRow
              key={i.id}
              iv={i}
              applicant={applicantName.get(i.application_id) ?? ""}
              interviewer={
                i.interviewer_id ? employeeName.get(i.interviewer_id) ?? "" : ""
              }
              onComplete={(input) => completeMut.mutate({ id: i.id, input })}
              completing={completeMut.isPending}
            />
          ))}
        </div>
      )}
      {completeMut.isError && (
        <p className="text-sm text-danger">{String(completeMut.error)}</p>
      )}
    </section>
  );
}

interface InterviewRowProps {
  iv: Interview;
  applicant: string;
  interviewer: string;
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
  const [open, setOpen] = useState(false);
  const [rating, setRating] = useState<number>(iv.rating ?? 3);
  const [recommendation, setRecommendation] = useState(
    iv.recommendation || "neutral",
  );
  const [feedback, setFeedback] = useState(iv.feedback ?? "");

  return (
    <div className="rounded-md border border-border bg-bg-elevated p-2 text-[13px]">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-semibold">{applicant || iv.application_id}</span>
        <Badge variant="outline" size="xs">
          {humanType(iv.interview_type)}
        </Badge>
        <Badge variant={STATUS_BADGE[iv.status]} size="xs">
          {iv.status}
        </Badge>
        {iv.scheduled_at && (
          <span className="text-fg-muted">{formatTime(iv.scheduled_at)}</span>
        )}
        {interviewer && (
          <span className="text-fg-muted">· {interviewer}</span>
        )}
        {iv.duration_minutes > 0 && (
          <span className="text-fg-muted">· {iv.duration_minutes}m</span>
        )}
        {iv.status === "scheduled" && (
          <Button
            size="sm"
            variant="outline"
            className="ms-auto"
            onClick={() => setOpen((v) => !v)}
          >
            {open ? "Cancel" : "Record feedback"}
          </Button>
        )}
      </div>
      {(iv.meeting_link || iv.location) && (
        <div className="mt-0.5 text-fg-muted">
          {iv.meeting_link ? (
            <a
              href={iv.meeting_link}
              target="_blank"
              rel="noreferrer"
              className="text-accent underline"
            >
              Join link
            </a>
          ) : (
            iv.location
          )}
        </div>
      )}
      {iv.status !== "scheduled" && iv.feedback && (
        <p className="mt-1 whitespace-pre-wrap text-fg-muted">
          {iv.recommendation ? `[${iv.recommendation}] ` : ""}
          {iv.feedback}
        </p>
      )}
      {open && iv.status === "scheduled" && (
        <form
          className="mt-2 flex flex-col gap-2 rounded bg-bg-subtle p-2"
          onSubmit={(e) => {
            e.preventDefault();
            onComplete({ rating, recommendation, feedback });
            setOpen(false);
          }}
        >
          <div className="flex flex-wrap items-center gap-2">
            <label className="text-fg-muted">Rating</label>
            <Select
              className="w-auto"
              value={String(rating)}
              onChange={(e) => setRating(Number(e.target.value))}
            >
              {[1, 2, 3, 4, 5].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </Select>
            <label className="text-fg-muted">Recommendation</label>
            <Select
              className="w-auto"
              value={recommendation}
              onChange={(e) => setRecommendation(e.target.value)}
            >
              {RECOMMENDATIONS.map((r) => (
                <option key={r.value} value={r.value}>
                  {r.label}
                </option>
              ))}
            </Select>
          </div>
          <textarea
            className="min-h-[64px] rounded border border-border bg-bg p-2 text-fg"
            placeholder="Feedback…"
            value={feedback}
            onChange={(e) => setFeedback(e.target.value)}
          />
          <div>
            <Button size="sm" type="submit" disabled={completing}>
              {completing ? "Saving…" : "Complete interview"}
            </Button>
          </div>
        </form>
      )}
    </div>
  );
}

function ScheduleInterviewForm({
  applications,
  employees,
  onScheduled,
}: {
  applications: JobApplication[];
  employees: KRecord[];
  onScheduled: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<CreateInterviewInput>({
    application_id: "",
    interview_type: "video",
    duration_minutes: 60,
  });
  const [error, setError] = useState<string | null>(null);

  // Only applications actively in the pipeline can sensibly be
  // interviewed; terminal states are filtered out of the picker.
  const interviewable = applications.filter(
    (a) =>
      a.status !== "rejected" &&
      a.status !== "withdrawn" &&
      a.status !== "hired",
  );

  const createMut = useMutation({
    mutationFn: (input: CreateInterviewInput) => api.createInterview(input),
    onSuccess: () => {
      setForm({
        application_id: "",
        interview_type: "video",
        duration_minutes: 60,
      });
      setOpen(false);
      setError(null);
      onScheduled();
    },
    onError: (e) => setError(String(e)),
  });

  if (!open) {
    return (
      <div>
        <Button size="sm" onClick={() => setOpen(true)}>
          Schedule interview
        </Button>
      </div>
    );
  }

  return (
    <form
      className="flex flex-col gap-2 rounded-lg border border-border bg-bg-subtle p-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (!form.application_id) {
          setError("Select an application");
          return;
        }
        createMut.mutate({
          ...form,
          // datetime-local yields "YYYY-MM-DDTHH:mm"; widen to RFC3339
          // by appending seconds + Z so the server parses it as UTC.
          scheduled_at: form.scheduled_at
            ? new Date(form.scheduled_at).toISOString()
            : undefined,
        });
      }}
    >
      <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
        <Select
          value={form.application_id}
          onChange={(e) =>
            setForm({ ...form, application_id: e.target.value })
          }
          required
        >
          <option value="">Application…</option>
          {interviewable.map((a) => (
            <option key={a.id} value={a.id}>
              {a.applicant_name} ({a.status})
            </option>
          ))}
        </Select>
        <Select
          value={form.interview_type}
          onChange={(e) => {
            const interview_type = e.target.value;
            // In-person interviews carry a physical location; remote ones
            // carry a meeting link. Clear the field that no longer applies
            // so we never persist both (which would make the applicant
            // email render a location as a clickable "Join link").
            setForm({
              ...form,
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
        <Select
          value={form.interviewer_id ?? ""}
          onChange={(e) =>
            setForm({ ...form, interviewer_id: e.target.value || undefined })
          }
        >
          <option value="">Interviewer…</option>
          {employees.map((emp) => (
            <option key={emp.id} value={emp.id}>
              {(emp.data as EmployeeData)?.name ?? emp.id}
            </option>
          ))}
        </Select>
        <Input
          type="datetime-local"
          value={form.scheduled_at ?? ""}
          onChange={(e) => setForm({ ...form, scheduled_at: e.target.value })}
        />
        <Input
          type="number"
          min={15}
          step={15}
          placeholder="Duration (minutes)"
          value={form.duration_minutes ?? 60}
          onChange={(e) =>
            setForm({
              ...form,
              duration_minutes: Number(e.target.value) || 60,
            })
          }
        />
        {form.interview_type === "in_person" ? (
          <Input
            placeholder="Location (e.g. Room 4B)"
            value={form.location ?? ""}
            onChange={(e) => setForm({ ...form, location: e.target.value })}
          />
        ) : (
          <Input
            placeholder="Meeting link"
            value={form.meeting_link ?? ""}
            onChange={(e) => setForm({ ...form, meeting_link: e.target.value })}
          />
        )}
      </div>
      {error && <p className="text-sm text-danger">{error}</p>}
      <div className="flex gap-2">
        <Button size="sm" type="submit" disabled={createMut.isPending}>
          {createMut.isPending ? "Scheduling…" : "Schedule"}
        </Button>
        <Button
          size="sm"
          type="button"
          variant="ghost"
          onClick={() => {
            setOpen(false);
            setError(null);
          }}
        >
          Cancel
        </Button>
      </div>
    </form>
  );
}

function humanType(t: string): string {
  return INTERVIEW_TYPES.find((x) => x.value === t)?.label ?? t;
}

function formatDay(iso: string): string {
  if (iso === "Unscheduled") return iso;
  const d = new Date(`${iso}T00:00:00`);
  return d.toLocaleDateString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
    year: "numeric",
  });
}

function formatTime(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  });
}
