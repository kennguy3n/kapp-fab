import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  ApplicationStatus,
  JobApplication,
  JobOpening,
} from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  Eyebrow,
  Select,
  Skeleton,
  initials,
  toast,
  type BadgeProps,
} from "@kapp/ui";
import {
  AlertTriangle,
  ChevronRight,
  Inbox,
  RefreshCw,
  Star,
} from "lucide-react";
import { api } from "../lib/api";
import { humanizeToken } from "../lib/ktypeView";

// COLUMNS are the live pipeline lanes shown on the board — hired is
// always visible as the success endpoint. Rejected / withdrawn are
// surfaced via a toggle (TERMINAL_COLUMNS) so the active board stays
// focused on candidates still in flight.
const COLUMNS: Array<{ status: ApplicationStatus; label: string }> = [
  { status: "applied", label: "Applied" },
  { status: "screening", label: "Screening" },
  { status: "shortlisted", label: "Shortlisted" },
  { status: "interview", label: "Interview" },
  { status: "offered", label: "Offered" },
  { status: "hired", label: "Hired" },
];

const TERMINAL_COLUMNS: Array<{ status: ApplicationStatus; label: string }> = [
  { status: "rejected", label: "Rejected" },
  { status: "withdrawn", label: "Withdrawn" },
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

// Pipeline stage → Badge variant. The recruitment tokens aren't in the
// shared statusVariant map, so the domain maps them here.
function appStatusVariant(status: ApplicationStatus): BadgeProps["variant"] {
  switch (status) {
    case "applied":
      return "neutral";
    case "screening":
    case "shortlisted":
      return "info";
    case "interview":
      return "warning";
    case "offered":
      return "accent";
    case "hired":
      return "success";
    case "rejected":
      return "danger";
    case "withdrawn":
    default:
      return "neutral";
  }
}

/**
 * ApplicationsPage renders the recruitment pipeline as a kanban board
 * bucketed by stage. Cards can be dragged into the next legal lane — the
 * drop validates against ADVANCE_TARGETS before calling the advance
 * endpoint, so an illegal skip is a no-op. Each card also carries an
 * inline 1–5 rating that patches the row.
 */
export function ApplicationsPage() {
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const openingId = params.get("job_opening_id") ?? "";
  const [showTerminal, setShowTerminal] = useState(false);
  const [dragId, setDragId] = useState<string | null>(null);
  const [rejectTarget, setRejectTarget] = useState<JobApplication | null>(null);

  const openingsQ = useQuery<JobOpening[]>({
    queryKey: ["recruitment", "job-openings", ""],
    queryFn: () => api.listJobOpenings(),
  });
  const appsQ = useQuery<JobApplication[]>({
    queryKey: ["recruitment", "applications", openingId],
    queryFn: () =>
      api.listApplications(
        openingId ? { job_opening_id: openingId } : undefined,
      ),
  });

  const openingTitle = useMemo(() => {
    const m = new Map<string, string>();
    (openingsQ.data ?? []).forEach((o) => m.set(o.id, o.title));
    return m;
  }, [openingsQ.data]);

  const advanceMut = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) =>
      api.advanceApplication(id, status),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["recruitment", "applications"] }),
    onError: (e) =>
      toast.error("Couldn't move candidate", { description: String(e) }),
  });
  const rejectMut = useMutation({
    mutationFn: (id: string) => api.rejectApplication(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["recruitment", "applications"] });
      setRejectTarget(null);
      toast.success("Candidate rejected");
    },
    onError: (e) => toast.error("Couldn't reject", { description: String(e) }),
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
    onError: (e) =>
      toast.error("Couldn't save rating", { description: String(e) }),
  });

  const grouped = useMemo(() => {
    const m = new Map<ApplicationStatus, JobApplication[]>();
    [...COLUMNS, ...TERMINAL_COLUMNS].forEach((c) => m.set(c.status, []));
    (appsQ.data ?? []).forEach((a) => {
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
  const total = appsQ.data?.length ?? 0;

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
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Applications
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Move candidates through your hiring pipeline. Drag a card to the
            next lane, or use the button on each card.
          </p>
        </div>
      </header>

      <div className="flex flex-wrap items-center gap-2">
        <label className="text-sm font-medium text-fg-muted" htmlFor="opening-filter">
          Opening
        </label>
        <Select
          id="opening-filter"
          size="sm"
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
          {(openingsQ.data ?? []).map((o) => (
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

      {appsQ.isError && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
        >
          <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
          <span className="min-w-0 flex-1">We couldn't load applications.</span>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<RefreshCw className="h-4 w-4" />}
            onClick={() => appsQ.refetch()}
          >
            Retry
          </Button>
        </div>
      )}

      {appsQ.isLoading && <BoardSkeleton columns={visibleColumns.length} />}

      {!appsQ.isLoading && !appsQ.isError && total === 0 && (
        <EmptyState
          icon={<Inbox />}
          title="No applications yet"
          description={
            openingId
              ? "No one has applied to this opening yet. Try clearing the opening filter."
              : "Candidates will appear here as they apply to your open roles."
          }
        />
      )}

      {!appsQ.isLoading && !appsQ.isError && total > 0 && (
        <div className="overflow-x-auto pb-2">
          <div
            className="grid gap-3"
            style={{
              gridTemplateColumns: `repeat(${visibleColumns.length}, minmax(15rem, 1fr))`,
            }}
          >
            {visibleColumns.map((col) => {
              const cards = grouped.get(col.status) ?? [];
              return (
                <div
                  key={col.status}
                  className="flex min-h-[16rem] flex-col gap-2 rounded-lg border border-border bg-bg-subtle p-2"
                  onDragOver={(e) => {
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
                  <div className="flex items-center justify-between px-1">
                    <Badge variant={appStatusVariant(col.status)} size="sm">
                      {col.label}
                    </Badge>
                    <span className="text-xs tabular-nums text-fg-muted">
                      {cards.length}
                    </span>
                  </div>
                  <div className="flex flex-col gap-2">
                    {cards.map((a) => (
                      <ApplicationCard
                        key={a.id}
                        app={a}
                        openingTitle={openingTitle.get(a.job_opening_id) ?? ""}
                        showOpening={!openingId}
                        draggable={
                          ADVANCE_TARGETS[a.status as ApplicationStatus] !== null
                        }
                        onDragStart={() => setDragId(a.id)}
                        onDragEnd={() => setDragId(null)}
                        onAdvance={(status) =>
                          advanceMut.mutate({ id: a.id, status })
                        }
                        onReject={() => setRejectTarget(a)}
                        onRate={(rating) => rateMut.mutate({ app: a, rating })}
                        disabled={busy}
                      />
                    ))}
                    {cards.length === 0 && (
                      <p className="px-1 py-4 text-center text-xs text-fg-subtle">
                        Nothing here
                      </p>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}

      <ConfirmDialog
        open={rejectTarget !== null}
        onOpenChange={(o) => !o && setRejectTarget(null)}
        title="Reject this candidate?"
        description={
          rejectTarget
            ? `${rejectTarget.applicant_name} will be moved out of the active pipeline.`
            : ""
        }
        confirmLabel="Reject candidate"
        destructive
        loading={rejectMut.isPending}
        onConfirm={() => rejectTarget && rejectMut.mutate(rejectTarget.id)}
      />
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
  // Terminal states have no legal transitions out (server's
  // applicationTransitions table); 'hired' lives in its own lane.
  const terminal =
    app.status === "rejected" ||
    app.status === "withdrawn" ||
    app.status === "hired";
  return (
    <article
      className={`rounded-md border border-border bg-bg-elevated p-2.5 text-sm shadow-xs transition-colors ${
        draggable ? "cursor-grab active:cursor-grabbing hover:border-accent/50" : ""
      }`}
      draggable={draggable}
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
    >
      <div className="flex items-start gap-2">
        <Avatar size="sm">
          <AvatarFallback>{initials(app.applicant_name)}</AvatarFallback>
        </Avatar>
        <div className="min-w-0 flex-1">
          <div className="truncate font-medium text-fg">
            {app.applicant_name}
          </div>
          {app.applicant_email && (
            <div className="truncate text-xs text-fg-muted">
              {app.applicant_email}
            </div>
          )}
        </div>
      </div>
      {showOpening && openingTitle && (
        <div className="mt-1.5 truncate text-xs text-fg-muted">
          {openingTitle}
        </div>
      )}
      <div className="mt-2 flex items-center justify-between gap-2">
        <Badge variant="neutral" size="xs">
          {humanizeToken(app.source)}
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
              trailingIcon={<ChevronRight className="h-4 w-4" />}
              onClick={() => onAdvance(next)}
            >
              Advance to {humanizeToken(next)}
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
    </article>
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
    <span className="inline-flex items-center" role="group" aria-label="Candidate rating">
      {[1, 2, 3, 4, 5].map((n) => (
        <button
          key={n}
          type="button"
          disabled={disabled || n === value}
          aria-label={`Rate ${n} of 5`}
          aria-pressed={n <= value}
          className="rounded-sm p-0.5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) disabled:cursor-default"
          onClick={() => onChange(n)}
        >
          <Star
            className={
              n <= value
                ? "h-3.5 w-3.5 fill-warning text-warning"
                : "h-3.5 w-3.5 text-fg-subtle hover:text-fg-muted"
            }
          />
        </button>
      ))}
    </span>
  );
}

function BoardSkeleton({ columns }: { columns: number }) {
  return (
    <div className="overflow-x-auto pb-2">
      <div
        className="grid gap-3"
        style={{
          gridTemplateColumns: `repeat(${columns}, minmax(15rem, 1fr))`,
        }}
      >
        {Array.from({ length: columns }).map((_, i) => (
          <div
            key={i}
            className="flex min-h-[16rem] flex-col gap-2 rounded-lg border border-border bg-bg-subtle p-2"
          >
            <Skeleton className="h-5 w-24 rounded-md" />
            <Skeleton className="h-20 w-full rounded-md" />
            <Skeleton className="h-20 w-full rounded-md" />
          </div>
        ))}
      </div>
    </div>
  );
}
