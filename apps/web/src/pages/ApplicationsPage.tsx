import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  ApplicationStatus,
  JobApplication,
  JobOpening,
} from "@kapp/client";
import { Badge, Button, Select } from "@kapp/ui";
import { api } from "../lib/api";

// COLUMNS are the live pipeline lanes shown on the board. Terminal
// states (rejected / withdrawn / hired) are surfaced via a toggle so
// the active board stays focused on candidates still in flight.
const COLUMNS: Array<{ status: ApplicationStatus; label: string; accent: string }> =
  [
    { status: "applied", label: "Applied", accent: "bg-bg-muted" },
    { status: "screening", label: "Screening", accent: "bg-info/15" },
    { status: "shortlisted", label: "Shortlisted", accent: "bg-info/25" },
    { status: "interview", label: "Interview", accent: "bg-warning/25" },
    { status: "offered", label: "Offered", accent: "bg-accent/15" },
    { status: "hired", label: "Hired", accent: "bg-success/20" },
  ];

const TERMINAL_COLUMNS: Array<{
  status: ApplicationStatus;
  label: string;
  accent: string;
}> = [
  { status: "rejected", label: "Rejected", accent: "bg-danger/15" },
  { status: "withdrawn", label: "Withdrawn", accent: "bg-bg-muted" },
];

// ADVANCE_TARGETS mirrors the server-side applicationTransitions map
// (internal/hr/recruitment_store.go). The UI only offers legal forward
// moves; rejection/withdrawal are handled by dedicated buttons. Keeping
// this in lockstep avoids surfacing transitions the store will reject.
const ADVANCE_TARGETS: Record<ApplicationStatus, ApplicationStatus | null> = {
  applied: "screening",
  screening: "shortlisted",
  shortlisted: "interview",
  interview: "offered",
  offered: "hired",
  hired: null,
  rejected: null,
  withdrawn: null,
};

/**
 * ApplicationsPage renders the recruitment pipeline as a kanban board
 * bucketed by application status. Cards can be dragged into the next
 * legal lane (drag-to-advance) — the drop validates against
 * ADVANCE_TARGETS before calling the advance endpoint, so an illegal
 * skip is a no-op rather than a server round-trip that 409s. Each card
 * also carries an inline 1–5 rating control that patches the row.
 */
export function ApplicationsPage() {
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const openingId = params.get("job_opening_id") ?? "";
  const [showTerminal, setShowTerminal] = useState(false);
  const [dragId, setDragId] = useState<string | null>(null);

  const openingsQ = useQuery({
    queryKey: ["recruitment", "job-openings", ""],
    queryFn: () => api.listJobOpenings(),
  });
  const appsQ = useQuery({
    queryKey: ["recruitment", "applications", openingId],
    queryFn: () =>
      api.listApplications(
        openingId ? { job_opening_id: openingId } : undefined,
      ),
  });

  const openingTitle = useMemo(() => {
    const m = new Map<string, string>();
    (openingsQ.data ?? []).forEach((o: JobOpening) => m.set(o.id, o.title));
    return m;
  }, [openingsQ.data]);

  const advanceMut = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) =>
      api.advanceApplication(id, status),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["recruitment", "applications"] }),
  });
  const rejectMut = useMutation({
    mutationFn: (id: string) => api.rejectApplication(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["recruitment", "applications"] }),
  });
  const rateMut = useMutation({
    mutationFn: ({ app, rating }: { app: JobApplication; rating: number }) =>
      api.updateApplication(app.id, {
        applicant_name: app.applicant_name,
        applicant_email: app.applicant_email,
        phone: app.phone,
        resume_file_id: app.resume_file_id ?? undefined,
        cover_letter: app.cover_letter,
        source: app.source,
        referrer_employee_id: app.referrer_employee_id ?? undefined,
        rating,
        notes: app.notes,
      }),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["recruitment", "applications"] }),
  });

  const grouped = useMemo(() => {
    const m = new Map<ApplicationStatus, JobApplication[]>();
    [...COLUMNS, ...TERMINAL_COLUMNS].forEach((c) => m.set(c.status, []));
    (appsQ.data ?? []).forEach((a: JobApplication) => {
      const arr = m.get(a.status as ApplicationStatus);
      if (arr) arr.push(a);
    });
    return m;
  }, [appsQ.data]);

  const visibleColumns = showTerminal
    ? [...COLUMNS, ...TERMINAL_COLUMNS]
    : COLUMNS;
  const terminalCount =
    (grouped.get("rejected") ?? []).length +
    (grouped.get("withdrawn") ?? []).length;
  const busy = advanceMut.isPending || rejectMut.isPending || rateMut.isPending;

  function onDropTo(target: ApplicationStatus) {
    const id = dragId;
    setDragId(null);
    if (!id) return;
    const app = (appsQ.data ?? []).find((a) => a.id === id);
    if (!app) return;
    if (ADVANCE_TARGETS[app.status as ApplicationStatus] === target) {
      advanceMut.mutate({ id, status: target });
    }
  }

  return (
    <section className="flex flex-col gap-3">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Applications
      </h1>
      <p className="text-sm text-fg-muted">
        Candidate pipeline. Drag a card into the next lane to advance it,
        or use the buttons. Advancing into Hired auto-creates a draft
        employee record.
      </p>

      <div className="flex flex-wrap items-center gap-2">
        <label className="text-sm text-fg-muted" htmlFor="opening-filter">
          Opening
        </label>
        <Select
          id="opening-filter"
          className="w-auto"
          value={openingId}
          onChange={(e) => {
            const next = new URLSearchParams(params);
            if (e.target.value) next.set("job_opening_id", e.target.value);
            else next.delete("job_opening_id");
            setParams(next);
          }}
        >
          <option value="">All openings</option>
          {(openingsQ.data ?? []).map((o: JobOpening) => (
            <option key={o.id} value={o.id}>
              {o.title}
            </option>
          ))}
        </Select>
        <Button
          size="sm"
          variant={showTerminal ? "secondary" : "outline"}
          aria-pressed={showTerminal}
          onClick={() => setShowTerminal((v) => !v)}
        >
          {showTerminal ? "Hide" : "Show"} closed ({terminalCount})
        </Button>
      </div>

      {appsQ.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {appsQ.isError && (
        <p className="text-sm text-danger">{String(appsQ.error)}</p>
      )}
      {advanceMut.isError && (
        <p className="text-sm text-danger">{String(advanceMut.error)}</p>
      )}

      <div
        className="grid gap-3"
        style={{
          gridTemplateColumns: `repeat(${visibleColumns.length}, minmax(180px, 1fr))`,
        }}
      >
        {visibleColumns.map((col) => (
          <div
            key={col.status}
            className="min-h-[240px] rounded-lg bg-bg-subtle p-2"
            onDragOver={(e) => {
              // Only present a drop target for the next legal lane.
              if (dragId) {
                const app = (appsQ.data ?? []).find((a) => a.id === dragId);
                if (
                  app &&
                  ADVANCE_TARGETS[app.status as ApplicationStatus] ===
                    col.status
                ) {
                  e.preventDefault();
                }
              }
            }}
            onDrop={() => onDropTo(col.status)}
          >
            <div className={`mb-2 rounded px-2 py-1 font-semibold ${col.accent}`}>
              {col.label} ({(grouped.get(col.status) ?? []).length})
            </div>
            {(grouped.get(col.status) ?? []).map((a) => (
              <ApplicationCard
                key={a.id}
                app={a}
                openingTitle={openingTitle.get(a.job_opening_id) ?? ""}
                showOpening={!openingId}
                draggable={ADVANCE_TARGETS[a.status as ApplicationStatus] !== null}
                onDragStart={() => setDragId(a.id)}
                onDragEnd={() => setDragId(null)}
                onAdvance={(status) => advanceMut.mutate({ id: a.id, status })}
                onReject={() => rejectMut.mutate(a.id)}
                onRate={(rating) => rateMut.mutate({ app: a, rating })}
                disabled={busy}
              />
            ))}
          </div>
        ))}
      </div>
    </section>
  );
}

