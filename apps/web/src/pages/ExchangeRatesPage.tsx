import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ExchangeRate, UpsertExchangeRateInput } from "@kapp/client";
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
      <p style={{ color: "#6b7280" }}>
        Per-tenant daily FX quotes. The posting engine looks up the
        effective rate for a journal entry using the latest row on or
        before the entry date.
      </p>

      <form
        onSubmit={submit}
        style={{ display: "flex", gap: 8, margin: "12px 0", fontSize: 13, flexWrap: "wrap" }}
      >
        <input
          placeholder="from"
          value={form.from_currency}
          onChange={(e) => setForm({ ...form, from_currency: e.target.value })}
          maxLength={3}
          required
          style={{ width: 60 }}
        />
        <input
          placeholder="to"
          value={form.to_currency}
          onChange={(e) => setForm({ ...form, to_currency: e.target.value })}
          maxLength={3}
          required
          style={{ width: 60 }}
        />
        <input
          type="date"
          value={form.rate_date}
          onChange={(e) => setForm({ ...form, rate_date: e.target.value })}
          required
        />
        <input
          placeholder="rate"
          value={form.rate}
          onChange={(e) => setForm({ ...form, rate: e.target.value })}
          required
          style={{ width: 100 }}
        />
        <input
          placeholder="provider (optional)"
          value={form.provider ?? ""}
          onChange={(e) => setForm({ ...form, provider: e.target.value })}
        />
        <button type="submit" disabled={upsert.isPending}>
          {upsert.isPending ? "Saving…" : "Save rate"}
        </button>
      </form>

      {upsert.isError && (
        <p style={{ color: "#b91c1c", fontSize: 13 }}>
          {(upsert.error as Error).message}
        </p>
      )}

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load rates: {(q.error as Error).message}
        </p>
      )}
      {!q.isLoading && rates.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No exchange rates yet.
        </p>
      )}

      {rates.length > 0 && (
        <table style={{ width: "100%", fontSize: 13, borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th>Date</th>
              <th>Pair</th>
              <th style={{ textAlign: "right" }}>Rate</th>
              <th>Provider</th>
            </tr>
          </thead>
          <tbody>
            {rates.map((r) => (
              <tr
                key={`${r.from_currency}-${r.to_currency}-${r.rate_date}`}
                style={{ borderBottom: "1px solid #f3f4f6" }}
              >
                <td>{r.rate_date.slice(0, 10)}</td>
                <td>
                  <code>
                    {r.from_currency} → {r.to_currency}
                  </code>
                </td>
                <td style={{ textAlign: "right" }}>{r.rate}</td>
                <td>{r.provider ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
