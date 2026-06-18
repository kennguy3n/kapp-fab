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
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type BadgeProps,
} from "@kapp/ui";
import { AlertTriangle, Factory, Inbox, Plus, ShoppingCart } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { mt, mtp, type MrpStringKey } from "../components/MrpStrings";

type Formatters = ReturnType<typeof useFormatter>;

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

// parseCalendarDate reads the YYYY-MM-DD prefix of an API date/timestamp
// and builds a Date at local midnight, so formatting never drifts a day
// across timezones the way `new Date("2026-01-01")` (parsed as UTC) can.
function parseCalendarDate(value: string): Date {
  const [y, m, d] = value.slice(0, 10).split("-").map(Number);
  return new Date(y, (m || 1) - 1, d || 1);
}

function formatDate(fmt: Formatters, value: string): string {
  return fmt.date(parseCalendarDate(value));
}

function formatHorizon(fmt: Formatters, start: string, end: string): string {
  return `${formatDate(fmt, start)} → ${formatDate(fmt, end)}`;
}

interface DemandDraft {
  item_id: string;
  qty: string;
  due_date: string;
  source: MRPDemandSource;
}

/**
 * MrpPage renders the MRP console. The left column drives a run (horizon,
 * optional reorder-level top-up, and a set of independent demand lines)
 * and lists past runs; the right column drills into the selected run to
 * explain the demand it was computed against and to present the netted
 * planned orders as actionable make / buy suggestion cards with their
 * backward-scheduled release dates.
 */
