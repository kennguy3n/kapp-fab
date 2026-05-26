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
 * The layout uses inline styles to stay consistent with the rest of
 * the apps/web pages (which intentionally avoid a CSS-in-JS library
 * for build-time simplicity).
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

const statusBadge = (status: Budget["status"]) => {
  const colours: Record<Budget["status"], string> = {
    draft: "#9ca3af",
    active: "#16a34a",
    closed: "#6b7280",
  };
  return (
    <span
      style={{
        display: "inline-block",
        padding: "2px 8px",
        fontSize: 11,
        borderRadius: 4,
        background: colours[status],
        color: "white",
        textTransform: "uppercase",
      }}
    >
      {status}
    </span>
  );
};

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
    mutationFn: (input: CreateBudgetInput) => api.createBudget(input),
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
    mutationFn: (line: LineDraft) =>
      api.upsertBudgetLine(selectedId as string, {
        id: line.id,
        account_code: line.account_code,
        cost_center: line.cost_center || undefined,
        months: line.months,
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
      <p style={{ color: "#6b7280" }}>
        Annual finance plans, with monthly line items by account and
        cost centre. The variance dashboard compares posted journal
        entries against plan and emits alerts when variance crosses
        the per-budget threshold.
      </p>

      {/* ---------- Budget list + create form ----------- */}
      <div style={{ display: "flex", alignItems: "flex-start", gap: 24 }}>
        <div style={{ flex: "1 1 320px" }}>
          <h2 style={{ fontSize: 16 }}>Budgets</h2>
          <button
            type="button"
            onClick={() => setCreating((c) => !c)}
            style={{ marginBottom: 8 }}
          >
            {creating ? "Cancel" : "+ New budget"}
          </button>
          {creating && (
            <form
              onSubmit={(e) => {
                e.preventDefault();
                createBudget.mutate(newBudget);
              }}
              style={{ display: "grid", gap: 6, marginBottom: 12 }}
            >
              <input
                placeholder="Name (e.g. Marketing FY26)"
                value={newBudget.name}
                onChange={(e) =>
                  setNewBudget({ ...newBudget, name: e.target.value })
                }
                required
              />
              <input
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
              <select
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
              </select>
              <input
                placeholder="Default cost centre (optional)"
                value={newBudget.cost_center ?? ""}
                onChange={(e) =>
                  setNewBudget({ ...newBudget, cost_center: e.target.value })
                }
              />
              <input
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
              <button type="submit" disabled={createBudget.isPending}>
                {createBudget.isPending ? "Saving…" : "Create budget"}
              </button>
              {createBudget.isError && (
                <p style={{ color: "#b91c1c", fontSize: 12 }}>
                  {(createBudget.error as Error).message}
                </p>
              )}
            </form>
          )}
          {budgetsQ.isLoading && <p>Loading…</p>}
          {budgetsQ.isError && (
            <p style={{ color: "#b91c1c" }}>
              {(budgetsQ.error as Error).message}
            </p>
          )}
          {!budgetsQ.isLoading && budgets.length === 0 && (
            <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
              No budgets yet. Create one to get started.
            </p>
          )}
          <ul style={{ listStyle: "none", padding: 0 }}>
            {budgets.map((b) => {
              const isSelected = b.id === selectedId;
              return (
                <li key={b.id} style={{ marginBottom: 4 }}>
                  <button
                    type="button"
                    onClick={() => setSelectedId(b.id)}
                    style={{
                      width: "100%",
                      textAlign: "left",
                      padding: 8,
                      background: isSelected ? "#eef2ff" : "transparent",
                      border: isSelected
                        ? "1px solid #6366f1"
                        : "1px solid #e5e7eb",
                      borderRadius: 4,
                      cursor: "pointer",
                    }}
                  >
                    <div
                      style={{
                        display: "flex",
                        justifyContent: "space-between",
                      }}
                    >
                      <strong>{b.name}</strong>
                      {statusBadge(b.status)}
                    </div>
                    <div style={{ fontSize: 12, color: "#6b7280" }}>
                      FY{b.fiscal_year}
                      {b.cost_center ? ` · CC ${b.cost_center}` : ""}
                    </div>
                  </button>
                </li>
              );
            })}
          </ul>
        </div>

        {/* ---------- Selected budget: lines + variance ----------- */}
        <div style={{ flex: "2 1 700px" }}>
          {!selectedBudget && (
            <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
              Select a budget from the list to edit lines and view variance.
            </p>
          )}
          {selectedBudget && (
            <>
              <h2 style={{ fontSize: 16 }}>
                {selectedBudget.name} — Lines
              </h2>

              {/* Line editor */}
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  upsertLine.mutate(draft);
                }}
                style={{ marginBottom: 16 }}
              >
                <div
                  style={{
                    display: "flex",
                    gap: 6,
                    flexWrap: "wrap",
                    alignItems: "center",
                    fontSize: 12,
                  }}
                >
                  <input
                    placeholder="Account code"
                    value={draft.account_code}
                    onChange={(e) =>
                      setDraft({ ...draft, account_code: e.target.value })
                    }
                    required
                    style={{ width: 110 }}
                  />
                  <input
                    placeholder="Cost centre (optional)"
                    value={draft.cost_center}
                    onChange={(e) =>
                      setDraft({ ...draft, cost_center: e.target.value })
                    }
                    style={{ width: 130 }}
                  />
                  {MONTH_LABELS.map((label, idx) => (
                    <label
                      key={label}
                      style={{ display: "flex", flexDirection: "column" }}
                    >
                      <span style={{ fontSize: 10, color: "#6b7280" }}>
                        {label}
                      </span>
                      <input
                        type="number"
                        step="0.01"
                        value={draft.months[idx]}
                        onChange={(e) => {
                          const next = [...draft.months];
                          next[idx] = e.target.value;
                          setDraft({ ...draft, months: next });
                        }}
                        style={{ width: 70 }}
                      />
                    </label>
                  ))}
                  <div style={{ marginLeft: 8 }}>
                    Annual: <strong>{draftTotal}</strong>
                  </div>
                  <button type="submit" disabled={upsertLine.isPending}>
                    {upsertLine.isPending ? "Saving…" : "Save line"}
                  </button>
                </div>
              </form>

              {linesQ.isLoading && <p>Loading…</p>}
              {linesQ.data && linesQ.data.length === 0 && (
                <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
                  No lines on this budget yet.
                </p>
              )}
              {linesQ.data && linesQ.data.length > 0 && (
                <table
                  style={{
                    width: "100%",
                    fontSize: 12,
                    borderCollapse: "collapse",
                    marginBottom: 16,
                  }}
                >
                  <thead>
                    <tr
                      style={{
                        textAlign: "right",
                        borderBottom: "1px solid #e5e7eb",
                      }}
                    >
                      <th style={{ textAlign: "left" }}>Account</th>
                      <th style={{ textAlign: "left" }}>CC</th>
                      {MONTH_LABELS.map((m) => (
                        <th key={m}>{m}</th>
                      ))}
                      <th>Annual</th>
                      <th />
                    </tr>
                  </thead>
                  <tbody>
                    {linesQ.data.map((line) => (
                      <tr key={line.id} style={{ textAlign: "right" }}>
                        <td style={{ textAlign: "left" }}>
                          {line.account_code}
                        </td>
                        <td style={{ textAlign: "left" }}>
                          {line.cost_center ?? ""}
                        </td>
                        {line.months.map((m, i) => (
                          <td key={i}>{fmtNumber(m)}</td>
                        ))}
                        <td>
                          <strong>{fmtNumber(line.annual_total)}</strong>
                        </td>
                        <td>
                          <button
                            type="button"
                            onClick={() => deleteLine.mutate(line.id)}
                            style={{ color: "#b91c1c", fontSize: 11 }}
                          >
                            Delete
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}

              {/* Variance dashboard */}
              <h2 style={{ fontSize: 16 }}>Variance — plan vs. actual</h2>
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
    <table style={{ width: "100%", fontSize: 12, borderCollapse: "collapse" }}>
      <thead>
        <tr style={{ textAlign: "right", borderBottom: "1px solid #e5e7eb" }}>
          <th style={{ textAlign: "left" }}>Account</th>
          <th style={{ textAlign: "left" }}>CC</th>
          <th style={{ textAlign: "left" }}>Period</th>
          <th>Plan</th>
          <th>Actual</th>
          <th>Variance</th>
          <th>%</th>
          <th>Chart</th>
        </tr>
      </thead>
      <tbody>
        {report.rows.map((row) => (
          <VarianceRowRender key={row.account_code + row.period + row.cost_center} row={row} maxAbs={maxAbs} />
        ))}
        <tr style={{ fontWeight: "bold", borderTop: "1px solid #e5e7eb" }}>
          <td colSpan={3} style={{ textAlign: "left" }}>
            Total
          </td>
          <td style={{ textAlign: "right" }}>{fmtNumber(report.total_budgeted)}</td>
          <td style={{ textAlign: "right" }}>{fmtNumber(report.total_actual)}</td>
          <td style={{ textAlign: "right" }}>{fmtNumber(report.total_variance)}</td>
          <td />
          <td />
        </tr>
        <tr style={{ fontSize: 11, color: COLOUR_FAVOURABLE }}>
          <td colSpan={5} style={{ textAlign: "right", paddingTop: 4 }}>
            Favourable variance (better than plan)
          </td>
          <td style={{ textAlign: "right", paddingTop: 4 }}>
            +{fmtNumber(report.total_favourable_variance)}
          </td>
          <td />
          <td />
        </tr>
        <tr style={{ fontSize: 11, color: COLOUR_UNFAVOURABLE }}>
          <td colSpan={5} style={{ textAlign: "right" }}>
            Unfavourable variance (worse than plan)
          </td>
          <td style={{ textAlign: "right" }}>
            −{fmtNumber(report.total_unfavourable_variance)}
          </td>
          <td />
          <td />
        </tr>
      </tbody>
    </table>
  );
}

const COLOUR_UNFAVOURABLE = "#dc2626";
const COLOUR_FAVOURABLE = "#16a34a";
const COLOUR_NEUTRAL = "#6b7280";

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
function monthRange(period: string): { from: string; to: string } | null {
  const m = /^(\d{4})-(\d{2})$/.exec(period);
  if (!m) return null;
  const year = Number(m[1]);
  const month = Number(m[2]); // 1-based
  if (!Number.isFinite(year) || !Number.isFinite(month)) return null;
  if (month < 1 || month > 12) return null;
  const from = new Date(Date.UTC(year, month - 1, 1, 0, 0, 0));
  // Last day of month at 23:59:59 UTC: day=0 of the next month
  // rolls back to the previous month's final day.
  const to = new Date(Date.UTC(year, month, 0, 23, 59, 59));
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
    <tr style={{ textAlign: "right" }}>
      <td style={{ textAlign: "left" }}>{accountLabel}</td>
      <td style={{ textAlign: "left" }}>{row.cost_center ?? ""}</td>
      <td style={{ textAlign: "left" }}>
        <Link to={periodHref}>{row.period}</Link>
      </td>
      <td>{fmtNumber(row.budgeted)}</td>
      <td>{fmtNumber(row.actual)}</td>
      <td style={{ color: colour }}>{fmtNumber(row.variance)}</td>
      <td style={{ color: colour }}>
        {(pct * 100).toLocaleString(undefined, { maximumFractionDigits: 1 })}%
      </td>
      <td style={{ width: 200 }}>
        <div
          style={{
            height: 8,
            background: "#f3f4f6",
            borderRadius: 4,
            overflow: "hidden",
          }}
        >
          <div
            style={{
              width: `${widthPct}%`,
              height: "100%",
              background: colour,
            }}
          />
        </div>
      </td>
    </tr>
  );
}

export default BudgetPage;
