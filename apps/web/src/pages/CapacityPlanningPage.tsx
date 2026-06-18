import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { CapacityDayLoad, WorkCenterSchedule } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { AlertTriangle, CalendarRange, Gauge } from "lucide-react";
import { api } from "../lib/api";
import { parseCalendarDate, toCalendarISO } from "../lib/date";
import { useFormatter } from "../lib/i18n";

type Formatters = ReturnType<typeof useFormatter>;

function dayWeekday(fmt: Formatters, value: string): string {
  return fmt.date(parseCalendarDate(value), { weekday: "short" });
}
function dayMonthDay(fmt: Formatters, value: string): string {
  return fmt.date(parseCalendarDate(value), { month: "short", day: "numeric" });
}

interface OverloadSlot {
  centerId: string;
  centerName: string;
  date: string;
  utilization: number;
}

/**
 * CapacityPlanningPage renders the Stream 2 finite-capacity grid. It
 * queries every released / in-progress work order, joins each to its
 * snapshotted routing operations, and shows utilisation (scheduled vs
 * available minutes) per work center per day over the selected window.
 *
 * v1 is a read-only conflict surface: cells where scheduled minutes
 * exceed available capacity are flagged as overloaded (red) but the
 * planner does not auto-reschedule.
 */
export function CapacityPlanningPage() {
  const fmt = useFormatter();
  const today = new Date();
  // Advance by calendar days rather than adding 6*24h of milliseconds:
  // setDate normalises across DST transitions, so the default window is
  // always exactly 7 calendar days. Millisecond arithmetic would land an
  // hour off on a DST boundary and, for a user near midnight, toCalendarISO
  // could then read the previous local day — a 6-day window.
  const weekOut = new Date(today);
  weekOut.setDate(weekOut.getDate() + 6);
  const [start, setStart] = useState(toCalendarISO(today));
  const [end, setEnd] = useState(toCalendarISO(weekOut));

  const planQ = useQuery({
    queryKey: ["mfg", "capacity", start, end],
    queryFn: () => api.capacityPlan({ start, end }),
  });

  const rows = planQ.data?.rows ?? [];

  // The day axis is identical for every row, so read it off the first
  // row (all rows share the planner's dayKeys ordering).
  const dayHeaders: string[] = rows.length > 0 ? rows[0].days.map((d) => d.date) : [];

  const overloads = useMemo<OverloadSlot[]>(() => {
    const out: OverloadSlot[] = [];
    rows.forEach((row) => {
      row.days.forEach((day) => {
        if (day.overloaded) {
          out.push({
            centerId: row.work_center_id,
            centerName: row.work_center_name,
            date: day.date,
            utilization: Number(day.utilization_percent),
          });
        }
      });
    });
    return out.sort((a, b) => b.utilization - a.utilization);
  }, [rows]);

  return (
    <section className="flex flex-col gap-6">
      <header className="flex flex-col gap-1">
        <Eyebrow>Manufacturing</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Capacity planning
        </h1>
        <p className="text-sm text-fg-muted">
          How busy each work center is across the days ahead, based on released
          and in-progress work orders. Anything over 100% is more work than the
          center can finish that day.
        </p>
      </header>

      <div className="flex flex-wrap items-end gap-3">
        <Field label="From">
          <Input
            type="date"
            value={start}
            max={end}
            onChange={(e) => setStart(e.target.value)}
            className="w-auto tabular-nums"
          />
        </Field>
        <Field label="To">
          <Input
            type="date"
            value={end}
            min={start}
            onChange={(e) => setEnd(e.target.value)}
            className="w-auto tabular-nums"
          />
        </Field>
      </div>

      {planQ.isLoading ? (
        <Skeleton className="h-64 w-full" />
      ) : planQ.isError ? (
        <EmptyState
          icon={<AlertTriangle className="size-6" />}
          title="Couldn't load the capacity plan"
          description={(planQ.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void planQ.refetch()}
              disabled={planQ.isFetching}
            >
              Try again
            </Button>
          }
        />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<Gauge className="size-6" />}
          title="Nothing scheduled"
          description="Add work centers and release work orders to see how busy each center will be."
        />
      ) : (
        <div className="flex flex-col gap-4">
          <OverloadSummary overloads={overloads} fmt={fmt} />

          <div className="overflow-x-auto rounded-xl border border-border">
            <Table className="min-w-full">
              <TableHeader>
                <TableRow>
                  <TableHead className="sticky left-0 z-10 bg-bg-subtle">
                    Work center
                  </TableHead>
                  {dayHeaders.map((d) => (
                    <TableHead
                      key={d}
                      className="whitespace-nowrap text-center tabular-nums"
                    >
                      <span className="block text-xs font-normal text-fg-muted">
                        {dayWeekday(fmt, d)}
                      </span>
                      <span className="block">{dayMonthDay(fmt, d)}</span>
                    </TableHead>
                  ))}
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row: WorkCenterSchedule) => (
                  <TableRow key={row.work_center_id}>
                    <TableCell className="sticky left-0 z-10 whitespace-nowrap bg-bg font-medium">
                      {row.work_center_name}
                      {row.status !== "active" && (
                        <Badge variant="warning" size="xs" className="ml-2">
                          {row.status === "maintenance"
                            ? "Maintenance"
                            : "Retired"}
                        </Badge>
                      )}
                    </TableCell>
                    {row.days.map((day: CapacityDayLoad) => (
                      <CapacityCell key={day.date} day={day} fmt={fmt} />
                    ))}
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>

          <Legend />
        </div>
      )}
    </section>
  );
}

