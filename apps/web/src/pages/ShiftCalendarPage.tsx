import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
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
  initials,
  toast,
  cn,
} from "@kapp/ui";
import {
  AlertTriangle,
  ChevronLeft,
  ChevronRight,
  Plus,
  RefreshCw,
  Users,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import { humanizeToken, statusVariant } from "../lib/ktypeView";

const KTYPE_SHIFT_TYPE = "hr.shift_type";
const KTYPE_SHIFT_ASSIGNMENT = "hr.shift_assignment";
const KTYPE_EMPLOYEE = "hr.employee";

interface ShiftTypeData {
  name?: string;
  start_time?: string;
  end_time?: string;
  color?: string;
  department?: string;
  active?: boolean;
}

interface ShiftAssignmentData {
  employee_id?: string;
  shift_type_id?: string;
  shift_date?: string;
  status?: string;
  notes?: string;
}

interface EmployeeData {
  name?: string;
  department?: string;
}

type View = "week" | "month";
type ShiftType = { id: string } & ShiftTypeData;
type Employee = { id: string; name?: string; department?: string };

/**
 * ShiftCalendarPage is a calendar-first roster: employees down the
 * side, dates across the top, and each scheduled shift shown in its
 * cell. Operators schedule in two clicks — click an open day to open a
 * pre-filled form, pick a shift, done. Week and month views share the
 * same grid.
 */
export function ShiftCalendarPage() {
  const fmt = useFormatter();
  const [view, setView] = useState<View>("week");
  const [anchor, setAnchor] = useState(() => isoDate(new Date()));
  const [assignTarget, setAssignTarget] = useState<{
    employeeId?: string;
    date?: string;
  } | null>(null);

  const employeesQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_EMPLOYEE],
    queryFn: () => api.listRecords(KTYPE_EMPLOYEE),
  });
  const shiftTypesQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_SHIFT_TYPE],
    queryFn: () => api.listRecords(KTYPE_SHIFT_TYPE),
  });
  const assignmentsQ = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_SHIFT_ASSIGNMENT],
    queryFn: () => api.listRecords(KTYPE_SHIFT_ASSIGNMENT),
  });

  const employees: Employee[] = useMemo(
    () =>
      (employeesQ.data ?? []).map((r) => ({
        id: r.id,
        ...(r.data as EmployeeData),
      })),
    [employeesQ.data],
  );
  const shiftTypes = useMemo(
    () =>
      new Map<string, ShiftType>(
        (shiftTypesQ.data ?? []).map((r) => [
          r.id,
          { id: r.id, ...(r.data as ShiftTypeData) },
        ]),
      ),
    [shiftTypesQ.data],
  );
  const assignmentsByCell = useMemo(
    () => indexAssignments(assignmentsQ.data ?? [], shiftTypes),
    [assignmentsQ.data, shiftTypes],
  );

  const dates = useMemo(() => buildDateRange(anchor, view), [anchor, view]);
  const loading =
    employeesQ.isLoading || shiftTypesQ.isLoading || assignmentsQ.isLoading;
  const error = employeesQ.isError || shiftTypesQ.isError || assignmentsQ.isError;

  function step(dir: -1 | 1) {
    const d = new Date(`${anchor}T00:00:00`);
    if (view === "week") d.setDate(d.getDate() + dir * 7);
    else d.setMonth(d.getMonth() + dir);
    setAnchor(isoDate(d));
  }

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Shift Schedule
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            See who's working when. Click any open day to schedule a shift.
          </p>
        </div>
        <Button
          size="sm"
          leadingIcon={<Plus className="h-4 w-4" />}
          disabled={employees.length === 0 || shiftTypes.size === 0}
          onClick={() => setAssignTarget({})}
        >
          Schedule shift
        </Button>
      </header>

      <div className="flex flex-wrap items-center justify-between gap-3">
        <Tabs value={view} onValueChange={(v) => setView(v as View)}>
          <TabsList>
            <TabsTrigger value="week">Week</TabsTrigger>
            <TabsTrigger value="month">Month</TabsTrigger>
          </TabsList>
        </Tabs>
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex items-center gap-1">
            <Button
              size="icon"
              variant="outline"
              aria-label="Previous period"
              onClick={() => step(-1)}
            >
              <ChevronLeft className="h-4 w-4" />
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => setAnchor(isoDate(new Date()))}
            >
              Today
            </Button>
            <Button
              size="icon"
              variant="outline"
              aria-label="Next period"
              onClick={() => step(1)}
            >
              <ChevronRight className="h-4 w-4" />
            </Button>
          </div>
          <Field label="Jump to date" hideLabel>
            <Input
              size="sm"
              type="date"
              className="w-auto"
              value={anchor}
              onChange={(e) => setAnchor(e.target.value || anchor)}
            />
          </Field>
          <span className="text-sm font-medium text-fg">
            {fmt.date(new Date(`${dates[0]}T00:00:00`))} –{" "}
            {fmt.date(new Date(`${dates[dates.length - 1]}T00:00:00`))}
          </span>
        </div>
      </div>

      {error && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
        >
          <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
          <span className="min-w-0 flex-1">
            We couldn't load the schedule.
          </span>
          <Button
            size="sm"
            variant="outline"
            leadingIcon={<RefreshCw className="h-4 w-4" />}
            onClick={() => {
              employeesQ.refetch();
              shiftTypesQ.refetch();
              assignmentsQ.refetch();
            }}
          >
            Retry
          </Button>
        </div>
      )}

      {loading && <ScheduleSkeleton cols={dates.length} />}

      {!loading && !error && employees.length === 0 && (
        <EmptyState
          icon={<Users />}
          title="No employees to schedule"
          description="Add people in the Employees area first, then come back to build their roster."
        />
      )}

      {!loading && !error && employees.length > 0 && (
        <ScheduleGrid
          dates={dates}
          employees={employees}
          shiftTypes={shiftTypes}
          assignmentsByCell={assignmentsByCell}
          onPick={(employeeId, date) => setAssignTarget({ employeeId, date })}
        />
      )}

      <AssignShiftModal
        open={assignTarget !== null}
        onOpenChange={(o) => !o && setAssignTarget(null)}
        employees={employees}
        shiftTypes={Array.from(shiftTypes.values())}
        initialEmployeeId={assignTarget?.employeeId ?? ""}
        initialDate={assignTarget?.date ?? isoDate(new Date())}
      />
    </section>
  );
}

