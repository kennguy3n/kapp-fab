import { forwardRef, type HTMLAttributes } from "react";
import { cn } from "../lib/cn";

/**
 * Eyebrow is the KChat brand motif — a small monospace label with a
 * leading underscore (e.g. `_Communities`, `_Coming soon`) that sits
 * above a heading or section title to categorise it.
 *
 * The underscore is rendered as a decorative, `aria-hidden` prefix so
 * the accessibility tree reads the plain label ("Communities") rather
 * than the punctuation — callers therefore pass the bare word and the
 * component owns the `_` glyph, the monospace face, and the accent
 * colour.  Override the colour (or any class) via `className`.
 */
export interface EyebrowProps extends HTMLAttributes<HTMLSpanElement> {}

export const Eyebrow = forwardRef<HTMLSpanElement, EyebrowProps>(
  ({ className, children, ...props }, ref) => (
    <span
      ref={ref}
      className={cn(
        "inline-flex items-center font-mono text-xs font-medium tracking-wide text-accent",
        className,
      )}
      {...props}
    >
      <span aria-hidden="true" className="select-none">
        _
      </span>
      {children}
    </span>
  ),
);
Eyebrow.displayName = "Eyebrow";
