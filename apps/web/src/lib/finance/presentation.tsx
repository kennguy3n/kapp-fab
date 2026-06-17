import type { ReactNode } from "react";
import { AlertTriangle, CheckCircle2, RotateCw } from "lucide-react";
import type { AccountType } from "@kapp/client";
import type { BadgeProps } from "@kapp/ui";
import {
  Badge,
  Button,
  EmptyState,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableRow,
} from "@kapp/ui";
import { humanizeToken, statusVariant } from "../ktypeView";

export type BadgeVariant = NonNullable<BadgeProps["variant"]>;

// Semantic colour mapping for the five chart-of-accounts classes.
// Each type gets a distinct, stable variant so the category is
// readable from colour alone (rule: machine enum tokens never reach
// the user as bare lowercase text).
const ACCOUNT_TYPE_VARIANT: Record<AccountType, BadgeVariant> = {
  asset: "info",
  liability: "warning",
  equity: "accent",
  revenue: "success",
  expense: "neutral",
};

/** Title-cased label for a chart-of-accounts type token. */
export function accountTypeLabel(type: string): string {
  return humanizeToken(type);
}

/** Renders an account type (asset/liability/…) as a semantic Badge. */
export function AccountTypeBadge({
  type,
  className,
}: {
  type: AccountType;
  className?: string;
}) {
  return (
    <Badge variant={ACCOUNT_TYPE_VARIANT[type] ?? "neutral"} className={className}>
      {accountTypeLabel(type)}
    </Badge>
  );
}

/** Renders a KRecord status token as a Badge using the shared mapping. */
export function StatusBadge({ status }: { status: string }) {
  return <Badge variant={statusVariant(status)}>{humanizeToken(status)}</Badge>;
}

/**
 * Balanced / out-of-balance pill used by the trial balance, journal
 * entries, and chart-of-accounts integrity checks. Carries an icon so
 * the state is legible without relying on colour alone (a11y).
 */
export function BalancedBadge({
  balanced,
  balancedLabel = "Balanced",
  unbalancedLabel = "Out of balance",
}: {
  balanced: boolean;
  balancedLabel?: string;
  unbalancedLabel?: string;
}) {
  return balanced ? (
    <Badge variant="success">
      <CheckCircle2 aria-hidden className="h-3.5 w-3.5" />
      {balancedLabel}
    </Badge>
  ) : (
    <Badge variant="danger">
      <AlertTriangle aria-hidden className="h-3.5 w-3.5" />
      {unbalancedLabel}
    </Badge>
  );
}

/**
 * Standard error surface for a failed finance query: an icon, the
 * server message, and a Retry action that re-runs the query.
 */
export function FinanceError({
  title = "Couldn't load this report",
  error,
  onRetry,
}: {
  title?: string;
  error: unknown;
  onRetry: () => void;
}) {
  const message =
    error instanceof Error ? error.message : "An unexpected error occurred.";
  return (
    <EmptyState
      icon={<AlertTriangle aria-hidden />}
      title={title}
      description={message}
      action={
        <Button
          variant="secondary"
          leadingIcon={<RotateCw aria-hidden />}
          onClick={onRetry}
        >
          Try again
        </Button>
      }
    />
  );
}

/**
 * Skeleton placeholder shaped like the table that will replace it, so
 * the layout doesn't reflow when data arrives.
 */
export function TableSkeleton({
  rows = 6,
  columns = 4,
}: {
  rows?: number;
  columns?: number;
}) {
  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <div className="flex gap-4 border-b border-border bg-bg-subtle px-3 py-2.5">
        {Array.from({ length: columns }).map((_, i) => (
          <Skeleton key={i} variant="text" className="h-3.5 flex-1" />
        ))}
      </div>
      <div className="divide-y divide-border">
        {Array.from({ length: rows }).map((_, r) => (
          <div key={r} className="flex gap-4 px-3 py-3">
            {Array.from({ length: columns }).map((_, c) => (
              <Skeleton key={c} variant="text" className="h-4 flex-1" />
            ))}
          </div>
        ))}
      </div>
    </div>
  );
}

/** A single full-width "no data" row inside an existing table body. */
export function TableEmptyRow({
  colSpan,
  children,
}: {
  colSpan: number;
  children: ReactNode;
}) {
  return (
    <TableRow>
      <TableCell colSpan={colSpan} className="py-8 text-center text-fg-muted">
        {children}
      </TableCell>
    </TableRow>
  );
}

// Re-export Table primitives consumers commonly need alongside the
// helpers above so a finance page imports its table scaffolding from a
// single module.
export { Table, TableBody };
