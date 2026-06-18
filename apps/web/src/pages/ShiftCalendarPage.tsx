import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { Button, Input, Select, cn } from "@kapp/ui";
import { api } from "../lib/api";
import { toCalendarISO } from "../lib/date";

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

/**
 * ShiftCalendarPage renders the Phase M shift schedule. Two views:
 *
 * - Week: a 7-day grid keyed by employee row × date column. Each
 *   cell shows the shift_type badge for any hr.shift_assignment
 *   matching (employee_id, date).
 * - Month: same shape projected onto the current month's calendar.
 *
 * The page is a thin client over /records/* — no dedicated handler
 * is needed because both KTypes go through the generic KRecord
 * surface. New assignments are created inline via the agent tool
 * `hr.assign_shift` would normally use, but the form here calls
 * createRecord directly so operators can schedule without touching
 * the agent surface.
 */
export function ShiftCalendarPage() {
  const [view, setView] = useState<View>("week");
  const [anchor, setAnchor] = useState(() => toCalendarISO(new Date()));

  const employeesQ = useQuery({
    queryKey: ["records", KTYPE_EMPLOYEE],
    queryFn: () => api.listRecords(KTYPE_EMPLOYEE),
  });
  const shiftTypesQ = useQuery({
    queryKey: ["records", KTYPE_SHIFT_TYPE],
    queryFn: () => api.listRecords(KTYPE_SHIFT_TYPE),
  });
  const assignmentsQ = useQuery({
    queryKey: ["records", KTYPE_SHIFT_ASSIGNMENT],
    queryFn: () => api.listRecords(KTYPE_SHIFT_ASSIGNMENT),
  });

  const employees = useMemo(
    () =>
      (employeesQ.data ?? []).map((r) => ({
        id: r.id,
        ...(r.data as EmployeeData),
      })),
    [employeesQ.data],
  );
  const shiftTypes = useMemo(
    () =>
      new Map(
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

  return (
    <section className="flex flex-col gap-2">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Shift Schedule
      </h1>
      <p className="text-sm text-fg-muted">
        Phase M shift calendar. Rows are employees, columns are dates,
        cells render any matching hr.shift_assignment for that
        (employee, date) tuple. Click an empty cell to schedule.
      </p>
      <header className="mb-3 flex items-center gap-2">
        <Button
          size="sm"
          variant={view === "week" ? "primary" : "outline"}
          onClick={() => setView("week")}
          disabled={view === "week"}
        >
          Week
        </Button>
        <Button
          size="sm"
          variant={view === "month" ? "primary" : "outline"}
          onClick={() => setView("month")}
          disabled={view === "month"}
        >
          Month
        </Button>
        <Input
          type="date"
          value={anchor}
          onChange={(e) => setAnchor(e.target.value)}
          className="ml-3 w-auto"
        />
        <span className="ml-3 text-sm text-fg-muted">
          {dates[0]} → {dates[dates.length - 1]}
        </span>
      </header>
      <ScheduleForm shiftTypes={Array.from(shiftTypes.values())} employees={employees} />
      {(employeesQ.isLoading || shiftTypesQ.isLoading || assignmentsQ.isLoading) && (
        <p className="text-sm text-fg-muted">Loading…</p>
      )}
      {employees.length === 0 ? (
        <p className="text-sm text-fg-muted">No employees yet.</p>
      ) : (
        <ScheduleGrid
          dates={dates}
          employees={employees}
          shiftTypes={shiftTypes}
          assignmentsByCell={assignmentsByCell}
        />
      )}
    </section>
  );
}

function ScheduleGrid({
  dates,
  employees,
  shiftTypes,
  assignmentsByCell,
}: {
  dates: string[];
  employees: { id: string; name?: string }[];
  shiftTypes: Map<string, { id: string } & ShiftTypeData>;
  assignmentsByCell: Map<string, KRecord[]>;
}) {
  return (
    <div className="overflow-x-auto">
      <table className="min-w-full border-collapse">
        <thead>
          <tr>
            <th className={TH}>Employee</th>
            {dates.map((d) => (
              <th key={d} className={TH}>
                {shortDate(d)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {employees.map((e) => (
            <tr key={e.id}>
              <td className={TD}>{e.name ?? "(unnamed)"}</td>
              {dates.map((d) => {
                const key = cellKey(e.id, d);
                const recs = assignmentsByCell.get(key) ?? [];
                if (recs.length === 0)
                  return <td key={key} className={cn(TD, "bg-bg-subtle")} />;
                return (
                  <td key={key} className={TD}>
                    <div className="flex flex-col gap-1">
                      {recs.map((rec) => {
                        const data = rec.data as ShiftAssignmentData;
                        const st = data.shift_type_id
                          ? shiftTypes.get(data.shift_type_id)
                          : undefined;
                        return (
                          <ShiftBadge
                            key={rec.id}
                            label={st?.name ?? "shift"}
                            time={st ? `${st.start_time ?? ""}–${st.end_time ?? ""}` : ""}
                            color={st?.color}
                            status={data.status ?? "scheduled"}
                          />
                        );
                      })}
                    </div>
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
  // `color` is tenant-defined per shift_type (free-form hex stored on
  // the record), so it stays an inline background; everything else is
  // driven by design tokens. Fall back to the info tint when unset.
  return (
    <div
      className={cn(
        "rounded px-1.5 py-1 text-xs leading-tight",
        !color && "bg-info/15",
      )}
      style={color ? { background: color } : undefined}
    >
      <div className="font-semibold">{label}</div>
      {time && <div className="text-fg-muted">{time}</div>}
      {status !== "scheduled" && (
        <div className="text-[10px] text-fg-subtle">{status}</div>
      )}
    </div>
  );
}

function ScheduleForm({
  shiftTypes,
  employees,
}: {
  shiftTypes: ({ id: string } & ShiftTypeData)[];
  employees: { id: string; name?: string }[];
}) {
  const qc = useQueryClient();
  const [employeeId, setEmployeeId] = useState("");
  const [shiftTypeId, setShiftTypeId] = useState("");
  const [shiftDate, setShiftDate] = useState(() => toCalendarISO(new Date()));
  const [error, setError] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.createRecord(KTYPE_SHIFT_ASSIGNMENT, {
        employee_id: employeeId,
        shift_type_id: shiftTypeId,
        shift_date: shiftDate,
        status: "scheduled",
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE_SHIFT_ASSIGNMENT] });
      setEmployeeId("");
      setShiftTypeId("");
      setError(null);
    },
    onError: (err: Error) => setError(err.message),
  });

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (!employeeId || !shiftTypeId || !shiftDate) {
          setError("employee, shift type, and date are required");
          return;
        }
        create.mutate();
      }}
      className="mb-3 flex flex-wrap items-center gap-2"
    >
      <Select
        className="w-auto"
        value={employeeId}
        onChange={(e) => setEmployeeId(e.target.value)}
      >
        <option value="">Select employee…</option>
        {employees.map((e) => (
          <option key={e.id} value={e.id}>
            {e.name ?? e.id}
          </option>
        ))}
      </Select>
      <Select
        className="w-auto"
        value={shiftTypeId}
        onChange={(e) => setShiftTypeId(e.target.value)}
      >
        <option value="">Select shift type…</option>
        {shiftTypes.map((s) => (
          <option key={s.id} value={s.id}>
            {s.name ?? s.id}
          </option>
        ))}
      </Select>
      <Input
        type="date"
        value={shiftDate}
        onChange={(e) => setShiftDate(e.target.value)}
        className="w-auto"
      />
      <Button type="submit" disabled={create.isPending}>
        {create.isPending ? "Scheduling…" : "Schedule"}
      </Button>
      {error && <span className="text-sm text-danger">{error}</span>}
    </form>
  );
}

function indexAssignments(
  records: KRecord[],
  shiftTypes: Map<string, { id: string } & ShiftTypeData>,
): Map<string, KRecord[]> {
  // Split shifts (e.g. an employee scheduled for both a Morning and
  // an Evening shift on the same date) are valid and the calendar
  // must surface every assignment, not silently keep the last one
  // wins. The map collects an array per (employee, date) cell, then
  // sorts each cell by the resolved shift_type.start_time so the
  // visual stack is stable across renders. Assignments missing a
  // shift_type or start_time fall to the bottom via a sentinel
  // "99:99" sort key — they're rare in practice (foreign-key drop)
  // but shouldn't crash the grid.
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
        (aData.shift_type_id ? shiftTypes.get(aData.shift_type_id)?.start_time : undefined) ??
        "99:99";
      const bStart =
        (bData.shift_type_id ? shiftTypes.get(bData.shift_type_id)?.start_time : undefined) ??
        "99:99";
      return aStart.localeCompare(bStart);
    });
  }
  return out;
}

function cellKey(employeeID: string, date: string): string {
  return `${employeeID}::${date}`;
}

function shortDate(iso: string): string {
  const d = new Date(iso + "T00:00:00");
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function buildDateRange(anchor: string, view: View): string[] {
  const start = new Date(anchor + "T00:00:00");
  const out: string[] = [];
  if (view === "week") {
    const offset = start.getDay();
    start.setDate(start.getDate() - offset);
    for (let i = 0; i < 7; i++) {
      const d = new Date(start);
      d.setDate(start.getDate() + i);
      out.push(toCalendarISO(d));
    }
  } else {
    const first = new Date(start.getFullYear(), start.getMonth(), 1);
    const last = new Date(start.getFullYear(), start.getMonth() + 1, 0);
    const days = last.getDate();
    for (let i = 0; i < days; i++) {
      const d = new Date(first);
      d.setDate(first.getDate() + i);
      out.push(toCalendarISO(d));
    }
  }
  return out;
}

// Shared cell classes for the bespoke employee × date grid. This grid
// has custom column-width / sticky-header needs that the generic
// @kapp/ui Table doesn't model, so it stays a raw <table> — but every
// border/spacing/colour is a design token rather than an inline hex.
const TH =
  "whitespace-nowrap border-b border-border bg-bg-subtle px-2 py-1.5 text-left text-xs font-semibold text-fg";
const TD =
  "min-w-[90px] border-b border-r border-border px-1.5 py-1 align-top";
