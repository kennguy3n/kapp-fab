import { forwardRef, type HTMLAttributes, type ReactNode } from "react";
import { cn } from "../lib/cn";

/**
 * StatCard is the KPI widget for dashboards — a labelled metric
 * with a large value, an optional icon, an optional trend
 * indicator, and an optional supporting subtitle.
 *
 * Routing is kept out of the design system the same way Sidebar
 * does it: callers that want the whole card to be a navigation
 * target pass a `renderContainer` render-prop and wrap the KPI
 * content in their router's `<Link>` (or any anchor).  When the
 * prop is omitted StatCard renders a plain, non-interactive
 * `<div>`.  This keeps StatCard router-agnostic while still owning
 * the interactive hover / focus chrome (handed to the render-prop
 * via `className`).
 */
export type StatTrendDirection = "up" | "down" | "flat";
export type StatTrendIntent = "positive" | "negative" | "neutral";

export interface StatTrend {
  direction: StatTrendDirection;
  /** Formatted delta, e.g. "+12%" or "3 fewer". */
  value: ReactNode;
  /**
   * Semantic colour.  Defaults: up → positive, down → negative,
   * flat → neutral.  Override when "down is good" (e.g. AR aging,
   * overdue tickets) by passing `intent="positive"` on a down
   * arrow.
   */
  intent?: StatTrendIntent;
}

export interface StatCardProps extends HTMLAttributes<HTMLDivElement> {
  label: ReactNode;
  value: ReactNode;
  /** Supporting line under the value (e.g. "Pipeline $145,000"). */
  sub?: ReactNode;
  icon?: ReactNode;
  trend?: StatTrend;
  /**
   * Optional wrapper that makes the card a navigation target.  It
   * receives the interactive `className` and the rendered KPI
   * `children`; return e.g. `<Link to={…} className={className}>
   * {children}</Link>`.
   */
  renderContainer?: (args: {
    className: string;
    children: ReactNode;
  }) => ReactNode;
}

function trendIntent(t: StatTrend): StatTrendIntent {
  if (t.intent) return t.intent;
  if (t.direction === "up") return "positive";
  if (t.direction === "down") return "negative";
  return "neutral";
}

const TrendArrow = ({ direction }: { direction: StatTrendDirection }) => (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2.5"
    strokeLinecap="round"
    strokeLinejoin="round"
    className="h-3.5 w-3.5"
    aria-hidden="true"
  >
    {direction === "up" && <polyline points="6 15 12 9 18 15" />}
    {direction === "down" && <polyline points="6 9 12 15 18 9" />}
    {direction === "flat" && <line x1="5" y1="12" x2="19" y2="12" />}
  </svg>
);

const baseClass =
  "block rounded-lg border border-border bg-bg-elevated p-4 text-fg shadow-sm transition-colors";
const interactiveClass =
  "hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)";

export const StatCard = forwardRef<HTMLDivElement, StatCardProps>(
  (
    { className, label, value, sub, icon, trend, renderContainer, ...props },
    ref,
  ) => {
    const intent = trend ? trendIntent(trend) : "neutral";
    const body = (
      <>
        <div className="flex items-start justify-between gap-2">
          <span className="text-sm font-medium text-fg-muted">{label}</span>
          {icon && (
            <span className="text-fg-subtle [&_svg]:h-4 [&_svg]:w-4">
              {icon}
            </span>
          )}
        </div>
        <div className="mt-2 flex items-baseline gap-2">
          <span className="text-2xl font-semibold tracking-tight font-tabular">
            {value}
          </span>
          {trend && (
            <span
              className={cn(
                "inline-flex items-center gap-0.5 text-xs font-medium",
                intent === "positive" && "text-success",
                intent === "negative" && "text-danger",
                intent === "neutral" && "text-fg-subtle",
              )}
            >
              <TrendArrow direction={trend.direction} />
              {trend.value}
            </span>
          )}
        </div>
        {sub && <p className="mt-1 text-xs text-fg-subtle">{sub}</p>}
      </>
    );

    if (renderContainer) {
      return renderContainer({
        className: cn(baseClass, interactiveClass, "group", className),
        children: body,
      });
    }

    return (
      <div ref={ref} className={cn(baseClass, className)} {...props}>
        {body}
      </div>
    );
  },
);
StatCard.displayName = "StatCard";
