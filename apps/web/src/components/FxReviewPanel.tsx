import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ExchangeRate } from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Input,
  Skeleton,
  StatCard,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import { consolidationApi, type RevaluationResult } from "./ConsolidationApi";
import { formatMoney, parseDateValue } from "./reconciliation";
import { ct } from "./ConsolidationStrings";

function signVariant(amount: string): "success" | "danger" | "default" {
  const n = Number(amount);
  if (!Number.isFinite(n) || n === 0) return "default";
  return n > 0 ? "success" : "danger";
}

// Money amounts across the FX surface share the reconciliation formatter
// so every figure renders with grouped digits and two decimals.
function useMoney() {
  const f = useFormatter();
  return (value: string | number) =>
    formatMoney(f, typeof value === "number" ? value : Number(value));
}

function ExchangeRatesCard() {
  const qc = useQueryClient();
  const f = useFormatter();
  const q = useQuery<{ rates: ExchangeRate[] }>({
    queryKey: ["consolidation.fx.rates"],
    queryFn: () => api.listExchangeRates({ limit: 100 }),
  });
  const [form, setForm] = useState({
    from_currency: "USD",
    to_currency: "EUR",
    rate_date: new Date().toISOString().slice(0, 10),
    rate: "1.0",
    provider: "",
  });
  const upsert = useMutation({
    mutationFn: () =>
      api.upsertExchangeRate({
        ...form,
        from_currency: form.from_currency.toUpperCase(),
        to_currency: form.to_currency.toUpperCase(),
      }),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["consolidation.fx.rates"] }),
  });
  const rates = q.data?.rates ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle>{ct("consolidation.fx.rates")}</CardTitle>
        <p className="text-sm text-fg-muted">{ct("consolidation.fx.ratesHint")}</p>
      </CardHeader>
      <CardContent className="grid gap-3">
        <form
          className="flex flex-wrap items-end gap-2 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            upsert.mutate();
          }}
        >
          <Input
            aria-label={ct("consolidation.fx.from")}
            placeholder={ct("consolidation.fx.from")}
            value={form.from_currency}
            onChange={(e) => setForm({ ...form, from_currency: e.target.value })}
            maxLength={3}
            className="w-16"
            required
          />
          <Input
            aria-label={ct("consolidation.fx.to")}
            placeholder={ct("consolidation.fx.to")}
            value={form.to_currency}
            onChange={(e) => setForm({ ...form, to_currency: e.target.value })}
            maxLength={3}
            className="w-16"
            required
          />
          <Input
            type="date"
            aria-label={ct("consolidation.fx.date")}
            value={form.rate_date}
            onChange={(e) => setForm({ ...form, rate_date: e.target.value })}
            className="w-auto"
            required
          />
          <Input
            aria-label={ct("consolidation.fx.rate")}
            placeholder={ct("consolidation.fx.rate")}
            value={form.rate}
            onChange={(e) => setForm({ ...form, rate: e.target.value })}
            className="w-24"
            required
          />
          <Input
            aria-label={ct("consolidation.fx.provider")}
            placeholder={ct("consolidation.fx.provider")}
            value={form.provider}
            onChange={(e) => setForm({ ...form, provider: e.target.value })}
            className="w-auto"
          />
          <Button type="submit" disabled={upsert.isPending}>
            {upsert.isPending
              ? ct("consolidation.fx.savingRate")
              : ct("consolidation.fx.saveRate")}
          </Button>
        </form>
        {upsert.error ? (
          <p className="text-sm text-danger">{(upsert.error as Error).message}</p>
        ) : null}

        {q.isLoading ? (
          <div className="grid gap-2">
            <Skeleton variant="rect" className="h-8" />
            <Skeleton variant="rect" className="h-8" />
          </div>
        ) : null}
        {q.isError && !q.isLoading ? (
          <div className="grid gap-2 rounded-md border border-border bg-bg-subtle p-3">
            <p className="text-sm text-danger">
              {ct("consolidation.fx.ratesError")}
            </p>
            <div>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                onClick={() => q.refetch()}
              >
                {ct("consolidation.retry")}
              </Button>
            </div>
          </div>
        ) : null}
        {!q.isLoading && !q.isError && rates.length === 0 ? (
          <EmptyState
            title={ct("consolidation.fx.noRates")}
            description={ct("consolidation.fx.noRatesHint")}
          />
        ) : null}
        {rates.length > 0 ? (
          <Table className="text-sm">
            <TableHeader>
              <TableRow>
                <TableHead>{ct("consolidation.fx.date")}</TableHead>
                <TableHead>{ct("consolidation.fx.pair")}</TableHead>
                <TableHead className="text-right">
                  {ct("consolidation.fx.rate")}
                </TableHead>
                <TableHead>{ct("consolidation.fx.provider")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rates.map((r) => {
                const d = parseDateValue(r.rate_date);
                return (
                  <TableRow
                    key={`${r.from_currency}-${r.to_currency}-${r.rate_date}`}
                  >
                    <TableCell className="whitespace-nowrap">
                      {d ? f.date(d) : r.rate_date.slice(0, 10)}
                    </TableCell>
                    <TableCell>
                      <code>
                        {r.from_currency} → {r.to_currency}
                      </code>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.rate}
                    </TableCell>
                    <TableCell>{r.provider ?? ""}</TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        ) : null}
      </CardContent>
    </Card>
  );
}

function TranslateCard() {
  const money = useMoney();
  const [form, setForm] = useState({
    from: "EUR",
    to: "USD",
    amount: "1000",
    date: "",
  });
  const convert = useMutation({
    mutationFn: () =>
      api.convertCurrency({
        from: form.from.toUpperCase(),
        to: form.to.toUpperCase(),
        amount: form.amount,
        ...(form.date ? { date: form.date } : {}),
      }),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>{ct("consolidation.fx.translate")}</CardTitle>
        <p className="text-sm text-fg-muted">
          {ct("consolidation.fx.translateHint")}
        </p>
      </CardHeader>
      <CardContent className="grid gap-3">
        <form
          className="flex flex-wrap items-end gap-2 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            convert.mutate();
          }}
        >
          <Input
            aria-label={ct("consolidation.fx.amount")}
            placeholder={ct("consolidation.fx.amount")}
            value={form.amount}
            onChange={(e) => setForm({ ...form, amount: e.target.value })}
            className="w-28"
            required
          />
          <Input
            aria-label={ct("consolidation.fx.from")}
            placeholder={ct("consolidation.fx.from")}
            value={form.from}
            onChange={(e) => setForm({ ...form, from: e.target.value })}
            maxLength={3}
            className="w-16"
            required
          />
          <Input
            aria-label={ct("consolidation.fx.to")}
            placeholder={ct("consolidation.fx.to")}
            value={form.to}
            onChange={(e) => setForm({ ...form, to: e.target.value })}
            maxLength={3}
            className="w-16"
            required
          />
          <Input
            type="date"
            aria-label={ct("consolidation.fx.date")}
            value={form.date}
            onChange={(e) => setForm({ ...form, date: e.target.value })}
            className="w-auto"
          />
          <Button type="submit" disabled={convert.isPending}>
            {convert.isPending
              ? ct("consolidation.fx.converting")
              : ct("consolidation.fx.convert")}
          </Button>
        </form>
        {convert.error ? (
          <p className="text-sm text-danger">{(convert.error as Error).message}</p>
        ) : null}
        {convert.data ? (
          <p className="text-sm" data-testid="fx-convert-result">
            {ct("consolidation.fx.converted")}:{" "}
            <span className="font-medium tabular-nums">
              {money(convert.data.converted)} {convert.data.to}
            </span>{" "}
            <span className="text-fg-muted">
              ({ct("consolidation.fx.usingRate")} {convert.data.rate})
            </span>
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

function UnrealizedCard() {
  const money = useMoney();
  const [form, setForm] = useState({
    foreign_amount: "1000",
    foreign_currency: "EUR",
    functional_currency: "USD",
    original_rate: "1.05",
    as_of: "",
  });
  const compute = useMutation({
    mutationFn: () =>
      api.unrealizedGainLoss({
        foreign_amount: form.foreign_amount,
        foreign_currency: form.foreign_currency.toUpperCase(),
        functional_currency: form.functional_currency.toUpperCase(),
        original_rate: form.original_rate,
        ...(form.as_of ? { as_of: form.as_of } : {}),
      }),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>{ct("consolidation.fx.unrealized")}</CardTitle>
        <p className="text-sm text-fg-muted">
          {ct("consolidation.fx.unrealizedHint")}
        </p>
      </CardHeader>
      <CardContent className="grid gap-3">
        <form
          className="flex flex-wrap items-end gap-2 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            compute.mutate();
          }}
        >
          <label className="grid gap-1">
            {ct("consolidation.fx.foreignAmount")}
            <Input
              value={form.foreign_amount}
              onChange={(e) => setForm({ ...form, foreign_amount: e.target.value })}
              className="w-28"
              required
            />
          </label>
          <label className="grid gap-1">
            {ct("consolidation.fx.foreignCurrency")}
            <Input
              value={form.foreign_currency}
              onChange={(e) =>
                setForm({ ...form, foreign_currency: e.target.value })
              }
              maxLength={3}
              className="w-16"
              required
            />
          </label>
          <label className="grid gap-1">
            {ct("consolidation.fx.functionalCurrency")}
            <Input
              value={form.functional_currency}
              onChange={(e) =>
                setForm({ ...form, functional_currency: e.target.value })
              }
              maxLength={3}
              className="w-16"
              required
            />
          </label>
          <label className="grid gap-1">
            {ct("consolidation.fx.originalRate")}
            <Input
              value={form.original_rate}
              onChange={(e) => setForm({ ...form, original_rate: e.target.value })}
              className="w-24"
              required
            />
          </label>
          <Button type="submit" disabled={compute.isPending}>
            {compute.isPending
              ? ct("consolidation.fx.computing")
              : ct("consolidation.fx.compute")}
          </Button>
        </form>
        {compute.error ? (
          <p className="text-sm text-danger">{(compute.error as Error).message}</p>
        ) : null}
        {compute.data ? (
          <p className="text-sm" data-testid="fx-unrealized-result">
            {ct("consolidation.fx.unrealizedResult")}:{" "}
            <Badge variant={signVariant(compute.data.unrealized_gain_loss)}>
              {money(compute.data.unrealized_gain_loss)}
            </Badge>{" "}
            <span className="text-xs text-fg-subtle">
              {ct("consolidation.fx.gainHint")}
            </span>
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

function RevaluationCard() {
  const money = useMoney();
  const [form, setForm] = useState({
    tenant_id: "",
    gain_account: "",
    loss_account: "",
    as_of: "",
  });
  const run = useMutation<RevaluationResult>({
    mutationFn: () =>
      consolidationApi.runFxRevaluation({
        tenant_id: form.tenant_id.trim(),
        ...(form.gain_account.trim() ? { gain_account: form.gain_account.trim() } : {}),
        ...(form.loss_account.trim() ? { loss_account: form.loss_account.trim() } : {}),
        ...(form.as_of ? { as_of: new Date(form.as_of).toISOString() } : {}),
      }),
  });
  const result = run.data;

  return (
    <Card>
      <CardHeader>
        <CardTitle>{ct("consolidation.fx.revaluation")}</CardTitle>
        <p className="text-sm text-fg-muted">
          {ct("consolidation.fx.revaluationHint")}
        </p>
      </CardHeader>
      <CardContent className="grid gap-3">
        <form
          className="flex flex-wrap items-end gap-2 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            run.mutate();
          }}
        >
          <label className="grid flex-1 gap-1">
            {ct("consolidation.fx.tenantId")}
            <Input
              value={form.tenant_id}
              onChange={(e) => setForm({ ...form, tenant_id: e.target.value })}
              placeholder="tenant uuid"
              required
            />
          </label>
          <label className="grid gap-1">
            {ct("consolidation.fx.gainAccount")}
            <Input
              value={form.gain_account}
              onChange={(e) => setForm({ ...form, gain_account: e.target.value })}
              className="w-28"
            />
          </label>
          <label className="grid gap-1">
            {ct("consolidation.fx.lossAccount")}
            <Input
              value={form.loss_account}
              onChange={(e) => setForm({ ...form, loss_account: e.target.value })}
              className="w-28"
            />
          </label>
          <label className="grid gap-1">
            {ct("consolidation.fx.revalAsOf")}
            <Input
              type="date"
              value={form.as_of}
              onChange={(e) => setForm({ ...form, as_of: e.target.value })}
              className="w-auto"
            />
          </label>
          <Button type="submit" disabled={!form.tenant_id.trim() || run.isPending}>
            {run.isPending
              ? ct("consolidation.fx.runningReval")
              : ct("consolidation.fx.runReval")}
          </Button>
        </form>
        {run.error ? (
          <p className="text-sm text-danger">{(run.error as Error).message}</p>
        ) : null}

        {!result ? (
          <p className="text-sm italic text-fg-subtle">
            {ct("consolidation.fx.revalEmpty")}
          </p>
        ) : (
          <div className="grid gap-3" data-testid="fx-reval-result">
            <div className="grid grid-cols-3 gap-3">
              <StatCard
                label={ct("consolidation.fx.totalGain")}
                value={money(result.total_gain)}
              />
              <StatCard
                label={ct("consolidation.fx.totalLoss")}
                value={money(result.total_loss)}
              />
              <StatCard
                label={ct("consolidation.fx.net")}
                value={
                  <Badge variant={signVariant(result.net)}>
                    {money(result.net)}
                  </Badge>
                }
              />
            </div>

            <div>
              <h4 className="mb-1 text-xs font-medium uppercase tracking-wide text-fg-subtle">
                {ct("consolidation.fx.revalLines")}
              </h4>
              {result.lines.length === 0 ? (
                <p className="text-sm italic text-fg-subtle">
                  {ct("consolidation.fx.noRevalLines")}
                </p>
              ) : (
                <Table className="text-xs">
                  <TableHeader>
                    <TableRow>
                      <TableHead>{ct("consolidation.tb.account")}</TableHead>
                      <TableHead>{ct("consolidation.fx.from")}</TableHead>
                      <TableHead className="text-right">
                        {ct("consolidation.fx.foreignNet")}
                      </TableHead>
                      <TableHead className="text-right">
                        {ct("consolidation.fx.currentRate")}
                      </TableHead>
                      <TableHead className="text-right">
                        {ct("consolidation.fx.recordedBase")}
                      </TableHead>
                      <TableHead className="text-right">
                        {ct("consolidation.fx.revaluedBase")}
                      </TableHead>
                      <TableHead className="text-right">
                        {ct("consolidation.fx.delta")}
                      </TableHead>
                      <TableHead>{ct("consolidation.fx.glAccount")}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {result.lines.map((l, i) => (
                      <TableRow key={`${l.account_code}-${l.currency}-${i}`}>
                        <TableCell>
                          <code>{l.account_code}</code>
                        </TableCell>
                        <TableCell>{l.currency}</TableCell>
                        <TableCell className="text-right tabular-nums">
                          {money(l.foreign_net)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {l.current_rate}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {money(l.recorded_base)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {money(l.revalued_base)}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          <Badge variant={signVariant(l.delta)}>
                            {money(l.delta)}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <code>{l.gain_loss_account}</code>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </div>

            {result.skipped.length > 0 ? (
              <div>
                <h4 className="mb-1 text-xs font-medium uppercase tracking-wide text-warning">
                  {ct("consolidation.fx.skipped")}
                </h4>
                <Table className="text-xs">
                  <TableHeader>
                    <TableRow>
                      <TableHead>{ct("consolidation.tb.account")}</TableHead>
                      <TableHead>{ct("consolidation.fx.from")}</TableHead>
                      <TableHead className="text-right">
                        {ct("consolidation.fx.foreignNet")}
                      </TableHead>
                      <TableHead>{ct("consolidation.fx.reason")}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {result.skipped.map((s, i) => (
                      <TableRow key={`${s.account_code}-${s.currency}-${i}`}>
                        <TableCell>
                          <code>{s.account_code}</code>
                        </TableCell>
                        <TableCell>{s.currency}</TableCell>
                        <TableCell className="text-right tabular-nums">
                          {money(s.foreign_net)}
                        </TableCell>
                        <TableCell>{s.reason}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            ) : null}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

/**
 * FX review surface: manage exchange-rate quotes, translate amounts at
 * the current rate, preview unrealized FX gain/loss on an open position
 * before posting, and trigger / review a tenant FX revaluation run.
 */
export function FxReviewPanel() {
  return (
    <div className="grid gap-4">
      <ExchangeRatesCard />
      <div className="grid gap-4 lg:grid-cols-2">
        <TranslateCard />
        <UnrealizedCard />
      </div>
      <RevaluationCard />
    </div>
  );
}
