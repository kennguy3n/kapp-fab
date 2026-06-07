import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  Budget,
  BudgetLine,
  BudgetVarianceReport,
  BudgetVarianceRow,
  CreateBudgetInput,
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

/**
 * Phase N5 — BudgetPage.
 *
 * Surfaces the budget module to the finance UI. The page is split
 * into three vertical sections:
 *
 *   1. A list of budgets with inline status badges and a
 *      "+ New budget" header form.
 *   2. The selected budget's line editor — a wide table whose 12
 *      monthly columns are individually editable; the row's annual
 *      total reflects the running sum as the user types.
 *   3. The variance dashboard — a bar chart per (account, month)
 *      comparing planned vs. actual with the variance % rendered
 *      inline. Drill-down to the underlying journal entries is
 *      driven by the period link on each row.
 *
 * The layout uses the @kapp/ui design system (Button, Input, Select,
 * Table, Badge) with semantic token classes. The only remaining
 * inline styles are the data-driven variance bar widths/colours,
 * which are computed per row and reference design tokens via CSS
 * custom properties rather than hardcoded hex.
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

const fmtNumber = (s: string | undefined): string => {
  if (!s) return "0";
  const n = Number(s);
  if (!Number.isFinite(n)) return s;
  return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
};

// normalizeMoneyInput coerces a raw user-typed decimal string to a
// wire-safe value. `<input type="number">` produces "" when the user
// clears the field (a common editing operation), and shopspring's
// decimal.Decimal cannot unmarshal an empty JSON string — it returns
// `decimal: NewFromString: can't convert  to decimal: exponent is
// not numeric` and the Go handler then surfaces a generic 400
// "invalid JSON body" with no field-level context. Normalising at
// the wire boundary (rather than mid-typing) keeps the on-screen
// input cursor stable while the user is actively editing yet
// guarantees every monthly amount the API receives is a valid
// decimal literal.
const normalizeMoneyInput = (raw: string): string => {
  const trimmed = raw.trim();
  return trimmed === "" ? "0" : trimmed;
};

// normalizeOptionalDecimal collapses an empty / whitespace-only
// optional decimal string (e.g. a cleared variance_threshold input)
// to `undefined` so the request body omits the field rather than
// shipping "" — which is the same shopspring/decimal unmarshal
// failure path as normalizeMoneyInput, but for nullable fields.
const normalizeOptionalDecimal = (raw: string | undefined): string | undefined => {
  if (raw === undefined) return undefined;
  const trimmed = raw.trim();
  return trimmed === "" ? undefined : trimmed;
};

const STATUS_VARIANT: Record<
  Budget["status"],
  "default" | "success" | "warning"
> = {
  draft: "warning",
  active: "success",
  closed: "default",
};

const statusBadge = (status: Budget["status"]) => (
  <Badge variant={STATUS_VARIANT[status]} className="uppercase">
    {status}
  </Badge>
);

export function BudgetPage() {
  const qc = useQueryClient();
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

  const budgets = budgetsQ.data ?? [];
  const selectedBudget =
    selectedId !== null
      ? budgets.find((b) => b.id === selectedId) ?? null
      : null;

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
    // Normalize the optional `variance_threshold` at the wire
    // boundary so a cleared input does not ship `""` to the Go
    // backend (where `*decimal.Decimal` unmarshalling fails). The
    // editor state keeps the raw string so the input cursor stays
    // stable while the user is mid-typing.
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
      setSelectedId(b.id);
      qc.invalidateQueries({ queryKey: ["budgets"] });
    },
  });

  const upsertLine = useMutation({
    // Normalize cleared monthly inputs at the wire boundary. Each
    // raw user keystroke is kept in `draft.months` for smooth UX
    // (so the cursor doesn't snap when the user is mid-edit), but
    // the API only ever sees valid decimal literals — cleared
    // months ship as "0" rather than "" which would fail
    // shopspring/decimal unmarshalling on the Go side.
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
    },
  });

  const deleteLine = useMutation({
    mutationFn: (lineId: string) =>
      api.deleteBudgetLine(selectedId as string, lineId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["budget-lines", selectedId] });
      qc.invalidateQueries({ queryKey: ["budget-variance", selectedId] });
    },
  });

  const draftTotal = useMemo(
    () =>
      draft.months.reduce((sum, m) => sum + (Number(m) || 0), 0).toLocaleString(
        undefined,
        { maximumFractionDigits: 2 },
      ),
    [draft.months],
  );

  return (
    <section>
      <h1>Budgets</h1>
      <p className="text-fg-muted">
        Annual finance plans, with monthly line items by account and
        cost centre. The variance dashboard compares posted journal
        entries against plan and emits alerts when variance crosses
        the per-budget threshold.
      </p>

      {/* ---------- Budget list + create form ----------- */}
      <div className="flex items-start gap-6">
        <div className="flex-[1_1_320px]">
          <h2 className="text-base">Budgets</h2>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="mb-2"
            onClick={() => setCreating((c) => !c)}
          >
            {creating ? "Cancel" : "+ New budget"}
          </Button>
          {creating && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                createBudget.mutate(newBudget);
              }}
              className="mb-3 grid gap-1.5"
            >
              <Input
                placeholder="Name (e.g. Marketing FY26)"
                value={newBudget.name}
                onChange={(e) =>
                  setNewBudget({ ...newBudget, name: e.target.value })
                }
                required
              />
              <Input
                type="number"
                placeholder="Fiscal year"
                value={newBudget.fiscal_year}
                onChange={(e) =>
                  setNewBudget({
                    ...newBudget,
                    fiscal_year: Number(e.target.value),
                  })
                }
                required
              />
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
              <Input
                placeholder="Default cost centre (optional)"
                value={newBudget.cost_center ?? ""}
                onChange={(e) =>
                  setNewBudget({ ...newBudget, cost_center: e.target.value })
                }
              />
              <Input
                type="number"
                step="0.001"
                placeholder="Variance threshold (e.g. 0.10 = 10%)"
                value={newBudget.variance_threshold ?? ""}
                onChange={(e) =>
                  setNewBudget({
                    ...newBudget,
                    variance_threshold: e.target.value,
                  })
                }
              />
              <Button type="submit" disabled={createBudget.isPending}>
                {createBudget.isPending ? "Saving…" : "Create budget"}
              </Button>
              {createBudget.isError && (
                <p className="text-xs text-danger">
                  {(createBudget.error as Error).message}
                </p>
              )}
            </form>
          )}
          {budgetsQ.isLoading && <p>Loading…</p>}
          {budgetsQ.isError && (
            <p className="text-danger">
              {(budgetsQ.error as Error).message}
            </p>
          )}
          {!budgetsQ.isLoading && budgets.length === 0 && (
            <p className="italic text-fg-subtle">
              No budgets yet. Create one to get started.
            </p>
          )}
          <ul className="list-none p-0">
            {budgets.map((b) => {
              const isSelected = b.id === selectedId;
              return (
                <li key={b.id} className="mb-1">
                  <Button
                    type="button"
                    variant={isSelected ? "secondary" : "outline"}
                    onClick={() => setSelectedId(b.id)}
                    className={`h-auto w-full flex-col items-stretch justify-start whitespace-normal py-2 text-left${
                      isSelected ? " border-accent" : ""
                    }`}
                  >
                    <div className="flex justify-between">
                      <strong>{b.name}</strong>
                      {statusBadge(b.status)}
                    </div>
                    <div className="text-xs text-fg-muted">
                      FY{b.fiscal_year}
                      {b.cost_center ? ` · CC ${b.cost_center}` : ""}
                    </div>
                  </Button>
                </li>
              );
            })}
          </ul>
        </div>

        {/* ---------- Selected budget: lines + variance ----------- */}
        <div className="flex-[2_1_700px]">
          {!selectedBudget && (
            <p className="italic text-fg-subtle">
              Select a budget from the list to edit lines and view variance.
            </p>
          )}
          {selectedBudget && (
            <>
              <h2 className="text-base">{selectedBudget.name} — Lines</h2>

              {/* Line editor */}
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  upsertLine.mutate(draft);
                }}
                className="mb-4"
              >
                <div className="flex flex-wrap items-center gap-1.5 text-xs">
                  <Input
                    placeholder="Account code"
                    value={draft.account_code}
                    onChange={(e) =>
                      setDraft({ ...draft, account_code: e.target.value })
                    }
                    required
                    className="w-[110px]"
                  />
                  <Input
                    placeholder="Cost centre (optional)"
                    value={draft.cost_center}
                    onChange={(e) =>
                      setDraft({ ...draft, cost_center: e.target.value })
                    }
                    className="w-[130px]"
                  />
                  {MONTH_LABELS.map((label, idx) => (
                    <label key={label} className="flex flex-col">
                      <span className="text-[10px] text-fg-muted">{label}</span>
                      <Input
                        type="number"
                        step="0.01"
                        value={draft.months[idx]}
                        onChange={(e) => {
                          const next = [...draft.months];
                          next[idx] = e.target.value;
                          setDraft({ ...draft, months: next });
                        }}
                        className="w-[70px]"
                      />
                    </label>
                  ))}
                  <div className="ml-2">
                    Annual: <strong>{draftTotal}</strong>
                  </div>
                  <Button type="submit" size="sm" disabled={upsertLine.isPending}>
                    {upsertLine.isPending ? "Saving…" : "Save line"}
                  </Button>
                </div>
              </form>

              {linesQ.isLoading && <p>Loading…</p>}
              {linesQ.data && linesQ.data.length === 0 && (
                <p className="italic text-fg-subtle">
                  No lines on this budget yet.
                </p>
              )}
              {linesQ.data && linesQ.data.length > 0 && (
                <Table className="mb-4 text-xs">
                  <TableHeader>
                    <TableRow className="text-right">
                      <TableHead className="text-left">Account</TableHead>
                      <TableHead className="text-left">CC</TableHead>
                      {MONTH_LABELS.map((m) => (
                        <TableHead key={m} className="text-right">
                          {m}
                        </TableHead>
                      ))}
                      <TableHead className="text-right">Annual</TableHead>
                      <TableHead />
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {linesQ.data.map((line) => (
                      <TableRow key={line.id} className="text-right">
                        <TableCell className="text-left">
                          {line.account_code}
                        </TableCell>
                        <TableCell className="text-left">
                          {line.cost_center ?? ""}
                        </TableCell>
                        {line.months.map((m, i) => (
                          <TableCell key={i}>{fmtNumber(m)}</TableCell>
                        ))}
                        <TableCell>
                          <strong>{fmtNumber(line.annual_total)}</strong>
                        </TableCell>
                        <TableCell>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            className="text-danger"
                            onClick={() => deleteLine.mutate(line.id)}
                          >
                            Delete
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}

              {/* Variance dashboard */}
              <h2 className="text-base">Variance — plan vs. actual</h2>
              {varianceQ.isLoading && <p>Computing…</p>}
              {varianceQ.data && (
                <VarianceTable report={varianceQ.data} />
              )}
            </>
          )}
        </div>
      </div>
    </section>
  );
}

