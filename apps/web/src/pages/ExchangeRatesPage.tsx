import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download } from "lucide-react";
import type { ExchangeRate, UpsertExchangeRateInput } from "@kapp/client";
import {
  Badge,
  Button,
  Eyebrow,
  Field,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { csvFilename, downloadCsv, parseAmount } from "../lib/finance/format";
import { FinanceError, TableSkeleton } from "../lib/finance/presentation";

const CURRENCY_RE = /^[A-Za-z]{3}$/;

function todayISO(): string {
  return new Date().toISOString().slice(0, 10);
}

/**
 * ExchangeRatesPage renders the tenant's per-day currency conversion
 * table and lets finance admins upsert new quotes. The list query
 * returns rates newest-first; the upsert form accepts an ISO-4217
 * pair plus the rate date so historical quotes can be seeded
 * alongside today's.
 */
export function ExchangeRatesPage() {
  const qc = useQueryClient();
  const f = useFormatter();

  const q = useQuery<{ rates: ExchangeRate[] }>({
    queryKey: ["exchange-rates"],
    queryFn: () => api.listExchangeRates({ limit: 200 }),
  });

  const upsert = useMutation({
    mutationFn: (input: UpsertExchangeRateInput) =>
      api.upsertExchangeRate(input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["exchange-rates"] });
      toast.success("Exchange rate saved");
    },
    onError: (err) =>
      toast.error("Couldn't save rate", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const [from, setFrom] = useState("USD");
  const [to, setTo] = useState("EUR");
  const [rateDate, setRateDate] = useState(todayISO());
  const [rate, setRate] = useState("1.0");
  const [provider, setProvider] = useState("");
  const [touched, setTouched] = useState(false);

  const rateNum = parseAmount(rate);
  const fromError =
    touched && !CURRENCY_RE.test(from.trim())
      ? "Use a 3-letter code, e.g. USD."
      : undefined;
  const toError =
    touched && !CURRENCY_RE.test(to.trim())
      ? "Use a 3-letter code, e.g. EUR."
      : undefined;
  const rateError =
    touched && !(Number.isFinite(rateNum) && rateNum > 0)
      ? "Enter a rate greater than zero."
      : undefined;
  const canSubmit =
    CURRENCY_RE.test(from.trim()) &&
    CURRENCY_RE.test(to.trim()) &&
    Number.isFinite(rateNum) &&
    rateNum > 0 &&
    !!rateDate;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setTouched(true);
    if (!canSubmit) return;
    upsert.mutate(
      {
        from_currency: from.trim().toUpperCase(),
        to_currency: to.trim().toUpperCase(),
        rate_date: rateDate,
        rate: rate.trim(),
        provider: provider.trim() || undefined,
      },
      {
        onSuccess: () => {
          setRate("1.0");
          setProvider("");
          setTouched(false);
        },
      },
    );
  };

  const rates = useMemo(() => q.data?.rates ?? [], [q.data]);

  const exportCsv = () => {
    downloadCsv(
      csvFilename("exchange_rates"),
      ["Date", "From", "To", "Rate", "Source"],
      rates.map((r) => [
        r.rate_date.slice(0, 10),
        r.from_currency,
        r.to_currency,
        r.rate,
        r.provider ?? "",
      ]),
    );
    toast.success("Exchange rates exported");
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Finance</Eyebrow>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight text-fg">
            Exchange Rates
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Daily currency rates for your business. Postings use the most
            recent rate on or before the entry date.
          </p>
        </div>
        <Button
          variant="secondary"
          leadingIcon={<Download aria-hidden />}
          onClick={exportCsv}
          disabled={rates.length === 0}
        >
          Export CSV
        </Button>
      </header>

      <form
        onSubmit={submit}
        className="flex flex-col gap-4 rounded-lg border border-border bg-bg-subtle p-4"
        noValidate
      >
        <h2 className="text-sm font-semibold text-fg">Add or update a rate</h2>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
          <Field label="From currency" required error={fromError}>
            <Input
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              maxLength={3}
              placeholder="USD"
              invalid={!!fromError}
              className="uppercase"
              required
            />
          </Field>
          <Field label="To currency" required error={toError}>
            <Input
              value={to}
              onChange={(e) => setTo(e.target.value)}
              maxLength={3}
              placeholder="EUR"
              invalid={!!toError}
              className="uppercase"
              required
            />
          </Field>
          <Field label="Rate date" required>
            <Input
              type="date"
              value={rateDate}
              onChange={(e) => setRateDate(e.target.value)}
              max={todayISO()}
              required
            />
          </Field>
          <Field
            label="Rate"
            required
            error={rateError}
            help="Units of the to-currency per 1 from-currency."
          >
            <Input
              inputMode="decimal"
              value={rate}
              onChange={(e) => setRate(e.target.value)}
              placeholder="0.91"
              invalid={!!rateError}
              className="text-right tabular-nums"
              required
            />
          </Field>
          <Field label="Source" help="Optional, e.g. ECB or manual.">
            <Input
              value={provider}
              onChange={(e) => setProvider(e.target.value)}
              placeholder="manual"
            />
          </Field>
        </div>
        <div>
          <Button type="submit" disabled={upsert.isPending}>
            {upsert.isPending ? "Saving…" : "Save rate"}
          </Button>
        </div>
      </form>

      {q.isLoading && <TableSkeleton columns={4} />}

      {q.isError && (
        <FinanceError
          title="Failed to load rates"
          error={q.error}
          onRetry={() => void q.refetch()}
        />
      )}

      {q.data && rates.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            No exchange rates yet. Add your first quote above.
          </p>
        </div>
      )}

      {rates.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-40">Date</TableHead>
              <TableHead>Pair</TableHead>
              <TableHead className="text-right">Rate</TableHead>
              <TableHead className="w-32">Source</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rates.map((r) => (
              <TableRow
                key={`${r.from_currency}-${r.to_currency}-${r.rate_date}`}
              >
                <TableCell className="whitespace-nowrap text-fg-muted">
                  {f.date(new Date(r.rate_date))}
                </TableCell>
                <TableCell>
                  <span className="inline-flex items-center gap-1.5 font-medium">
                    <span>{r.from_currency}</span>
                    <span aria-hidden className="text-fg-subtle">
                      →
                    </span>
                    <span>{r.to_currency}</span>
                  </span>
                </TableCell>
                <TableCell className="text-right font-tabular tabular-nums">
                  {f.number(parseAmount(r.rate), { maximumFractionDigits: 6 })}
                </TableCell>
                <TableCell>
                  {r.provider ? (
                    <Badge variant="neutral">{r.provider}</Badge>
                  ) : (
                    <span className="text-fg-subtle">—</span>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