function CapacityCell({ day, fmt }: { day: CapacityDayLoad; fmt: Formatters }) {
  const util = Number(day.utilization_percent);
  const scheduled = Number(day.scheduled_minutes);
  const available = Number(day.available_minutes);
  const tone = day.overloaded
    ? "bg-danger/15 font-semibold text-danger"
    : util >= 80
      ? "bg-warning/15 text-warning-fg"
      : util > 0
        ? "bg-success/10"
        : "text-fg-subtle";
  return (
    <TableCell
      title={`Scheduled ${fmt.number(scheduled)} min of ${fmt.number(
        available,
      )} min available`}
      className={`text-center tabular-nums ${tone}`}
    >
      {util > 0 || day.overloaded ? `${fmt.number(util)}%` : "—"}
    </TableCell>
  );
}

function OverloadSummary({
  overloads,
  fmt,
}: {
  overloads: OverloadSlot[];
  fmt: Formatters;
}) {
  if (overloads.length === 0) {
    return (
      <div className="flex items-start gap-3 rounded-xl border border-success/40 bg-success/10 p-4">
        <Gauge className="mt-0.5 size-5 text-success" />
        <div>
          <p className="text-sm font-medium text-fg">Everything fits</p>
          <p className="text-sm text-fg-muted">
            No work center is over capacity in this window.
          </p>
        </div>
      </div>
    );
  }
  const top = overloads.slice(0, 4);
  return (
    <div className="flex flex-col gap-2 rounded-xl border border-danger/40 bg-danger/10 p-4">
      <div className="flex items-start gap-3">
        <AlertTriangle className="mt-0.5 size-5 text-danger" />
        <div>
          <p className="text-sm font-medium text-fg">
            {overloads.length} overloaded{" "}
            {overloads.length === 1 ? "slot" : "slots"}
          </p>
          <p className="text-sm text-fg-muted">
            These centers have more work than they can finish that day. Move
            work to a quieter day, add a shift, or push the due date.
          </p>
        </div>
      </div>
      <ul className="flex flex-col gap-1 pl-8">
        {top.map((o) => (
          <li
            key={`${o.centerId}-${o.date}`}
            className="text-sm text-fg"
          >
            <span className="font-medium">{o.centerName}</span> on{" "}
            {dayMonthDay(fmt, o.date)} —{" "}
            <span className="tabular-nums text-danger">
              {fmt.number(o.utilization)}%
            </span>{" "}
            of capacity
          </li>
        ))}
        {overloads.length > top.length && (
          <li className="text-sm text-fg-muted">
            +{overloads.length - top.length} more
          </li>
        )}
      </ul>
    </div>
  );
}

function Legend() {
  const items: { label: string; className: string }[] = [
    { label: "Idle", className: "bg-bg border border-border" },
    { label: "Busy", className: "bg-success/10" },
    { label: "Nearly full (≥80%)", className: "bg-warning/15" },
    { label: "Overloaded (>100%)", className: "bg-danger/15" },
  ];
  return (
    <div className="flex flex-wrap items-center gap-4 text-xs text-fg-muted">
      <span className="inline-flex items-center gap-1.5">
        <CalendarRange className="size-3.5" />
        Utilisation = scheduled ÷ available minutes
      </span>
      {items.map((it) => (
        <span key={it.label} className="inline-flex items-center gap-1.5">
          <span className={`size-3 rounded-xs ${it.className}`} aria-hidden />
          {it.label}
        </span>
      ))}
    </div>
  );
}
