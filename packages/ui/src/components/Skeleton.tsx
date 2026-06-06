import { forwardRef, type HTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Skeleton is the loading-placeholder primitive.  It renders a
 * muted, pulsing block sized to approximate the content that will
 * replace it, so the layout doesn't reflow when data arrives.
 *
 * Three shape variants cover the common cases:
 *   - rect:   a rounded rectangle (cards, images, buttons).
 *   - circle: a perfect circle (avatars, icon placeholders).
 *   - text:   a short, rounded bar at text line-height (labels,
 *             headings, single lines of copy).
 *
 * The pulse uses Tailwind's `animate-pulse`; `motion-reduce`
 * disables it for users who've asked the OS to limit motion.  The
 * fill is the `bg-bg-muted` design token so the placeholder tracks
 * light/dark theme automatically.
 */
const skeletonVariants = cva(
  cn("block bg-bg-muted animate-pulse motion-reduce:animate-none"),
  {
    variants: {
      variant: {
        rect: "rounded-md",
        circle: "rounded-full",
        text: "rounded h-4 my-0.5",
      },
    },
    defaultVariants: {
      variant: "rect",
    },
  },
);

export interface SkeletonProps
  extends HTMLAttributes<HTMLDivElement>,
    VariantProps<typeof skeletonVariants> {}

export const Skeleton = forwardRef<HTMLDivElement, SkeletonProps>(
  ({ className, variant, ...props }, ref) => (
    <div
      ref={ref}
      aria-hidden="true"
      className={cn(skeletonVariants({ variant }), className)}
      {...props}
    />
  ),
);
Skeleton.displayName = "Skeleton";

export { skeletonVariants };