export function MrpPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();
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
  const runs = runsQ.data ?? [];

  return (
    <section className="grid gap-6 lg:grid-cols-[minmax(0,3fr)_minmax(0,4fr)]">
      <div className="flex min-w-0 flex-col gap-5">
        <header className="flex flex-col gap-1">
          <Eyebrow>{mt("mrp.eyebrow")}</Eyebrow>
          <h1 className="text-2xl font-semibold tracking-tight text-fg">
            {mt("mrp.title")}
          </h1>
          <p className="text-sm text-fg-muted">{mt("mrp.subtitle")}</p>
        </header>

        <RunMrpForm
          items={itemsQ.data ?? []}
          onRan={(run) => {
            qc.invalidateQueries({ queryKey: RUNS_KEY });
            setSelectedRunId(run.id);
          }}
        />

        <div className="flex flex-col gap-3">
          <h2 className="text-sm font-semibold tracking-tight text-fg">
            {mt("mrp.runs.heading")}
          </h2>
          {runsQ.isLoading ? (
            <div className="flex flex-col gap-2">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-12 w-full" />
              <Skeleton className="h-12 w-full" />
            </div>
          ) : runsQ.isError ? (
            <EmptyState
              icon={<AlertTriangle className="size-6" />}
              title={mt("mrp.runs.errorTitle")}
              description={(runsQ.error as Error).message}
              action={
                <Button
                  variant="secondary"
                  onClick={() => void runsQ.refetch()}
                  disabled={runsQ.isFetching}
                >
                  {mt("mrp.runs.retry")}
                </Button>
              }
            />
          ) : runs.length === 0 ? (
            <EmptyState
              icon={<Inbox className="size-6" />}
              title={mt("mrp.runs.emptyTitle")}
              description={mt("mrp.runs.emptyBody")}
            />
          ) : (
            <div className="overflow-x-auto rounded-xl border border-border">
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
                    <TableHead className="text-right">
                      <span className="sr-only">{mt("mrp.runs.view")}</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {runs.map((run: MRPRun) => {
                    const selected = run.id === selectedRunId;
                    return (
                      <TableRow
                        key={run.id}
                        data-selected={selected ? "" : undefined}
                        className={selected ? "bg-accent/10" : undefined}
                      >
                        <TableCell className="font-medium whitespace-nowrap">
                          {formatHorizon(fmt, run.horizon_start, run.horizon_end)}
                          {run.include_min_stock && (
                            <Badge variant="neutral" className="ml-2">
                              {mt("mrp.runs.minStockBadge")}
                            </Badge>
                          )}
                        </TableCell>
                        <TableCell>
                          <RunStatusBadge status={run.status} />
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {fmt.number(run.planned_order_count)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums whitespace-nowrap">
                          {fmt.number(run.make_order_count)} /{" "}
                          {fmt.number(run.buy_order_count)}
                        </TableCell>
                        <TableCell className="text-right">
                          <Button
                            size="sm"
                            variant={selected ? "secondary" : "outline"}
                            onClick={() => setSelectedRunId(run.id)}
                            aria-label={mtp("mrp.detail.viewRun", {
                              date: formatDate(fmt, run.horizon_start),
                            })}
                          >
                            {mt("mrp.runs.view")}
                          </Button>
                        </TableCell>
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
            </div>
          )}
        </div>
      </div>

      <div className="flex min-w-0 flex-col gap-3">
        <h2 className="text-sm font-semibold tracking-tight text-fg">
          {mt("mrp.detail.heading")}
        </h2>
        {selectedRunId ? (
          <RunDetail
            key={selectedRunId}
            runId={selectedRunId}
            labelFor={labelFor}
            fmt={fmt}
          />
        ) : (
          <EmptyState
            icon={<Factory className="size-6" />}
            title={mt("mrp.detail.heading")}
            description={mt("mrp.detail.select")}
          />
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
  // Lazy initialisers: the default horizon is anchored to the mount-time
  // calendar day, not recomputed on every render. `start` is a one-shot
  // seed, not a live clock.
  const [horizonStart, setHorizonStart] = useState(() => today());
  const [horizonEnd, setHorizonEnd] = useState(() => addDays(today(), 30));
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
      className="flex flex-col gap-4 rounded-xl border border-border bg-bg-subtle p-4"
    >
      <h2 className="text-sm font-semibold tracking-tight text-fg">
        {mt("mrp.run.heading")}
      </h2>
      <div className="flex flex-wrap items-end gap-3">
        <Field label={mt("mrp.run.horizonStart")} required>
          <Input
            type="date"
            value={horizonStart}
            onChange={(e) => setHorizonStart(e.target.value)}
            required
          />
        </Field>
        <Field label={mt("mrp.run.horizonEnd")} required>
          <Input
            type="date"
            value={horizonEnd}
            onChange={(e) => setHorizonEnd(e.target.value)}
            required
          />
        </Field>
        <Field
          label={mt("mrp.run.buyLeadTime")}
          help={mt("mrp.run.buyLeadTimeHint")}
        >
          <Input
            type="number"
            min="0"
            step="1"
            value={buyLeadTimeDays}
            onChange={(e) => setBuyLeadTimeDays(e.target.value)}
            placeholder="7"
            className="w-28"
          />
        </Field>
      </div>
      <label className="flex items-center gap-2 text-sm text-fg">
        <input
          type="checkbox"
          className="size-4 rounded-sm accent-(--accent)"
          checked={includeMinStock}
          onChange={(e) => setIncludeMinStock(e.target.checked)}
        />
        {mt("mrp.run.includeMinStock")}
      </label>

      <fieldset className="m-0 flex flex-col gap-2 border-0 p-0">
        <legend className="text-sm font-semibold text-fg">
          {mt("mrp.run.demand")}
        </legend>
        <p className="text-xs text-fg-muted">{mt("mrp.run.demandHint")}</p>
        {demand.map((row, idx) => (
          <div
            key={idx}
            className="flex flex-wrap items-end gap-2 rounded-lg border border-border bg-bg p-2"
          >
            <Field label={mt("mrp.run.item")} hideLabel>
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
            </Field>
            <Field label={mt("mrp.run.qty")} hideLabel>
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
            </Field>
            <Field label={mt("mrp.run.dueDate")} hideLabel>
              <Input
                aria-label={`${mt("mrp.run.dueDate")} ${idx + 1}`}
                type="date"
                value={row.due_date}
                onChange={(e) => patchDemand(idx, { due_date: e.target.value })}
                required
              />
            </Field>
            <Field label={mt("mrp.run.source")} hideLabel>
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
            </Field>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => removeDemand(idx)}
            >
              {mt("mrp.run.removeDemand")}
            </Button>
          </div>
        ))}
        <div>
          <Button
            type="button"
            size="sm"
            variant="outline"
            leadingIcon={<Plus className="size-4" />}
            onClick={addDemand}
          >
            {mt("mrp.run.addDemand")}
          </Button>
        </div>
      </fieldset>

      <Field label={mt("mrp.run.notes")}>
        <Input value={notes} onChange={(e) => setNotes(e.target.value)} />
      </Field>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" disabled={runMut.isPending || !hasDemand}>
          {runMut.isPending ? mt("mrp.run.submitting") : mt("mrp.run.submit")}
        </Button>
        {!hasDemand && (
          <span className="text-xs text-fg-muted">
            {mt("mrp.run.needsDemand")}
          </span>
        )}
        {runMut.isError && (
          <span className="text-sm text-danger">
            {(runMut.error as Error).message}
          </span>
        )}
      </div>
    </form>
  );
}

interface RunDetailProps {
  runId: string;
  labelFor: (id: string) => string;
  fmt: Formatters;
}

function RunDetail({ runId, labelFor, fmt }: RunDetailProps) {
  const runQ = useQuery({
    queryKey: ["mfg", "mrp-runs", runId],
    queryFn: () => api.getMRPRun(runId),
  });

  if (runQ.isLoading)
    return (
      <div className="flex flex-col gap-2">
        <Skeleton className="h-9 w-2/3" />
        <Skeleton className="h-28 w-full" />
        <Skeleton className="h-28 w-full" />
      </div>
    );
  if (runQ.isError)
    return (
      <EmptyState
        icon={<AlertTriangle className="size-6" />}
        title={mt("mrp.detail.errorTitle")}
        description={(runQ.error as Error).message}
        action={
          <Button
            variant="secondary"
            onClick={() => void runQ.refetch()}
            disabled={runQ.isFetching}
          >
            {mt("mrp.runs.retry")}
          </Button>
        }
      />
    );
  const run = runQ.data;
  if (!run) return null;

  const demand = run.demand_lines ?? [];
  const planned = run.planned_orders ?? [];
  const makeOrders = planned.filter((po) => po.order_type === "make");
  const buyOrders = planned.filter((po) => po.order_type === "buy");

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <RunStatusBadge status={run.status} />
        <span className="text-fg-muted">
          {formatHorizon(fmt, run.horizon_start, run.horizon_end)}
        </span>
        {run.notes && <span className="text-fg-muted">· {run.notes}</span>}
      </div>

      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-semibold tracking-tight text-fg">
          {mt("mrp.detail.plannedHeading")}
        </h3>
        {planned.length === 0 ? (
          <EmptyState
            icon={<Inbox className="size-6" />}
            title={mt("mrp.detail.plannedEmpty")}
            description={mt("mrp.detail.explainer")}
          />
        ) : (
          <>
            <p className="text-sm text-fg-muted">{mt("mrp.detail.explainer")}</p>
            {makeOrders.length > 0 && (
              <SuggestionGroup
                icon={<Factory className="size-4" />}
                heading={mt("mrp.detail.makeHeading")}
                count={mtp("mrp.detail.makeCount", { count: makeOrders.length })}
                orders={makeOrders}
                badgeLabel={mt("mrp.orderType.make")}
                badgeVariant="info"
                labelFor={labelFor}
                fmt={fmt}
              />
            )}
            {buyOrders.length > 0 && (
              <SuggestionGroup
                icon={<ShoppingCart className="size-4" />}
                heading={mt("mrp.detail.buyHeading")}
                count={mtp("mrp.detail.buyCount", { count: buyOrders.length })}
                orders={buyOrders}
                badgeLabel={mt("mrp.orderType.buy")}
                badgeVariant="neutral"
                labelFor={labelFor}
                fmt={fmt}
              />
            )}
          </>
        )}
      </section>

      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-semibold tracking-tight text-fg">
          {mt("mrp.detail.demandHeading")}
        </h3>
        {demand.length === 0 ? (
          <p className="text-sm text-fg-muted">{mt("mrp.detail.demandEmpty")}</p>
        ) : (
          <div className="overflow-x-auto rounded-xl border border-border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{mt("mrp.run.item")}</TableHead>
                  <TableHead className="text-right">
                    {mt("mrp.run.qty")}
                  </TableHead>
                  <TableHead>{mt("mrp.run.dueDate")}</TableHead>
                  <TableHead>{mt("mrp.run.source")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {demand.map((line: MRPDemandLine) => (
                  <TableRow key={line.id}>
                    <TableCell>{labelFor(line.item_id)}</TableCell>
                    <TableCell className="text-right tabular-nums">
                      {fmt.number(Number(line.qty))}
                    </TableCell>
                    <TableCell className="whitespace-nowrap">
                      {formatDate(fmt, line.due_date)}
                    </TableCell>
                    <TableCell>
                      {mt(`mrp.source.${line.source}` as MrpStringKey)}
                      {line.source_ref ? ` · ${line.source_ref}` : ""}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </section>
    </div>
  );
}

interface SuggestionGroupProps {
  icon: React.ReactNode;
  heading: string;
  count: string;
  orders: MRPPlannedOrder[];
  badgeLabel: string;
  badgeVariant: BadgeProps["variant"];
  labelFor: (id: string) => string;
  fmt: Formatters;
}

function SuggestionGroup({
  icon,
  heading,
  count,
  orders,
  badgeLabel,
  badgeVariant,
  labelFor,
  fmt,
}: SuggestionGroupProps) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2 text-sm font-medium text-fg">
        <span className="text-fg-muted">{icon}</span>
        {heading}
        <span className="text-fg-subtle">· {count}</span>
      </div>
      <div className="grid gap-2 sm:grid-cols-2">
        {orders.map((po) => (
          <article
            key={po.id}
            className="flex flex-col gap-2 rounded-lg border border-border bg-bg p-3"
          >
            <div className="flex items-start justify-between gap-2">
              <span className="min-w-0 truncate font-medium text-fg">
                {labelFor(po.item_id)}
              </span>
              <Badge variant={badgeVariant}>{badgeLabel}</Badge>
            </div>
            <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
              <dt className="text-fg-muted">{mt("mrp.suggestion.qtyLabel")}</dt>
              <dd className="text-right font-medium tabular-nums text-fg">
                {fmt.number(Number(po.qty))}
              </dd>
              <dt className="text-fg-muted">
                {mt("mrp.suggestion.startByLabel")}
              </dt>
              <dd className="text-right whitespace-nowrap text-fg">
                {formatDate(fmt, po.suggested_start_date)}
              </dd>
              <dt className="text-fg-muted">
                {mt("mrp.suggestion.dueByLabel")}
              </dt>
              <dd className="text-right whitespace-nowrap text-fg">
                {formatDate(fmt, po.due_date)}
              </dd>
              <dt className="text-fg-muted">
                {mt("mrp.suggestion.leadTimeLabel")}
              </dt>
              <dd className="text-right tabular-nums text-fg">
                {mtp("mrp.detail.leadTimeDays", { days: po.lead_time_days })}
              </dd>
              {po.explosion_level > 0 && (
                <>
                  <dt className="text-fg-muted">
                    {mt("mrp.suggestion.levelLabel")}
                  </dt>
                  <dd className="text-right tabular-nums text-fg">
                    {fmt.number(po.explosion_level)}
                  </dd>
                </>
              )}
            </dl>
          </article>
        ))}
      </div>
    </div>
  );
}

export default MrpPage;
