import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

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
  const [anchor, setAnchor] = useState(() => isoDate(new Date()));

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
    <section>
      <h1>Shift Schedule</h1>
      <p style={{ color: "#6b7280" }}>
        Phase M shift calendar. Rows are employees, columns are dates,
        cells render any matching hr.shift_assignment for that
        (employee, date) tuple. Click an empty cell to schedule.
      </p>
      <header style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 12 }}>
        <button onClick={() => setView("week")} disabled={view === "week"}>
          Week
        </button>
        <button onClick={() => setView("month")} disabled={view === "month"}>
          Month
        </button>
        <input
          type="date"
          value={anchor}
          onChange={(e) => setAnchor(e.target.value)}
          style={{ marginLeft: 12 }}
        />
        <span style={{ color: "#6b7280", marginLeft: 12 }}>
          {dates[0]} → {dates[dates.length - 1]}
        </span>
      </header>
      <ScheduleForm shiftTypes={Array.from(shiftTypes.values())} employees={employees} />
      {(employeesQ.isLoading || shiftTypesQ.isLoading || assignmentsQ.isLoading) && (
        <p>Loading…</p>
      )}
      {employees.length === 0 ? (
        <p style={{ color: "#6b7280" }}>No employees yet.</p>
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
    <div style={{ overflowX: "auto" }}>
      <table style={{ borderCollapse: "collapse", minWidth: "100%" }}>
        <thead>
          <tr>
            <th style={th()}>Employee</th>
            {dates.map((d) => (
              <th key={d} style={th()}>
                {shortDate(d)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {employees.map((e) => (
            <tr key={e.id}>
              <td style={td()}>{e.name ?? "(unnamed)"}</td>
              {dates.map((d) => {
                const key = cellKey(e.id, d);
                const recs = assignmentsByCell.get(key) ?? [];
                if (recs.length === 0) return <td key={key} style={tdEmpty()} />;
                return (
                  <td key={key} style={tdStacked()}>
                    <div style={badgeStack()}>
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
                            color={st?.color ?? "#dbeafe"}
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
  color: string;
  status: string;
}) {
  return (
    <div
      style={{
        background: color,
        padding: "4px 6px",
        borderRadius: 4,
        fontSize: 12,
        lineHeight: 1.3,
      }}
    >
      <div style={{ fontWeight: 600 }}>{label}</div>
      {time && <div style={{ color: "#374151" }}>{time}</div>}
      {status !== "scheduled" && (
        <div style={{ color: "#6b7280", fontSize: 10 }}>{status}</div>
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
  const [shiftDate, setShiftDate] = useState(() => isoDate(new Date()));
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
      style={{ display: "flex", gap: 8, marginBottom: 12, flexWrap: "wrap" }}
    >
      <select value={employeeId} onChange={(e) => setEmployeeId(e.target.value)}>
        <option value="">Select employee…</option>
        {employees.map((e) => (
          <option key={e.id} value={e.id}>
            {e.name ?? e.id}
          </option>
        ))}
      </select>
      <select value={shiftTypeId} onChange={(e) => setShiftTypeId(e.target.value)}>
        <option value="">Select shift type…</option>
        {shiftTypes.map((s) => (
          <option key={s.id} value={s.id}>
            {s.name ?? s.id}
          </option>
        ))}
      </select>
      <input
        type="date"
        value={shiftDate}
        onChange={(e) => setShiftDate(e.target.value)}
      />
      <button type="submit" disabled={create.isPending}>
        {create.isPending ? "Scheduling…" : "Schedule"}
      </button>
      {error && <span style={{ color: "#b91c1c" }}>{error}</span>}
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

function isoDate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
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

function th(): React.CSSProperties {
  return {
    textAlign: "left",
    borderBottom: "1px solid #d1d5db",
    padding: "6px 8px",
    background: "#f9fafb",
    fontSize: 12,
    fontWeight: 600,
    whiteSpace: "nowrap",
  };
}

function td(): React.CSSProperties {
  return {
    borderBottom: "1px solid #e5e7eb",
    borderRight: "1px solid #f3f4f6",
    padding: "4px 6px",
    verticalAlign: "top",
    minWidth: 90,
  };
}

function tdEmpty(): React.CSSProperties {
  return {
    ...td(),
    background: "#fafafa",
  };
}

function tdStacked(): React.CSSProperties {
  // Keep the <td> as the default `display: table-cell` so it
  // participates in the table's column-width and row-height
  // layout the same way `tdEmpty()` siblings do — putting
  // `display: flex` on the cell itself would break alignment in
  // any row that mixes filled and empty cells, which is the
  // common case once any employee has a partial schedule. The
  // flex stack lives one DOM level deeper via `badgeStack()`
  // below.
  return { ...td() };
}

function badgeStack(): React.CSSProperties {
  return {
    display: "flex",
    flexDirection: "column",
    gap: 4,
  };
}