function VarianceTable({ report }: { report: BudgetVarianceReport }) {
  const maxAbs = Math.max(
    1,
    ...report.rows.map((r) => Math.abs(Number(r.variance) || 0)),
  );
  return (
    <Table className="text-xs">
      <TableHeader>
        <TableRow className="text-right">
          <TableHead className="text-left">Account</TableHead>
          <TableHead className="text-left">CC</TableHead>
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
          <VarianceRowRender key={row.account_code + row.period + row.cost_center} row={row} maxAbs={maxAbs} />
        ))}
        <TableRow className="font-bold">
          <TableCell colSpan={3} className="text-left">
            Total
          </TableCell>
          <TableCell className="text-right">{fmtNumber(report.total_budgeted)}</TableCell>
          <TableCell className="text-right">{fmtNumber(report.total_actual)}</TableCell>
          <TableCell className="text-right">{fmtNumber(report.total_variance)}</TableCell>
          <TableCell />
          <TableCell />
        </TableRow>
        <TableRow className="text-[11px] text-success">
          <TableCell colSpan={5} className="pt-1 text-right">
            Favourable variance (better than plan)
          </TableCell>
          <TableCell className="pt-1 text-right">
            +{fmtNumber(report.total_favourable_variance)}
          </TableCell>
          <TableCell />
          <TableCell />
        </TableRow>
        <TableRow className="text-[11px] text-danger">
          <TableCell colSpan={5} className="text-right">
            Unfavourable variance (worse than plan)
          </TableCell>
          <TableCell className="text-right">
            −{fmtNumber(report.total_unfavourable_variance)}
          </TableCell>
          <TableCell />
          <TableCell />
        </TableRow>
      </TableBody>
    </Table>
  );
}

