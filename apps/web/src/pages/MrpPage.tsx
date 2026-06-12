import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  InventoryItem,
  MRPDemandLine,
  MRPDemandLineInput,
  MRPDemandSource,
  MRPPlannedOrder,
  MRPRun,
  RunMRPInput,
} from "@kapp/client";
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
import { mt, mtp, type MrpStringKey } from "../components/MrpStrings";

const RUNS_KEY = ["mfg", "mrp-runs"] as const;

const DEMAND_SOURCES: MRPDemandSource[] = [
  "sales_order",
  "work_order",
  "manual",
];

// today returns today's date as a YYYY-MM-DD string for the date
// inputs' default values, computed in the browser's local timezone so
// the planner's horizon lines up with the operator's calendar.
// `new Date().toISOString().slice(0, 10)` is off-by-one for UTC+ zones
// because it formats the UTC instant, not the local calendar day.
function today(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

// addDays returns the ISO date `days` after `iso` (YYYY-MM-DD). Used to
// seed a sensible default horizon end so the form is runnable without
// the operator hand-typing two dates.
function addDays(iso: string, days: number): string {
  const d = new Date(`${iso}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() + days);
  return d.toISOString().slice(0, 10);
}

interface DemandDraft {
  item_id: string;
  qty: string;
  due_date: string;
  source: MRPDemandSource;
}

/**
 * MrpPage renders the Batch-3 MRP console. The left column drives a run
 * (horizon, optional reorder-level top-up, and a set of independent
 * demand lines) and lists past runs; the right column drills into the
 * selected run to show the demand snapshot it was computed against and
 * the netted planned orders (make vs buy) with their backward-scheduled
 * release dates.
 */
export function MrpPage() {
  const qc = useQueryClient();
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null);

  const runsQ = useQuery({
    queryKey: RUNS_KEY,
    queryFn: () => api.listMRPRuns(),
  });
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it: InventoryItem) =>
      m.set(it.id, `${it.sku} — ${it.name}`),
    );
    return m;
  }, [itemsQ.data]);

  const labelFor = (id: string) => itemLabel.get(id) ?? id;

  return (
    <section className="grid grid-cols-[3fr_4fr] gap-6">
      <div className="min-w-0">
        <h1>{mt("mrp.title")}</h1>
        <p className="text-fg-muted">{mt("mrp.subtitle")}</p>

        <RunMrpForm
          items={itemsQ.data ?? []}
          onRan={(run) => {
            qc.invalidateQueries({ queryKey: RUNS_KEY });
            setSelectedRunId(run.id);
          }}
        />

        <h2 className="mt-6">{mt("mrp.runs.heading")}</h2>
        {runsQ.isLoading && <p>{mt("mrp.runs.loading")}</p>}
        {runsQ.isError && (
          <p className="text-danger">
            {mt("mrp.runs.error")} {String(runsQ.error)}
          </p>
        )}
        {runsQ.data && runsQ.data.length === 0 && (
          <p className="text-fg-muted">{mt("mrp.runs.empty")}</p>
        )}
        {runsQ.data && runsQ.data.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{mt("mrp.runs.horizon")}</TableHead>
                <TableHead>{mt("mrp.runs.status")}</TableHead>
                <TableHead className="text-right">
                  {mt("mrp.runs.plannedOrders")}
                </TableHead>
                <TableHead className="text-right">
                  {mt("mrp.runs.makeBuy")}
                </TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {runsQ.data.map((run: MRPRun) => (
                <TableRow
                  key={run.id}
                  data-selected={run.id === selectedRunId ? "" : undefined}
                  className={
                    run.id === selectedRunId ? "bg-accent/10" : undefined
                  }
                >
                  <TableCell className="whitespace-nowrap">
                    {run.horizon_start.slice(0, 10)} →{" "}
                    {run.horizon_end.slice(0, 10)}
                    {run.include_min_stock && (
                      <Badge variant="default" className="ml-1">
                        {mt("mrp.runs.minStockBadge")}
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell>
                    <RunStatusBadge status={run.status} />
                  </TableCell>
                  <TableCell className="text-right">
                    {run.planned_order_count}
                  </TableCell>
                  <TableCell className="text-right whitespace-nowrap">
                    {run.make_order_count} / {run.buy_order_count}
                  </TableCell>
                  <TableCell>
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setSelectedRunId(run.id)}
                      aria-label={`${mt("mrp.runs.view")} ${run.horizon_start.slice(0, 10)}`}
                    >
                      {mt("mrp.runs.view")}
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <div className="min-w-0">
        <h2>{mt("mrp.detail.heading")}</h2>
        {selectedRunId ? (
          <RunDetail key={selectedRunId} runId={selectedRunId} labelFor={labelFor} />
        ) : (
          <p className="text-fg-muted">{mt("mrp.detail.select")}</p>
        )}
      </div>
    </section>
  );
}

function RunStatusBadge({ status }: { status: MRPRun["status"] }) {
  const key: MrpStringKey =
    status === "completed" ? "mrp.status.completed" : "mrp.status.failed";
  return (
    <Badge variant={status === "completed" ? "success" : "danger"}>
      {mt(key)}
    </Badge>
  );
}

interface RunMrpFormProps {
  items: InventoryItem[];
  onRan: (run: MRPRun) => void;
}

function RunMrpForm({ items, onRan }: RunMrpFormProps) {
  const start = today();
  const [horizonStart, setHorizonStart] = useState(start);
  const [horizonEnd, setHorizonEnd] = useState(addDays(start, 30));
  const [includeMinStock, setIncludeMinStock] = useState(false);
  const [buyLeadTimeDays, setBuyLeadTimeDays] = useState("");
  const [notes, setNotes] = useState("");
  const [demand, setDemand] = useState<DemandDraft[]>([]);

  const runMut = useMutation({
    mutationFn: (input: RunMRPInput) => api.runMRP(input),
    onSuccess: (run) => {
      onRan(run);
      setDemand([]);
      setNotes("");
    },
  });

  // Only rows with an item picked count as demand; a half-filled draft
  // row must not arm the submit. A run needs at least one such line OR a
  // reorder top-up, mirroring the backend's ErrMRPNoDemand guard so we
  // never POST a no-op (or partially-empty) run.
  const validDemand = demand.filter((row) => row.item_id !== "");
  const hasDemand = validDemand.length > 0 || includeMinStock;

  const addDemand = () =>
    setDemand((d) => [
      ...d,
      { item_id: "", qty: "1", due_date: horizonEnd, source: "manual" },
    ]);
  const removeDemand = (idx: number) =>
    setDemand((d) => d.filter((_, i) => i !== idx));
  const patchDemand = (idx: number, patch: Partial<DemandDraft>) =>
    setDemand((d) => d.map((row, i) => (i === idx ? { ...row, ...patch } : row)));

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!hasDemand) return;
    const lead = buyLeadTimeDays.trim();
    const input: RunMRPInput = {
      horizon_start: horizonStart,
      horizon_end: horizonEnd,
      include_min_stock: includeMinStock,
      notes: notes.trim() || undefined,
      ...(lead ? { buy_lead_time_days: Number(lead) } : {}),
      demand: validDemand.map<MRPDemandLineInput>((row) => ({
        item_id: row.item_id,
        qty: row.qty,
        due_date: row.due_date,
        source: row.source,
      })),
    };
    runMut.mutate(input);
  };

  return (
    <form
      onSubmit={submit}
      className="mt-4 flex flex-col gap-3 rounded-md bg-bg-subtle p-3"
    >
      <h2 className="m-0">{mt("mrp.run.heading")}</h2>
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-[13px]">
          {mt("mrp.run.horizonStart")}
          <Input
            type="date"
            value={horizonStart}
            onChange={(e) => setHorizonStart(e.target.value)}
            required
          />
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          {mt("mrp.run.horizonEnd")}
          <Input
            type="date"
            value={horizonEnd}
            onChange={(e) => setHorizonEnd(e.target.value)}
            required
          />
        </label>
        <label className="flex flex-col gap-1 text-[13px]">
          {mt("mrp.run.buyLeadTime")}
          <Input
            type="number"
            min="0"
            step="1"
            value={buyLeadTimeDays}
            onChange={(e) => setBuyLeadTimeDays(e.target.value)}
            placeholder="7"
            className="w-24"
          />
        </label>
      </div>
      <label className="flex items-center gap-2 text-[13px]">
        <input
          type="checkbox"
          checked={includeMinStock}
          onChange={(e) => setIncludeMinStock(e.target.checked)}
        />
        {mt("mrp.run.includeMinStock")}
      </label>

      <fieldset className="m-0 border-0 p-0">
        <legend className="text-[13px] font-semibold">
          {mt("mrp.run.demand")}
        </legend>
        <p className="text-xs text-fg-muted">{mt("mrp.run.demandHint")}</p>
        {demand.map((row, idx) => (
          <div key={idx} className="mt-2 flex flex-wrap items-end gap-2">
            <label className="flex flex-col gap-1 text-[13px]">
              {mt("mrp.run.item")}
              <Select
                aria-label={`${mt("mrp.run.item")} ${idx + 1}`}
                value={row.item_id}
                onChange={(e) => patchDemand(idx, { item_id: e.target.value })}
                required
              >
                <option value="">{mt("mrp.run.selectItem")}</option>
                {items.map((it) => (
                  <option key={it.id} value={it.id}>
                    {it.sku} — {it.name}
                  </option>
                ))}
              </Select>
            </label>
            <label className="flex flex-col gap-1 text-[13px]">
              {mt("mrp.run.qty")}
              <Input
                aria-label={`${mt("mrp.run.qty")} ${idx + 1}`}
                type="number"
                step="0.01"
                min="0"
                value={row.qty}
                onChange={(e) => patchDemand(idx, { qty: e.target.value })}
                required
                className="w-24"
              />
            </label>
            <label className="flex flex-col gap-1 text-[13px]">
              {mt("mrp.run.dueDate")}
              <Input
                aria-label={`${mt("mrp.run.dueDate")} ${idx + 1}`}
                type="date"
                value={row.due_date}
                onChange={(e) => patchDemand(idx, { due_date: e.target.value })}
                required
              />
            </label>
            <label className="flex flex-col gap-1 text-[13px]">
              {mt("mrp.run.source")}
              <Select
                aria-label={`${mt("mrp.run.source")} ${idx + 1}`}
                value={row.source}
                onChange={(e) =>
                  patchDemand(idx, {
                    source: e.target.value as MRPDemandSource,
                  })
                }
              >
                {DEMAND_SOURCES.map((s) => (
                  <option key={s} value={s}>
                    {mt(`mrp.source.${s}` as MrpStringKey)}
                  </option>
                ))}
              </Select>
            </label>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={() => removeDemand(idx)}
            >
              {mt("mrp.run.removeDemand")}
            </Button>
          </div>
        ))}
        <Button
          type="button"
          size="sm"
          variant="outline"
          className="mt-2"
          onClick={addDemand}
        >
          {mt("mrp.run.addDemand")}
        </Button>
      </fieldset>

      <label className="flex flex-col gap-1 text-[13px]">
        {mt("mrp.run.notes")}
        <Input value={notes} onChange={(e) => setNotes(e.target.value)} />
      </label>

      <div className="flex items-center gap-2">
        <Button type="submit" disabled={runMut.isPending || !hasDemand}>
          {runMut.isPending ? mt("mrp.run.submitting") : mt("mrp.run.submit")}
        </Button>
        {!hasDemand && (
          <span className="text-xs text-fg-muted">
            {mt("mrp.run.needsDemand")}
          </span>
        )}
        {runMut.isError && (
          <span className="text-danger">{String(runMut.error)}</span>
        )}
      </div>
    </form>
  );
}

interface RunDetailProps {
  runId: string;
  labelFor: (id: string) => string;
}

function RunDetail({ runId, labelFor }: RunDetailProps) {
  const runQ = useQuery({
    queryKey: ["mfg", "mrp-runs", runId],
    queryFn: () => api.getMRPRun(runId),
  });

  if (runQ.isLoading) return <p>{mt("mrp.detail.loading")}</p>;
  if (runQ.isError)
    return (
      <p className="text-danger">
        {mt("mrp.detail.error")} {String(runQ.error)}
      </p>
    );
  const run = runQ.data;
  if (!run) return null;

  const demand = run.demand_lines ?? [];
  const planned = run.planned_orders ?? [];

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-2 text-[13px]">
        <RunStatusBadge status={run.status} />
        <span className="text-fg-muted">
          {run.horizon_start.slice(0, 10)} → {run.horizon_end.slice(0, 10)}
        </span>
        {run.notes && <span className="text-fg-muted">· {run.notes}</span>}
      </div>

      <div>
        <h3 className="m-0">{mt("mrp.detail.demandHeading")}</h3>
        {demand.length === 0 ? (
          <p className="text-fg-muted">{mt("mrp.detail.demandEmpty")}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{mt("mrp.run.item")}</TableHead>
                <TableHead className="text-right">{mt("mrp.run.qty")}</TableHead>
                <TableHead>{mt("mrp.run.dueDate")}</TableHead>
                <TableHead>{mt("mrp.run.source")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {demand.map((line: MRPDemandLine) => (
                <TableRow key={line.id}>
                  <TableCell>{labelFor(line.item_id)}</TableCell>
                  <TableCell className="text-right">{line.qty}</TableCell>
                  <TableCell>{line.due_date.slice(0, 10)}</TableCell>
                  <TableCell>
                    {mt(`mrp.source.${line.source}` as MrpStringKey)}
                    {line.source_ref ? ` · ${line.source_ref}` : ""}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>

      <div>
        <h3 className="m-0">{mt("mrp.detail.plannedHeading")}</h3>
        {planned.length === 0 ? (
          <p className="text-fg-muted">{mt("mrp.detail.plannedEmpty")}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{mt("mrp.run.item")}</TableHead>
                <TableHead>{mt("mrp.detail.orderType")}</TableHead>
                <TableHead className="text-right">{mt("mrp.run.qty")}</TableHead>
                <TableHead>{mt("mrp.detail.suggestedStart")}</TableHead>
                <TableHead>{mt("mrp.detail.dueDate")}</TableHead>
                <TableHead className="text-right">
                  {mt("mrp.detail.level")}
                </TableHead>
                <TableHead className="text-right">
                  {mt("mrp.detail.leadTime")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {planned.map((po: MRPPlannedOrder) => (
                <TableRow key={po.id}>
                  <TableCell>{labelFor(po.item_id)}</TableCell>
                  <TableCell>
                    <Badge
                      variant={po.order_type === "make" ? "info" : "default"}
                    >
                      {mt(
                        po.order_type === "make"
                          ? "mrp.orderType.make"
                          : "mrp.orderType.buy",
                      )}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-right">{po.qty}</TableCell>
                  <TableCell>{po.suggested_start_date.slice(0, 10)}</TableCell>
                  <TableCell>{po.due_date.slice(0, 10)}</TableCell>
                  <TableCell className="text-right">
                    {po.explosion_level}
                  </TableCell>
                  <TableCell className="text-right">
                    {mtp("mrp.detail.leadTimeDays", { days: po.lead_time_days })}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </div>
    </div>
  );
}

export default MrpPage;