function ScheduleGrid({
  dates,
  employees,
  shiftTypes,
  assignmentsByCell,
  onPick,
}: {
  dates: string[];
  employees: Employee[];
  shiftTypes: Map<string, ShiftType>;
  assignmentsByCell: Map<string, KRecord[]>;
  onPick: (employeeId: string, date: string) => void;
}) {
  const today = isoDate(new Date());
  return (
    <div className="max-h-[34rem] overflow-auto rounded-lg border border-border">
      <table className="w-full border-separate border-spacing-0 text-sm">
        <thead>
          <tr>
            <th className="sticky left-0 top-0 z-30 border-b border-border bg-bg-subtle px-3 py-2 text-start text-xs font-medium text-fg-muted">
              Employee
            </th>
            {dates.map((d) => (
              <th
                key={d}
                className={cn(
                  "sticky top-0 z-20 min-w-[6rem] border-b border-l border-border bg-bg-subtle px-2 py-2 text-center text-xs font-medium",
                  isWeekend(d) ? "text-fg-subtle" : "text-fg-muted",
                  d === today && "bg-accent/10 text-accent",
                )}
              >
                <DateHeader iso={d} />
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {employees.map((e) => (
            <tr key={e.id} className="group">
              <td className="sticky left-0 z-10 border-b border-border bg-bg-elevated px-3 py-2 align-middle">
                <div className="flex items-center gap-2">
                  <Avatar size="xs">
                    <AvatarFallback>{initials(e.name ?? "?")}</AvatarFallback>
                  </Avatar>
                  <div className="min-w-0">
                    <div className="truncate text-sm font-medium text-fg">
                      {e.name ?? "Unnamed"}
                    </div>
                    {e.department && (
                      <div className="truncate text-xs text-fg-subtle">
                        {e.department}
                      </div>
                    )}
                  </div>
                </div>
              </td>
              {dates.map((d) => {
                const key = cellKey(e.id, d);
                const recs = assignmentsByCell.get(key) ?? [];
                return (
                  <td
                    key={key}
                    className={cn(
                      "border-b border-l border-border p-1 align-top",
                      isWeekend(d) && "bg-bg-subtle/40",
                      d === today && "bg-accent/5",
                    )}
                  >
                    <button
                      type="button"
                      aria-label={`Schedule a shift for ${e.name ?? "employee"} on ${d}`}
                      onClick={() => onPick(e.id, d)}
                      className="flex min-h-[2.75rem] w-full flex-col gap-1 rounded-md p-1 text-start transition-colors hover:bg-bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)"
                    >
                      {recs.length === 0 ? (
                        <span className="flex items-center gap-1 text-xs text-fg-subtle opacity-0 transition-opacity group-hover:opacity-100">
                          <Plus className="h-3 w-3" /> Add
                        </span>
                      ) : (
                        recs.map((rec) => {
                          const data = rec.data as ShiftAssignmentData;
                          const st = data.shift_type_id
                            ? shiftTypes.get(data.shift_type_id)
                            : undefined;
                          return (
                            <ShiftBadge
                              key={rec.id}
                              label={st?.name ?? "Shift"}
                              time={formatRange(st?.start_time, st?.end_time)}
                              color={st?.color}
                              status={data.status ?? "scheduled"}
                            />
                          );
                        })
                      )}
                    </button>
                  </td>
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function DateHeader({ iso }: { iso: string }) {
  const d = new Date(`${iso}T00:00:00`);
  return (
    <div className="flex flex-col leading-tight">
      <span>{d.toLocaleDateString(undefined, { weekday: "short" })}</span>
      <span className="text-sm font-semibold tabular-nums">{d.getDate()}</span>
    </div>
  );
}

/**
 * Tenant shift colors are free-form hex, so the text painted on top can't
 * rely on theme tokens (which assume the token background). Derive a readable
 * foreground from the fill's WCAG relative luminance — white on dark fills,
 * near-black on light ones — so chips always meet contrast in both themes.
 * Returns undefined for unparseable input so we fall back to design tokens.
 */
function readableForeground(hex: string): string | undefined {
  const match = /^#?([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(hex.trim());
  if (!match) return undefined;
  let h = match[1];
  if (h.length === 3)
    h = h
      .split("")
      .map((c) => c + c)
      .join("");
  const channel = (i: number) => parseInt(h.slice(i, i + 2), 16) / 255;
  const linear = (c: number) =>
    c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  const luminance =
    0.2126 * linear(channel(0)) +
    0.7152 * linear(channel(2)) +
    0.0722 * linear(channel(4));
  // 0.179 is the crossover where contrast against white equals black.
  return luminance > 0.179 ? "#191919" : "#ffffff";
}

function ShiftBadge({
  label,
  time,
  color,
  status,
}: {
  label: string;
  time: string;
  color?: string;
  status: string;
}) {
  // `color` is tenant-defined per shift type (free-form hex stored on the
  // record), so it stays an inline background and the text color is derived
  // from it for contrast; everything else is driven by design tokens. Fall
  // back to the info tint + tokens when no color is set.
  const fg = color ? readableForeground(color) : undefined;
  return (
    <div
      className={cn(
        "rounded-md px-1.5 py-1 text-start text-xs leading-tight",
        !color && "bg-info/15",
      )}
      style={color ? { background: color, color: fg } : undefined}
    >
      <div className={cn("font-semibold", !color && "text-fg")}>{label}</div>
      {time && (
        <div className={cn(!color && "text-fg-muted")}>{time}</div>
      )}
      {status !== "scheduled" && (
        <Badge variant={statusVariant(status)} size="xs" className="mt-0.5">
          {humanizeToken(status)}
        </Badge>
      )}
    </div>
  );
}

function AssignShiftModal({
  open,
  onOpenChange,
  employees,
  shiftTypes,
  initialEmployeeId,
  initialDate,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  employees: Employee[];
  shiftTypes: ShiftType[];
  initialEmployeeId: string;
  initialDate: string;
}) {
  return (
    <Modal open={open} onOpenChange={onOpenChange}>
      <ModalContent>
        <ModalHeader>
          <ModalTitle>Schedule a shift</ModalTitle>
          <ModalDescription>
            Assign a shift to a team member on a given day.
          </ModalDescription>
        </ModalHeader>
        {/* Keyed by the cell so switching cells while the modal stays open
            remounts the form to re-seed it. (Open → close → reopen resets via
            the Radix portal unmounting the form, independent of this key.) */}
        <AssignShiftForm
          key={`${initialEmployeeId}|${initialDate}`}
          employees={employees}
          shiftTypes={shiftTypes}
          initialEmployeeId={initialEmployeeId}
          initialDate={initialDate}
          onClose={() => onOpenChange(false)}
        />
      </ModalContent>
    </Modal>
  );
}

function AssignShiftForm({
  employees,
  shiftTypes,
  initialEmployeeId,
  initialDate,
  onClose,
}: {
  employees: Employee[];
  shiftTypes: ShiftType[];
  initialEmployeeId: string;
  initialDate: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [employeeId, setEmployeeId] = useState(initialEmployeeId);
  const [shiftTypeId, setShiftTypeId] = useState("");
  const [shiftDate, setShiftDate] = useState(initialDate);
  const [notes, setNotes] = useState("");
  const [submitted, setSubmitted] = useState(false);

  const create = useMutation({
    mutationFn: () =>
      api.createRecord(KTYPE_SHIFT_ASSIGNMENT, {
        employee_id: employeeId,
        shift_type_id: shiftTypeId,
        shift_date: shiftDate,
        status: "scheduled",
        notes: notes || undefined,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE_SHIFT_ASSIGNMENT] });
      toast.success("Shift scheduled");
      onClose();
    },
    onError: (e) =>
      toast.error("Couldn't schedule shift", { description: String(e) }),
  });

  const valid = employeeId && shiftTypeId && shiftDate;

  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        setSubmitted(true);
        if (!valid) return;
        create.mutate();
      }}
    >
      <Field
        label="Employee"
        required
        error={submitted && !employeeId ? "Choose an employee." : undefined}
      >
        <Select
          value={employeeId}
          onChange={(e) => setEmployeeId(e.target.value)}
        >
          <option value="">Select employee…</option>
          {employees.map((e) => (
            <option key={e.id} value={e.id}>
              {e.name ?? "Unnamed"}
            </option>
          ))}
        </Select>
      </Field>
      <Field
        label="Shift type"
        required
        error={submitted && !shiftTypeId ? "Choose a shift type." : undefined}
        help={
          shiftTypes.length === 0
            ? "No shift types defined yet — create one first."
            : undefined
        }
      >
        <Select
          value={shiftTypeId}
          onChange={(e) => setShiftTypeId(e.target.value)}
        >
          <option value="">Select shift type…</option>
          {shiftTypes.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name ?? "Shift"}
              {s.start_time ? ` (${formatRange(s.start_time, s.end_time)})` : ""}
            </option>
          ))}
        </Select>
      </Field>
      <Field
        label="Date"
        required
        error={submitted && !shiftDate ? "Pick a date." : undefined}
      >
        <Input
          type="date"
          value={shiftDate}
          onChange={(e) => setShiftDate(e.target.value)}
        />
      </Field>
      <Field label="Notes" help="Optional — visible to schedulers.">
        <Textarea
          rows={2}
          placeholder="e.g. Covering for Sam"
          value={notes}
          onChange={(e) => setNotes(e.target.value)}
        />
      </Field>
      {create.isError && (
        <p className="text-sm text-danger">
          Couldn't schedule the shift: {String(create.error)}
        </p>
      )}
      <ModalFooter>
        <Button type="button" variant="ghost" onClick={onClose}>
          Cancel
        </Button>
        <Button type="submit" disabled={create.isPending}>
          {create.isPending ? "Scheduling…" : "Schedule shift"}
        </Button>
      </ModalFooter>
    </form>
  );
}

function ScheduleSkeleton({ cols }: { cols: number }) {
  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <div className="flex border-b border-border bg-bg-subtle">
        <div className="w-44 px-3 py-2">
          <Skeleton className="h-4 w-20" />
        </div>
        {Array.from({ length: Math.min(cols, 7) }).map((_, i) => (
          <div key={i} className="flex-1 px-2 py-2">
            <Skeleton className="mx-auto h-8 w-8" />
          </div>
        ))}
      </div>
      {Array.from({ length: 5 }).map((_, r) => (
        <div key={r} className="flex border-b border-border">
          <div className="flex w-44 items-center gap-2 px-3 py-2">
            <Skeleton variant="circle" className="h-5 w-5" />
            <Skeleton className="h-4 w-24" />
          </div>
          {Array.from({ length: Math.min(cols, 7) }).map((_, i) => (
            <div key={i} className="flex-1 px-2 py-2">
              <Skeleton className="h-10 w-full" />
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function indexAssignments(
  records: KRecord[],
  shiftTypes: Map<string, ShiftType>,
): Map<string, KRecord[]> {
  // Split shifts (e.g. an employee scheduled for both a Morning and an
  // Evening shift on the same date) are valid, so the calendar collects
  // an array per (employee, date) cell and sorts each by the resolved
  // shift_type.start_time for a stable visual stack. Assignments missing
  // a type/start fall to the bottom via a "99:99" sentinel.
  const out = new Map<string, KRecord[]>();
  for (const r of records) {
    const data = r.data as ShiftAssignmentData;
    if (!data.employee_id || !data.shift_date) continue;
    const key = cellKey(data.employee_id, data.shift_date);
    const arr = out.get(key) ?? [];
    arr.push(r);
    out.set(key, arr);
  }
  for (const arr of out.values()) {
    arr.sort((a, b) => {
      const aData = a.data as ShiftAssignmentData;
      const bData = b.data as ShiftAssignmentData;
      const aStart =
        (aData.shift_type_id
          ? shiftTypes.get(aData.shift_type_id)?.start_time
          : undefined) ?? "99:99";
      const bStart =
        (bData.shift_type_id
          ? shiftTypes.get(bData.shift_type_id)?.start_time
          : undefined) ?? "99:99";
      return aStart.localeCompare(bStart);
    });
  }
  return out;
}

function cellKey(employeeID: string, date: string): string {
  return `${employeeID}::${date}`;
}

function isoDate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

function isWeekend(iso: string): boolean {
  const day = new Date(`${iso}T00:00:00`).getDay();
  return day === 0 || day === 6;
}

/** Trim an "HH:MM:SS" time to "HH:MM" and join a start/end into a range. */
function formatRange(start?: string, end?: string): string {
  const t = (s?: string) => (s ? s.slice(0, 5) : "");
  const a = t(start);
  const b = t(end);
  if (a && b) return `${a}–${b}`;
  return a || b || "";
}

function buildDateRange(anchor: string, view: View): string[] {
  const start = new Date(`${anchor}T00:00:00`);
  const out: string[] = [];
  if (view === "week") {
    const offset = start.getDay();
    start.setDate(start.getDate() - offset);
    for (let i = 0; i < 7; i++) {
      const d = new Date(start);
      d.setDate(start.getDate() + i);
      out.push(isoDate(d));
    }
  } else {
    const first = new Date(start.getFullYear(), start.getMonth(), 1);
    const last = new Date(start.getFullYear(), start.getMonth() + 1, 0);
    const days = last.getDate();
    for (let i = 0; i < days; i++) {
      const d = new Date(first);
      d.setDate(first.getDate() + i);
      out.push(isoDate(d));
    }
  }
  return out;
}