// Variance colours reference the design-system semantic tokens via
// CSS custom properties so the data-driven inline `color`/`background`
// on each variance row/bar stays in sync with the oklch palette
// instead of hardcoding hex literals.
const COLOUR_UNFAVOURABLE = "var(--danger)";
const COLOUR_FAVOURABLE = "var(--success)";
const COLOUR_NEUTRAL = "var(--fg-muted)";

// varianceColour picks the row's red/green colour from the
// backend-stamped `favourable` flag rather than re-deriving the
// good/bad reading client-side from account_type. The backend
// (isFavourableVariance in internal/finance/budget.go) is the
// single source of truth for which (account_type, variance sign)
// combinations are favourable, so the rollups in the footer
// (total_favourable_variance / total_unfavourable_variance) and
// the per-row colouring stay consistent even when the favourability
// rules for asset / liability / equity accounts evolve.
function varianceColour(variance: number, favourable: boolean): string {
  if (variance === 0) return COLOUR_NEUTRAL;
  return favourable ? COLOUR_FAVOURABLE : COLOUR_UNFAVOURABLE;
}

// monthRange translates a "YYYY-MM" period label into the
// inclusive UTC start/end of that calendar month, formatted as
// RFC3339 strings the JournalEntriesPage filter expects. Returns
// nulls if the period label is not a recognised YYYY-MM shape — in
// that case the drill-down link omits the date filter rather than
// emitting malformed query parameters.
//
// The `to` instant carries 999 milliseconds (the highest precision a
// JS Date supports). The backend handler at
// services/api/finance_handlers.go::listJournalEntries notices a
// 23:59:59.<sub-second> RFC3339 input and promotes it to nanosecond
// 999_999_999, matching the variance computation's end-of-day
// contract so the drill-down window covers the exact same set of
// journal entries the variance row aggregated.
function monthRange(period: string): { from: string; to: string } | null {
  const m = /^(\d{4})-(\d{2})$/.exec(period);
  if (!m) return null;
  const year = Number(m[1]);
  const month = Number(m[2]); // 1-based
  if (!Number.isFinite(year) || !Number.isFinite(month)) return null;
  if (month < 1 || month > 12) return null;
  const from = new Date(Date.UTC(year, month - 1, 1, 0, 0, 0, 0));
  // Last day of month at 23:59:59.999 UTC: day=0 of the next month
  // rolls back to the previous month's final day. Millisecond=999
  // is the highest precision a JS Date supports; the backend
  // promotes the value to the final nanosecond of the day so the
  // drill-down window exactly matches the variance window.
  const to = new Date(Date.UTC(year, month, 0, 23, 59, 59, 999));
  return { from: from.toISOString(), to: to.toISOString() };
}