interface ApplicationCardProps {
  app: JobApplication;
  openingTitle: string;
  showOpening: boolean;
  draggable: boolean;
  onDragStart: () => void;
  onDragEnd: () => void;
  onAdvance: (status: string) => void;
  onReject: () => void;
  onRate: (rating: number) => void;
  disabled: boolean;
}

function ApplicationCard({
  app,
  openingTitle,
  showOpening,
  draggable,
  onDragStart,
  onDragEnd,
  onAdvance,
  onReject,
  onRate,
  disabled,
}: ApplicationCardProps) {
  const next = ADVANCE_TARGETS[app.status as ApplicationStatus];
  const terminal = app.status === "rejected" || app.status === "withdrawn";
  return (
    <div
      className="mb-2 cursor-grab rounded-md border border-border bg-bg-elevated p-2 text-[13px] active:cursor-grabbing"
      draggable={draggable}
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
    >
      <div className="font-semibold">{app.applicant_name}</div>
      {app.applicant_email && (
        <div className="truncate text-fg-muted">{app.applicant_email}</div>
      )}
      {showOpening && openingTitle && (
        <div className="mt-0.5 text-fg-muted">{openingTitle}</div>
      )}
      <div className="mt-1 flex items-center gap-1">
        <Badge variant="outline" size="xs">
          {app.source}
        </Badge>
        <RatingStars
          value={app.rating ?? 0}
          onChange={onRate}
          disabled={disabled}
        />
      </div>
      {!terminal && (
        <div className="mt-2 flex flex-wrap items-center gap-1">
          {next && (
            <Button
              size="sm"
              variant="outline"
              disabled={disabled}
              onClick={() => onAdvance(next)}
            >
              → {next}
            </Button>
          )}
          <Button
            size="sm"
            variant="ghost"
            disabled={disabled}
            onClick={onReject}
          >
            Reject
          </Button>
        </div>
      )}
    </div>
  );
}

// RatingStars is a compact 1–5 control. Clicking the current rating
// again is a no-op (avoids a redundant PATCH); clicking a new value
// patches the application row.
function RatingStars({
  value,
  onChange,
  disabled,
}: {
  value: number;
  onChange: (v: number) => void;
  disabled: boolean;
}) {
  return (
    <span className="inline-flex" role="group" aria-label="Rating">
      {[1, 2, 3, 4, 5].map((n) => (
        <button
          key={n}
          type="button"
          disabled={disabled || n === value}
          aria-label={`Rate ${n}`}
          className={
            n <= value ? "text-warning" : "text-fg-subtle hover:text-fg-muted"
          }
          onClick={() => onChange(n)}
        >
          ★
        </button>
      ))}
    </span>
  );
}
