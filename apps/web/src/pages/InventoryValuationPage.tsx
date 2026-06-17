import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Input,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { AlertTriangle, Coins, Download } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";

const todayLocalISO = (() => {
  const d = new Date();
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
})();

/** Quote a CSV cell only when it contains a delimiter, quote, or newline. */
function csvCell(value: string): string {
  return /[",\n]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}

/** Trigger a client-side download of a CSV built from the given rows. */
function downloadCsv(filename: string, headers: string[], rows: string[][]) {
  const body = [headers, ...rows]
    .map((cols) => cols.map(csvCell).join(","))
    .join("\n");
  const blob = new Blob([body], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

const MONEY_OPTS: Intl.NumberFormatOptions = {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
};

/**
 * InventoryValuationPage shows the monetary value of on-hand stock
 * as of a user-chosen date. Rows are grouped per item across all
 * warehouses; the total equals the sum of every row's value.
 */
export function InventoryValuationPage() {
  const fmt = useFormatter();
  const [asOf, setAsOf] = useState<string>(todayLocalISO);
  const q = useQuery({
    queryKey: ["inventory", "valuation", asOf],
    queryFn: () => api.getInventoryValuation(asOf),
  });
  const report = q.data;

  function handleExport() {
    if (!report) return;
    const header = ["SKU", "Item", "Quantity", "Value"];
    const data = report.rows.map((r) => [r.sku, r.name, r.qty, r.value_cost]);
    downloadCsv(`inventory-valuation-${asOf}.csv`, header, data);
    toast.success("Export complete", {
      description: `inventory-valuation-${asOf}.csv · ${data.length} row(s)`,
    });
  }

  const asOfLabel = fmt.date(new Date(`${asOf}T00:00:00`));

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Inventory</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Inventory Valuation
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              What your on-hand stock is worth as of {asOfLabel}.
            </p>
          </div>
          <div className="flex flex-wrap items-end gap-2">
            <Field label="As of" className="w-44">
              <Input
                type="date"
                value={asOf}
                max={todayLocalISO}
                onChange={(e) => setAsOf(e.target.value)}
              />
            </Field>
            <Button
              size="sm"
              variant="outline"
              leadingIcon={<Download className="size-4" />}
              onClick={handleExport}
              disabled={!report || report.rows.length === 0}
            >
              Export CSV
            </Button>
          </div>
        </div>
      </header>

      {q.isLoading ? (
        <ValuationSkeleton />
      ) : q.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load the valuation report"
          description={(q.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void q.refetch()}
              disabled={q.isFetching}
            >
              Retry
            </Button>
          }
        />
      ) : !report || report.rows.length === 0 ? (
        <EmptyState
          icon={<Coins />}
          title="Nothing to value yet"
          description="Once you hold stock with a cost, its value appears here."
        />
      ) : (
        <Table>
          <TableHeader className="sticky top-0 z-10">
            <TableRow>
              <TableHead>SKU</TableHead>
              <TableHead>Item</TableHead>
              <TableHead className="text-right">Quantity</TableHead>
              <TableHead className="text-right">Value</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {report.rows.map((r) => (
              <TableRow key={r.item_id}>
                <TableCell className="font-medium text-fg">{r.sku}</TableCell>
                <TableCell className="text-fg-muted">{r.name}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {fmt.number(Number(r.qty))}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {fmt.number(Number(r.value_cost), MONEY_OPTS)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
          <TableFooter>
            <TableRow>
              <TableCell colSpan={3} className="text-right font-medium text-fg">
                Total value
              </TableCell>
              <TableCell className="text-right font-semibold tabular-nums text-fg">
                {fmt.number(Number(report.total_value), MONEY_OPTS)}
              </TableCell>
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </section>
  );
}

/** Loading placeholder matching the valuation table shape. */
function ValuationSkeleton() {
  return (
    <div className="flex flex-col gap-2">
      <Skeleton className="h-9 w-full" />
      {Array.from({ length: 6 }).map((_, i) => (
        <Skeleton key={i} className="h-11 w-full" />
      ))}
    </div>
  );
}