function VarianceRowRender({
  row,
  maxAbs,
}: {
  row: BudgetVarianceRow;
  maxAbs: number;
}) {
  const variance = Number(row.variance) || 0;
  const pct = Number(row.variance_pct) || 0;
  const widthPct = Math.min(100, (Math.abs(variance) / maxAbs) * 100);
  const colour = varianceColour(variance, row.favourable);
  // Drill-down link: opens the journal entries page filtered to the
  // account_code and the calendar-month window of this variance row
  // so the user can see the underlying postings without leaving the
  // budget context. The JournalEntriesPage reads `account_code`,
  // `from`, and `to` from the query string and forwards them to
  // GET /finance/journal-entries.
  const range = monthRange(row.period);
  const qs = new URLSearchParams();
  qs.set("account_code", row.account_code);
  if (range) {
    qs.set("from", range.from);
    qs.set("to", range.to);
  }
  const periodHref = `/finance/journal?${qs.toString()}`;
  // Render "4000 — Sales Revenue" when the backend resolved the
  // account name; fall back to the bare code when the chart of
  // accounts has no entry for this code (the variance still has to
  // surface so the operator notices the orphan posting).
  const accountLabel = row.account_name
    ? `${row.account_code} — ${row.account_name}`
    : row.account_code;
  return (
    <TableRow className="text-right">
      <TableCell className="text-left">{accountLabel}</TableCell>
      <TableCell className="text-left">{row.cost_center ?? ""}</TableCell>
      <TableCell className="text-left">
        <Link to={periodHref}>{row.period}</Link>
      </TableCell>
      <TableCell>{fmtNumber(row.budgeted)}</TableCell>
      <TableCell>{fmtNumber(row.actual)}</TableCell>
      <TableCell style={{ color: colour }}>{fmtNumber(row.variance)}</TableCell>
      <TableCell style={{ color: colour }}>
        {row.unplanned
          ? "—"
          : `${(pct * 100).toLocaleString(undefined, { maximumFractionDigits: 1 })}%`}
      </TableCell>
      <TableCell className="w-[200px]">
        <div className="h-2 overflow-hidden rounded bg-bg-muted">
          <div
            className="h-full"
            style={{ width: `${widthPct}%`, background: colour }}
          />
        </div>
      </TableCell>
    </TableRow>
  );
}

export default BudgetPage;
