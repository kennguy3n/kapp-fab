import { forwardRef, type HTMLAttributes, type ReactNode } from "react";
import { cn } from "../lib/cn";

/**
 * EmptyState is the first-class "nothing here" surface required by
 * ARCHITECTURE.md §9.8 ("Error and empty states are first-class").
 * It centers an optional icon, a title, an optional description,
 * and an optional action slot (typically a Button) so every list /
 * dashboard / search view has a consistent, intentional zero-data
 * design instead of a bare "No results" string.
 *
 * It is intentionally presentational and router-agnostic: callers
 * pass whatever action node they need (a Button, a Link-as-Button,
 * a retry handler) via the `action` slot.
 */
export interface EmptyStateProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "title"> {
  /** Decorative icon rendered in a muted circle above the title. */
  icon?: ReactNode;
  /** Primary message — what's empty and, ideally, why. */
  title: ReactNode;
  /** Optional supporting copy beneath the title. */
  description?: ReactNode;
  /** Optional call-to-action (e.g. a Button) rendered below copy. */
  action?: ReactNode;
}

export const EmptyState = forwardRef<HTMLDivElement, EmptyStateProps>(
  ({ className, icon, title, description, action, ...props }, ref) => (
    <div
      ref={ref}
      className={cn(
        "flex flex-col items-center justify-center text-center",
        "gap-3 px-6 py-12",
        className,
      )}
      {...props}
    >
      {icon && (
        <div className="flex h-12 w-12 items-center justify-center rounded-full bg-bg-muted text-fg-muted [&_svg]:h-6 [&_svg]:w-6">
          {icon}
        </div>
      )}
      <div className="flex flex-col gap-1">
        <h3 className="text-base font-semibold text-fg">{title}</h3>
        {description && (
          <p className="mx-auto max-w-sm text-sm text-fg-muted">
            {description}
          </p>
        )}
      </div>
      {action && <div className="mt-1">{action}</div>}
    </div>
  ),
);
EmptyState.displayName = "EmptyState";
