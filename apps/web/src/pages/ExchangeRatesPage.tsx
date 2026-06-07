import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ExchangeRate, UpsertExchangeRateInput } from "@kapp/client";
import {
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * ExchangeRatesPage renders the tenant's per-day currency conversion
 * table and lets finance admins upsert new quotes. The list query
 * returns rates newest-first; the upsert form accepts an ISO-4217
 * pair plus the rate date so historical quotes can be seeded
 * alongside today's.
 */
export function ExchangeRatesPage() {
  const qc = useQueryClient();
  const q = useQuery<{ rates: ExchangeRate[] }>({
    queryKey: ["exchange-rates"],
    queryFn: () => api.listExchangeRates({ limit: 200 }),
  });

  const upsert = useMutation({
    mutationFn: (input: UpsertExchangeRateInput) => api.upsertExchangeRate(input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["exchange-rates"] }),
  });

  const [form, setForm] = useState<UpsertExchangeRateInput>({
    from_currency: "USD",
    to_currency: "EUR",
    rate_date: new Date().toISOString().slice(0, 10),
    rate: "1.0",
    provider: "",
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    upsert.mutate({
      ...form,
      from_currency: form.from_currency.toUpperCase(),
      to_currency: form.to_currency.toUpperCase(),
    });
  };

  const rates = q.data?.rates ?? [];

  return (
    <section>
      <h1>Exchange Rates</h1>
      <p className="text-fg-muted">
        Per-tenant daily FX quotes. The posting engine looks up the
        effective rate for a journal entry using the latest row on or
        before the entry date.
      </p>

      <form
        onSubmit={submit}
        className="my-3 flex flex-wrap gap-2 text-[13px]"
      >
        <Input
          placeholder="from"
          value={form.from_currency}
          onChange={(e) => setForm({ ...form, from_currency: e.target.value })}
          maxLength={3}
          required
          className="w-16"
        />
        <Input
          placeholder="to"
          value={form.to_currency}
          onChange={(e) => setForm({ ...form, to_currency: e.target.value })}
          maxLength={3}
          required
          className="w-16"
        />
        <Input
          type="date"
          value={form.rate_date}
          onChange={(e) => setForm({ ...form, rate_date: e.target.value })}
          required
          className="w-auto"
        />
        <Input
          placeholder="rate"
          value={form.rate}
          onChange={(e) => setForm({ ...form, rate: e.target.value })}
          required
          className="w-[100px]"
        />
        <Input
          placeholder="provider (optional)"
          value={form.provider ?? ""}
          onChange={(e) => setForm({ ...form, provider: e.target.value })}
          className="w-auto"
        />
        <Button type="submit" disabled={upsert.isPending}>
          {upsert.isPending ? "Saving…" : "Save rate"}
        </Button>
      </form>

      {upsert.isError && (
        <p className="text-[13px] text-danger">
          {(upsert.error as Error).message}
        </p>
      )}

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load rates: {(q.error as Error).message}
        </p>
      )}
      {!q.isLoading && !q.isError && rates.length === 0 && (
        <p className="italic text-fg-subtle">
          No exchange rates yet.
        </p>
      )}

      {rates.length > 0 && (
        <Table className="text-[13px]">
          <TableHeader>
            <TableRow>
              <TableHead>Date</TableHead>
              <TableHead>Pair</TableHead>
              <TableHead className="text-right">Rate</TableHead>
              <TableHead>Provider</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rates.map((r) => (
              <TableRow
                key={`${r.from_currency}-${r.to_currency}-${r.rate_date}`}
              >
                <TableCell>{r.rate_date.slice(0, 10)}</TableCell>
                <TableCell>
                  <code>
                    {r.from_currency} → {r.to_currency}
                  </code>
                </TableCell>
                <TableCell className="text-right">{r.rate}</TableCell>
                <TableCell>{r.provider ?? ""}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
