import { Badge } from "@kapp/ui";
import {
  convertToBase,
  formatAmount,
  isForeignLine,
  type RateMap,
} from "./reconciliation";
import { rt, rtp } from "./ReconciliationStrings";

/**
 * ReconciliationFxCell renders a bank line's amount and, when the line is
 * denominated in a currency other than the account's base currency, its
 * base-currency equivalent plus the conversion rate applied. If no rate is
 * published it says so explicitly rather than inventing a figure — the
 * operator must not be shown a fabricated base amount. This is the surface
 * that stops a foreign line from being silently matched to a base-currency
 * ledger entry on a bare numeric coincidence.
 *
 * Note: we deliberately do NOT show "base − face value" as an "FX
 * difference" because those operands are in different currency units, so
 * the subtraction is not a meaningful financial quantity (it only looks
 * plausible when the rate is near 1.0, e.g. EUR/USD, and is nonsense for
 * pairs like JPY/USD). The unit-free conversion rate conveys the FX impact
 * correctly for every currency pair.
 */
export function ReconciliationFxCell({
  amount,
  lineCurrency,
  baseCurrency,
  rates,
  align = "right",
}: {
  amount: number;
  lineCurrency?: string;
  baseCurrency?: string;
  rates: RateMap;
  align?: "left" | "right";
}) {
  const foreign = isForeignLine(lineCurrency, baseCurrency);
  const alignCls = align === "right" ? "text-right" : "text-left";

  if (!foreign) {
    return (
      <span className={`font-semibold tabular-nums text-fg ${alignCls}`}>
        {formatAmount(amount, lineCurrency)}
      </span>
    );
  }

  const conv = convertToBase(amount, lineCurrency, baseCurrency, rates);

  return (
    <span className={`flex flex-col ${align === "right" ? "items-end" : "items-start"}`}>
      <span className="font-semibold tabular-nums text-fg">
        {formatAmount(amount, lineCurrency)}
      </span>
      <Badge variant="warning" size="xs" title={rt("reconciliation.fx.foreign")}>
        {rt("reconciliation.fx.foreign")}
      </Badge>
      {conv ? (
        <span
          className="text-xs text-fg-muted tabular-nums"
          title={rtp("reconciliation.fx.rateApplied", {
            rate: conv.rate.toFixed(6),
          })}
        >
          {rt("reconciliation.fx.base")}: {formatAmount(conv.base, baseCurrency)}
          {" · "}
          {rt("reconciliation.fx.rateLabel")}: {Number(conv.rate.toFixed(6))}
        </span>
      ) : (
        <span className="text-xs text-danger">
          {rt("reconciliation.fx.noRate")}
        </span>
      )}
    </span>
  );
}
