import { forwardRef, type HTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Badge is the small-label primitive — used for status pills,
 * count badges, category tags, etc.  Variants map to the
 * semantic colour scale (default / accent / success / warning /
 * danger / info) so the meaning of a badge is carried by colour
 * even before the label is read.
 *
 * Sizes are kept tight (xs / sm / md) — badges aren't meant to
 * be tap targets, just visual signals.  When you need an
 * interactive pill, render a `<Button size="sm" variant="outline">`
 * instead.
 */
const badgeVariants = cva(
  cn(
    "inline-flex items-center gap-1 rounded-full border px-2 py-0.5",
    "text-xs font-medium",
    "whitespace-nowrap",
  ),
  {
    variants: {
      variant: {
        default: "border-border bg-bg-muted text-fg",
        accent: "border-transparent bg-accent text-accent-fg",
        success: "border-transparent bg-success text-success-fg",
        warning: "border-transparent bg-warning text-warning-fg",
        danger: "border-transparent bg-danger text-danger-fg",
        info: "border-transparent bg-info text-info-fg",
        outline: "border-border bg-transparent text-fg",
      },
      size: {
        xs: "px-1.5 py-0 text-[10px]",
        sm: "px-2 py-0.5 text-xs",
        md: "px-2.5 py-1 text-sm",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "sm",
    },
  },
);

export interface BadgeProps
  extends HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badgeVariants> {}

export const Badge = forwardRef<HTMLSpanElement, BadgeProps>(
  ({ className, variant, size, ...props }, ref) => (
    <span
      ref={ref}
      className={cn(badgeVariants({ variant, size }), className)}
      {...props}
    />
  ),
);
Badge.displayName = "Badge";

export { badgeVariants };
