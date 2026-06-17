import { Badge } from "@kapp/ui";
import { useFormatter } from "../lib/i18n/useFormatter";
import {
  formatMoney,
  parseDateValue,
  txnData,
  type TransferPairRow,
} from "./reconciliation";

function Leg({
  label,
  accountName,
  description,
  valueDate,
}: {
  label: string;
  accountName: string;
  description?: string;
  valueDate?: string;
}) {
  const f = useFormatter();
  const date = parseDateValue(valueDate);
  return (
    <div className="min-w-0">
      <div className="text-xs uppercase tracking-wide text-fg-muted">
        {label}
      </div>
      <div className="truncate font-medium text-fg">
        {accountName || "(unknown account)"}
      </div>
      <div className="truncate text-xs text-fg-muted">
        {description || "(no description)"}
        {date ? ` · ${f.date(date)}` : ""}
      </div>
    </div>
  );
}

function TransferAmount({
  amount,
  currency,
}: {
  amount: number;
  currency?: string;
}) {
  const f = useFormatter();
  return (
    <div className="text-center font-semibold tabular-nums text-fg">
      {formatMoney(f, amount, currency)}
    </div>
  );
}

/**
 * ReconciliationTransfers surfaces the inter-account transfers the backend
 * detector already paired (it marks both legs status="transfer"). These
 * are internal money movements, not P&L activity, so the operator confirms
 * them as a single line rather than reconciling each leg against a journal
 * entry — the equivalent of Xero's "Transfer" tab.
 */
export function ReconciliationTransfers({
  pairs,
}: {
  pairs: TransferPairRow[];
}) {
  if (pairs.length === 0) return null;
  return (
    <section className="flex flex-col gap-3" aria-label="Detected transfers">
      <header className="flex items-center gap-2">
        <h2 className="text-base font-semibold text-fg">Detected transfers</h2>
        <Badge variant="info" size="xs">
          Auto-paired
        </Badge>
      </header>
      <p className="text-sm text-fg-muted">
        These lines were auto-detected as internal transfers between your own
        accounts and resolved as a single movement.
      </p>
      <ul className="flex list-none flex-col gap-2 p-0">
        {pairs.map((p) => {
          const outD = p.out ? txnData(p.out.txn) : undefined;
          const inD = p.in ? txnData(p.in.txn) : undefined;
          return (
            <li
              key={p.key}
              className="grid grid-cols-1 items-center gap-2 rounded-lg border border-border bg-bg-subtle p-3 sm:grid-cols-[1fr_auto_1fr]"
            >
              {p.out ? (
                <Leg
                  label="From"
                  accountName={p.out.accountName}
                  description={outD?.description}
                  valueDate={outD?.value_date}
                />
              ) : (
                <div className="text-xs text-fg-muted">
                  Counter-leg outside current view
                </div>
              )}
              <TransferAmount amount={p.amount} currency={p.currency} />
              {p.in ? (
                <Leg
                  label="To"
                  accountName={p.in.accountName}
                  description={inD?.description}
                  valueDate={inD?.value_date}
                />
              ) : (
                <div className="text-xs text-fg-muted">
                  Counter-leg outside current view
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
