import { forwardRef, type HTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Spinner is the canonical indeterminate loading indicator.  It
 * replaces the ad-hoc `animate-spin rounded-full border` markup
 * that was copy-pasted into route fallbacks (ShellRouteFallback,
 * PublicRouteFallback) and a handful of pages.  Rendering it as a
 * single primitive means the spin timing, border treatment, and
 * the `role="status"` / `aria-live` accessibility plumbing live in
 * one place.
 *
 * The visible ring is a CSS border with one transparent edge
 * (`border-r-transparent`) rotated by `animate-spin`; `currentColor`
 * drives the ring colour so callers can recolour via `text-*`
 * utilities without touching the component.
 */
const spinnerVariants = cva(
  cn(
    "inline-block animate-spin rounded-full border-current border-r-transparent",
    "motion-reduce:animate-[spin_1.5s_linear_infinite]",
  ),
  {
    variants: {
      size: {
        xs: "h-3 w-3 border-2",
        sm: "h-4 w-4 border-2",
        md: "h-6 w-6 border-2",
        lg: "h-8 w-8 border-[3px]",
        xl: "h-12 w-12 border-4",
      },
    },
    defaultVariants: {
      size: "md",
    },
  },
);

export interface SpinnerProps
  extends HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof spinnerVariants> {
  /**
   * Accessible label announced to assistive tech.  Defaults to
   * "Loading…".  Rendered as a visually-hidden span so sighted
   * users see only the ring while screen readers get the text.
   */
  label?: string;
}

export const Spinner = forwardRef<HTMLDivElement, SpinnerProps>(
  ({ className, size, label = "Loading…", ...props }, ref) => (
    <div
      ref={ref}
      role="status"
      aria-live="polite"
      className={cn("inline-flex items-center justify-center", className)}
      {...props}
    >
      <span className={spinnerVariants({ size })} aria-hidden="true" />
      <span className="sr-only">{label}</span>
    </div>
  ),
);
Spinner.displayName = "Spinner";

export { spinnerVariants };
