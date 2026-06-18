import { useMemo, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import type {
  Budget,
  BudgetLine,
  BudgetVarianceReport,
  BudgetVarianceRow,
  CreateBudgetInput,
  FinanceAccount,
  KRecord,
} from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
  Field,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { humanizeToken } from "../lib/ktypeView";
import { useMoney, type Money } from "../lib/finance/format";
import { FinanceError, TableSkeleton } from "../lib/finance/presentation";

/**
 * BudgetPage surfaces the budget module to the finance UI:
 *
 *   1. A list of budgets with status badges and a create form.
 *   2. The selected budget's monthly line editor, where account and
 *      cost centre are chosen from the chart of accounts / cost-centre
 *      tree (never free-typed codes) and the annual total updates as
 *      the user types.
 *   3. The variance dashboard comparing planned vs. actual with a
 *      per-row drill-down into the underlying journal entries.
 *
 * Styling is design-system only (@kapp/ui primitives + semantic
 * tokens). The only inline style is the data-driven variance bar
 * width, which references token-backed colour classes.
 */
const MONTH_LABELS = [
  "Jan",
  "Feb",
  "Mar",
  "Apr",
  "May",
  "Jun",
  "Jul",
  "Aug",
  "Sep",
  "Oct",
  "Nov",
  "Dec",
];

const COST_CENTER_KTYPE = "finance.cost_center";

type LineDraft = {
  id?: string;
  account_code: string;
  cost_center: string;
  months: string[];
};

const emptyDraft = (): LineDraft => ({
  account_code: "",
  cost_center: "",
  months: Array(12).fill("0"),
});

// normalizeMoneyInput coerces a raw user-typed decimal string to a
// wire-safe value. `<input type="number">` produces "" when the user
// clears the field; shopspring's decimal.Decimal cannot unmarshal an
// empty string, so cleared months ship as "0".
const normalizeMoneyInput = (raw: string): string => {
  const trimmed = raw.trim();
  return trimmed === "" ? "0" : trimmed;
};

// normalizeOptionalDecimal collapses an empty optional decimal string
// to `undefined` so the request body omits the field rather than
// shipping "".
const normalizeOptionalDecimal = (
  raw: string | undefined,
): string | undefined => {
  if (raw === undefined) return undefined;
  const trimmed = raw.trim();
  return trimmed === "" ? undefined : trimmed;
};

const STATUS_VARIANT: Record<Budget["status"], "neutral" | "success" | "warning"> = {
  draft: "warning",
  active: "success",
  closed: "neutral",
};

function StatusBadge({ status }: { status: Budget["status"] }) {
  return <Badge variant={STATUS_VARIANT[status]}>{humanizeToken(status)}</Badge>;
}

function recordCode(r: KRecord): string {
  return typeof r.data.code === "string" ? r.data.code : "";
}

function recordName(r: KRecord): string {
  return typeof r.data.name === "string" ? r.data.name : "";
}

// formatPeriod turns a machine "YYYY-MM" period into "Jan 2026".
function formatPeriod(period: string): string {
  const m = /^(\d{4})-(\d{2})$/.exec(period);
  if (!m) return period;
  const month = Number(m[2]);
  if (month < 1 || month > 12) return period;
  return `${MONTH_LABELS[month - 1]} ${m[1]}`;
}

export function BudgetPage() {
  const qc = useQueryClient();
  const money = useMoney();
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [newBudget, setNewBudget] = useState<CreateBudgetInput>({
    name: "",
    fiscal_year: new Date().getUTCFullYear(),
    status: "draft",
  });
  const [draft, setDraft] = useState<LineDraft>(emptyDraft());

  const budgetsQ = useQuery<Budget[]>({
    queryKey: ["budgets"],
    queryFn: () => api.listBudgets(),
  });
  const accountsQ = useQuery<FinanceAccount[]>({
    queryKey: ["finance", "accounts"],
    queryFn: () => api.listAccounts(),
  });
  const costCentersQ = useQuery<KRecord[]>({
    queryKey: ["records", COST_CENTER_KTYPE],
    queryFn: () => api.listRecords(COST_CENTER_KTYPE),
  });

  const budgets = budgetsQ.data ?? [];
  const selectedBudget =
    selectedId !== null
      ? budgets.find((b) => b.id === selectedId) ?? null
      : null;

  const accountName = useMemo(() => {
    const map = new Map<string, string>();
    for (const a of accountsQ.data ?? []) map.set(a.code, a.name);
    return map;
  }, [accountsQ.data]);
  const costCenterName = useMemo(() => {
    const map = new Map<string, string>();
    for (const r of costCentersQ.data ?? []) {
      const code = recordCode(r);
      if (code) map.set(code, recordName(r));
    }
    return map;
  }, [costCentersQ.data]);

  const accountLabel = (code: string) => {
    const name = accountName.get(code);
    return name ? `${code} — ${name}` : code;
  };
  const costCenterLabel = (code: string | undefined) => {
    if (!code) return "—";
    const name = costCenterName.get(code);
    return name ? `${code} — ${name}` : code;
  };

  const linesQ = useQuery<BudgetLine[]>({
    queryKey: ["budget-lines", selectedId],
    queryFn: () => api.listBudgetLines(selectedId as string),
    enabled: !!selectedId,
  });

  const varianceQ = useQuery<BudgetVarianceReport>({
    queryKey: ["budget-variance", selectedId],
    queryFn: () => api.budgetVariance(selectedId as string),
    enabled: !!selectedId,
  });

  const createBudget = useMutation({
    mutationFn: (input: CreateBudgetInput) =>
      api.createBudget({
        ...input,
        variance_threshold: normalizeOptionalDecimal(input.variance_threshold),
      }),
    onSuccess: (b) => {
      setCreating(false);
      setNewBudget({
        name: "",
        fiscal_year: new Date().getUTCFullYear(),
        status: "draft",
      });
      if (b) setSelectedId(b.id);
      qc.invalidateQueries({ queryKey: ["budgets"] });
      toast.success("Budget created");
    },
    onError: (err) =>
      toast.error("Couldn't create budget", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const upsertLine = useMutation({
    mutationFn: (line: LineDraft) =>
      api.upsertBudgetLine(selectedId as string, {
        id: line.id,
        account_code: line.account_code,
        cost_center: line.cost_center || undefined,
        months: line.months.map(normalizeMoneyInput),
      }),
    onSuccess: () => {
      setDraft(emptyDraft());
      qc.invalidateQueries({ queryKey: ["budget-lines", selectedId] });
      qc.invalidateQueries({ queryKey: ["budget-variance", selectedId] });
      toast.success("Budget line saved");
    },
    onError: (err) =>
      toast.error("Couldn't save line", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const deleteLine = useMutation({
    mutationFn: (lineId: string) =>
      api.deleteBudgetLine(selectedId as string, lineId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["budget-lines", selectedId] });
      qc.invalidateQueries({ queryKey: ["budget-variance", selectedId] });
    },
    onError: (err) =>
      toast.error("Couldn't delete line", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const draftTotalNum = useMemo(
    () => draft.months.reduce((sum, m) => sum + (Number(m) || 0), 0),
    [draft.months],
  );

  const accountOptions = accountsQ.data ?? [];
  const costCenterOptions = costCentersQ.data ?? [];

  const submitBudget = (e: FormEvent) => {
    e.preventDefault();
    if (!newBudget.name.trim()) return;
    createBudget.mutate(newBudget);
  };

  const submitLine = (e: FormEvent) => {
    e.preventDefault();
    if (!draft.account_code) return;
    upsertLine.mutate(draft);
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-1">
        <Eyebrow>Finance</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Budgets
        </h1>
        <p className="text-sm text-fg-muted">
          Plan spending and income by month, then track how each account is
          doing against plan. Alerts fire when a line drifts past its
          threshold.
        </p>
      </header>

      <div className="flex flex-col gap-6 lg:flex-row lg:items-start">
        {/* ---------- Budget list + create form ----------- */}
        <div className="flex w-full flex-col gap-3 lg:max-w-xs">
          <div className="flex items-center justify-between">
            <h2 className="text-sm font-semibold text-fg">Your budgets</h2>
            <Button
              type="button"
              variant={creating ? "ghost" : "outline"}
              size="sm"
              leadingIcon={creating ? undefined : <Plus aria-hidden />}
              onClick={() => setCreating((c) => !c)}
            >
              {creating ? "Cancel" : "New budget"}
            </Button>
          </div>

          {creating && (
            <form
              onSubmit={submitBudget}
              className="flex flex-col gap-3 rounded-lg border border-border bg-bg-subtle p-4"
              noValidate
            >
              <Field label="Name" required>
                <Input
                  value={newBudget.name}
                  onChange={(e) =>
                    setNewBudget({ ...newBudget, name: e.target.value })
                  }
                  placeholder="Marketing FY26"
                  required
                />
              </Field>
              <Field label="Fiscal year" required>
                <Input
                  type="number"
                  value={newBudget.fiscal_year}
                  onChange={(e) =>
                    setNewBudget({
                      ...newBudget,
                      fiscal_year: Number(e.target.value),
                    })
                  }
                  required
                />
              </Field>
              <Field label="Status">
                <Select
                  value={newBudget.status ?? "draft"}
                  onChange={(e) =>
                    setNewBudget({
                      ...newBudget,
                      status: e.target.value as Budget["status"],
                    })
                  }
                >
                  <option value="draft">Draft</option>
                  <option value="active">Active</option>
                  <option value="closed">Closed</option>
                </Select>
              </Field>
              <Field
                label="Default cost centre"
                help="Optional — applied to lines that don't set their own."
              >
                <Select
                  value={newBudget.cost_center ?? ""}
                  onChange={(e) =>
                    setNewBudget({ ...newBudget, cost_center: e.target.value })
                  }
                >
                  <option value="">None</option>
                  {costCenterOptions.map((r) => {
                    const code = recordCode(r);
                    return (
                      <option key={r.id} value={code}>
                        {code} — {recordName(r)}
                      </option>
                    );
                  })}
                </Select>
              </Field>
              <Field
                label="Variance threshold"
                help="Alert when actual drifts past this share of plan, e.g. 0.10 = 10%."
              >
                <Input
                  type="number"
                  step="0.001"
                  value={newBudget.variance_threshold ?? ""}
                  onChange={(e) =>
                    setNewBudget({
                      ...newBudget,
                      variance_threshold: e.target.value,
                    })
                  }
                  placeholder="0.10"
                />
              </Field>
              <Button type="submit" disabled={createBudget.isPending}>
                {createBudget.isPending ? "Saving…" : "Create budget"}
              </Button>
            </form>
          )}

          {budgetsQ.isLoading && <TableSkeleton rows={4} columns={1} />}
          {budgetsQ.isError && (
            <FinanceError
              title="Couldn't load budgets"
              error={budgetsQ.error}
              onRetry={() => void budgetsQ.refetch()}
            />
          )}
          {budgetsQ.data && budgets.length === 0 && (
            <div className="rounded-lg border border-border p-6">
              <p className="text-center text-sm text-fg-muted">
                No budgets yet. Create one to start planning.
              </p>
            </div>
          )}
          {budgets.length > 0 && (
            <ul className="flex list-none flex-col gap-1.5 p-0">
              {budgets.map((b) => {
                const isSelected = b.id === selectedId;
                return (
                  <li key={b.id}>
                    <button
                      type="button"
                      onClick={() => setSelectedId(b.id)}
                      aria-pressed={isSelected}
                      className={`w-full rounded-lg border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) ${
                        isSelected
                          ? "border-accent bg-accent/10"
                          : "border-border bg-bg-elevated hover:bg-bg-muted"
                      }`}
                    >
                      <div className="flex items-center justify-between gap-2">
                        <span className="truncate font-medium text-fg">
                          {b.name}
                        </span>
                        <StatusBadge status={b.status} />
                      </div>
                      <div className="mt-0.5 text-xs text-fg-muted">
                        FY{b.fiscal_year}
                        {b.cost_center
                          ? ` · ${costCenterLabel(b.cost_center)}`
                          : ""}
                      </div>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>

        {/* ---------- Selected budget: lines + variance ----------- */}
        <div className="min-w-0 flex-1">
          {!selectedBudget && (
            <div className="rounded-lg border border-dashed border-border p-10">
              <p className="text-center text-sm text-fg-muted">
                Select a budget to edit its monthly lines and see how it's
                tracking against actuals.
              </p>
            </div>
          )}

          {selectedBudget && (
            <div className="flex flex-col gap-6">
              <div className="flex flex-col gap-3">
                <h2 className="text-base font-semibold text-fg">
                  {selectedBudget.name} — lines
                </h2>

                {/* Line editor */}
                <form
                  onSubmit={submitLine}
                  className="flex flex-col gap-3 rounded-lg border border-border bg-bg-subtle p-4"
                  noValidate
                >
                  <div className="grid gap-3 sm:grid-cols-2">
                    <Field label="Account" required>
                      <Select
                        value={draft.account_code}
                        onChange={(e) =>
                          setDraft({ ...draft, account_code: e.target.value })
                        }
                        required
                      >
                        <option value="">Choose an account…</option>
                        {accountOptions.map((a) => (
                          <option key={a.code} value={a.code}>
                            {a.code} — {a.name}
                          </option>
                        ))}
                      </Select>
                    </Field>
                    <Field label="Cost centre" help="Optional.">
                      <Select
                        value={draft.cost_center}
                        onChange={(e) =>
                          setDraft({ ...draft, cost_center: e.target.value })
                        }
                      >
                        <option value="">None</option>
                        {costCenterOptions.map((r) => {
                          const code = recordCode(r);
                          return (
                            <option key={r.id} value={code}>
                              {code} — {recordName(r)}
                            </option>
                          );
                        })}
                      </Select>
                    </Field>
                  </div>
                  <fieldset className="flex flex-col gap-1.5">
                    <legend className="text-xs font-medium text-fg-muted">
                      Monthly amounts
                    </legend>
                    <div className="grid grid-cols-3 gap-2 sm:grid-cols-6">
                      {MONTH_LABELS.map((label, idx) => (
                        <label key={label} className="flex flex-col gap-1">
                          <span className="text-[11px] text-fg-muted">
                            {label}
                          </span>
                          <Input
                            type="number"
                            step="0.01"
                            size="sm"
                            aria-label={`${label} amount`}
                            className="text-right tabular-nums"
                            value={draft.months[idx]}
                            onChange={(e) => {
                              const next = [...draft.months];
                              next[idx] = e.target.value;
                              setDraft({ ...draft, months: next });
                            }}
                          />
                        </label>
                      ))}
                    </div>
                  </fieldset>
                  <div className="flex flex-wrap items-center justify-between gap-3">
                    <span className="text-sm text-fg-muted">
                      Annual total:{" "}
                      <strong className="font-tabular text-fg">
                        {money(draftTotalNum)}
                      </strong>
                    </span>
                    <Button
                      type="submit"
                      disabled={upsertLine.isPending || !draft.account_code}
                    >
                      {upsertLine.isPending ? "Saving…" : "Save line"}
                    </Button>
                  </div>
                </form>
              </div>

              {linesQ.isLoading && <TableSkeleton columns={5} />}
              {linesQ.isError && (
                <FinanceError
                  title="Couldn't load budget lines"
                  error={linesQ.error}
                  onRetry={() => void linesQ.refetch()}
                />
              )}
              {linesQ.data && linesQ.data.length === 0 && (
                <div className="rounded-lg border border-border p-8">
                  <p className="text-center text-sm text-fg-muted">
                    No lines yet. Add the first account above to start
                    planning this budget.
                  </p>
                </div>
              )}
              {linesQ.data && linesQ.data.length > 0 && (
                <div className="overflow-x-auto">
                  <Table className="text-xs">
                    <TableHeader>
                      <TableRow>
                        <TableHead className="text-left">Account</TableHead>
                        <TableHead className="text-left">Cost centre</TableHead>
                        {MONTH_LABELS.map((m) => (
                          <TableHead key={m} className="text-right">
                            {m}
                          </TableHead>
                        ))}
                        <TableHead className="text-right">Annual</TableHead>
                        <TableHead className="text-right">
                          <span className="sr-only">Actions</span>
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {linesQ.data.map((line) => (
                        <TableRow key={line.id}>
                          <TableCell className="whitespace-nowrap text-left">
                            {accountLabel(line.account_code)}
                          </TableCell>
                          <TableCell className="whitespace-nowrap text-left text-fg-muted">
                            {costCenterLabel(line.cost_center)}
                          </TableCell>
                          {line.months.map((m, i) => (
                            <TableCell
                              key={i}
                              className="text-right font-tabular"
                            >
                              {money(m, { blankZero: true })}
                            </TableCell>
                          ))}
                          <TableCell className="text-right font-tabular font-semibold">
                            {money(line.annual_total)}
                          </TableCell>
                          <TableCell className="text-right">
                            <Button
                              type="button"
                              variant="ghost"
                              size="sm"
                              className="text-danger"
                              disabled={deleteLine.isPending}
                              onClick={() => deleteLine.mutate(line.id)}
                            >
                              Delete
                            </Button>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}

              {/* Variance dashboard */}
              <div className="flex flex-col gap-3">
                <h2 className="text-base font-semibold text-fg">
                  Plan vs. actual
                </h2>
                {varianceQ.isLoading && <TableSkeleton columns={6} />}
                {varianceQ.isError && (
                  <FinanceError
                    title="Couldn't compute variance"
                    error={varianceQ.error}
                    onRetry={() => void varianceQ.refetch()}
                  />
                )}
                {varianceQ.data && varianceQ.data.rows.length === 0 && (
                  <div className="rounded-lg border border-border p-8">
                    <p className="text-center text-sm text-fg-muted">
                      No actuals posted against this budget yet. Variance
                      appears once journal entries hit these accounts.
                    </p>
                  </div>
                )}
                {varianceQ.data && varianceQ.data.rows.length > 0 && (
                  <VarianceTable report={varianceQ.data} money={money} />
                )}
              </div>
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

function varianceTone(
  variance: number,
  favourable: boolean,
): { text: string; bar: string } {
  if (variance === 0) return { text: "text-fg-muted", bar: "bg-fg-muted" };
  return favourable
    ? { text: "text-success", bar: "bg-success" }
    : { text: "text-danger", bar: "bg-danger" };
}

// monthRange translates a "YYYY-MM" period label into the inclusive
// UTC start/end of that calendar month, formatted as RFC3339 strings
// the JournalEntriesPage filter expects. Returns null for unrecognised
// labels so the drill-down link omits the date filter.
function monthRange(period: string): { from: string; to: string } | null {
  const m = /^(\d{4})-(\d{2})$/.exec(period);
  if (!m) return null;
  const year = Number(m[1]);
  const month = Number(m[2]);
  if (month < 1 || month > 12) return null;
  const from = new Date(Date.UTC(year, month - 1, 1, 0, 0, 0, 0));
  const to = new Date(Date.UTC(year, month, 0, 23, 59, 59, 999));
  return { from: from.toISOString(), to: to.toISOString() };
}

function VarianceTable({
  report,
  money,
}: {
  report: BudgetVarianceReport;
  money: Money;
}) {
  const maxAbs = Math.max(
    1,
    ...report.rows.map((r) => Math.abs(Number(r.variance) || 0)),
  );
  return (
    <div className="overflow-x-auto">
      <Table className="text-xs">
        <TableHeader>
          <TableRow>
            <TableHead className="text-left">Account</TableHead>
            <TableHead className="text-left">Cost centre</TableHead>
            <TableHead className="text-left">Period</TableHead>
            <TableHead className="text-right">Plan</TableHead>
            <TableHead className="text-right">Actual</TableHead>
            <TableHead className="text-right">Variance</TableHead>
            <TableHead className="text-right">%</TableHead>
            <TableHead className="text-right">Chart</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {report.rows.map((row) => (
            <VarianceRowRender
              key={row.account_code + row.period + (row.cost_center ?? "")}
              row={row}
              maxAbs={maxAbs}
              money={money}
            />
          ))}
          <TableRow className="font-semibold">
            <TableCell colSpan={3} className="text-left">
              Total
            </TableCell>
            <TableCell className="text-right font-tabular">
              {money(report.total_budgeted)}
            </TableCell>
            <TableCell className="text-right font-tabular">
              {money(report.total_actual)}
            </TableCell>
            <TableCell className="text-right font-tabular">
              {money(report.total_variance)}
            </TableCell>
            <TableCell />
            <TableCell />
          </TableRow>
          <TableRow className="text-success">
            <TableCell colSpan={5} className="text-right">
              Favourable variance (better than plan)
            </TableCell>
            <TableCell className="text-right font-tabular">
              +{money(report.total_favourable_variance)}
            </TableCell>
            <TableCell />
            <TableCell />
          </TableRow>
          <TableRow className="text-danger">
            <TableCell colSpan={5} className="text-right">
              Unfavourable variance (worse than plan)
            </TableCell>
            <TableCell className="text-right font-tabular">
              −{money(report.total_unfavourable_variance)}
            </TableCell>
            <TableCell />
            <TableCell />
          </TableRow>
        </TableBody>
      </Table>
    </div>
  );
}

function VarianceRowRender({
  row,
  maxAbs,
  money,
}: {
  row: BudgetVarianceRow;
  maxAbs: number;
  money: Money;
}) {
  const variance = Number(row.variance) || 0;
  const pct = Number(row.variance_pct) || 0;
  const widthPct = Math.min(100, (Math.abs(variance) / maxAbs) * 100);
  const tone = varianceTone(variance, row.favourable);
  const range = monthRange(row.period);
  const qs = new URLSearchParams();
  qs.set("account_code", row.account_code);
  if (range) {
    qs.set("from", range.from);
    qs.set("to", range.to);
  }
  const periodHref = `/finance/journal?${qs.toString()}`;
  const accountLabel = row.account_name
    ? `${row.account_code} — ${row.account_name}`
    : row.account_code;
  return (
    <TableRow>
      <TableCell className="text-left">{accountLabel}</TableCell>
      <TableCell className="text-left text-fg-muted">
        {row.cost_center ?? "—"}
      </TableCell>
      <TableCell className="text-left">
        <Link
          to={periodHref}
          className="text-accent hover:underline focus-visible:outline-none focus-visible:underline"
        >
          {formatPeriod(row.period)}
        </Link>
      </TableCell>
      <TableCell className="text-right font-tabular">
        {money(row.budgeted)}
      </TableCell>
      <TableCell className="text-right font-tabular">
        {money(row.actual)}
      </TableCell>
      <TableCell className={`text-right font-tabular ${tone.text}`}>
        {money(row.variance)}
      </TableCell>
      <TableCell className={`text-right font-tabular ${tone.text}`}>
        {row.unplanned
          ? "—"
          : `${(pct * 100).toLocaleString(undefined, { maximumFractionDigits: 1 })}%`}
      </TableCell>
      <TableCell className="w-[200px]">
        <div className="h-2 overflow-hidden rounded bg-bg-muted">
          <div
            className={`h-full ${tone.bar}`}
            style={{ width: `${widthPct}%` }}
          />
        </div>
      </TableCell>
    </TableRow>
  );
}

export default BudgetPage;
