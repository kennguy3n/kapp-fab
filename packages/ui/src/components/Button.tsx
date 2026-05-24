import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Button variants live in a class-variance-authority spec so the
 * variant axes (visual style + size) are type-checked at use sites
 * and Storybook / tooling can introspect the variant matrix.
 *
 * Visual variants:
 *   - primary:   filled accent — the canonical "save / submit" CTA.
 *   - secondary: filled muted background — co-equal action when
 *                a page has two important buttons (e.g. "Save"
 *                and "Save & Add Another").
 *   - outline:   bordered, transparent fill — used as a tertiary
 *                CTA or in destructive-confirmation dialogs where
 *                accent fill would feel too aggressive.
 *   - ghost:     no border, no fill — for inline / toolbar
 *                actions where button chrome would crowd the UI.
 *   - link:      underlined text that lives in body copy — semantic
 *                button but visually a link.
 *   - destructive: filled danger — for delete / archive actions
 *                where the user must be visually warned.
 *
 * Size axis is independent — any visual variant pairs with any size.
 */
const buttonVariants = cva(
  // Base class set applied to every variant.  Includes the focus
  // ring (managed via :focus-visible globally in globals.css plus
  // a per-control ring colour for high-contrast vs disabled state)
  // and the disabled-state pointer/opacity treatment so callers
  // never have to set `disabled:opacity-50` themselves.
  cn(
    "inline-flex items-center justify-center gap-2",
    "whitespace-nowrap rounded-md font-medium",
    "transition-colors duration-150",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)",
    "disabled:pointer-events-none disabled:opacity-50",
  ),
  {
    variants: {
      variant: {
        primary: cn(
          "bg-accent text-accent-fg",
          "hover:bg-accent-hover active:bg-accent-hover",
        ),
        secondary: cn(
          "bg-bg-muted text-fg",
          "hover:bg-bg-subtle active:bg-bg-muted",
        ),
        outline: cn(
          "border border-border bg-transparent text-fg",
          "hover:bg-bg-subtle active:bg-bg-muted",
        ),
        ghost: cn(
          "bg-transparent text-fg",
          "hover:bg-bg-subtle active:bg-bg-muted",
        ),
        link: cn(
          "bg-transparent text-accent underline-offset-4",
          "hover:underline",
        ),
        destructive: cn(
          "bg-danger text-danger-fg",
          "hover:opacity-90 active:opacity-95",
        ),
      },
      size: {
        sm: "h-7 px-2.5 text-xs",
        md: "h-9 px-3 text-sm",
        lg: "h-11 px-5 text-base",
        icon: "h-9 w-9 p-0",
      },
    },
    defaultVariants: {
      variant: "primary",
      size: "md",
    },
  },
);

export interface ButtonProps
  extends ButtonHTMLAttributes<HTMLButtonElement>,
    VariantProps<typeof buttonVariants> {
  /**
   * When `asChild` is true, Button renders its child as the
   * underlying element (via Radix Slot) and forwards all styling
   * + handlers + ARIA into it.  This is how you compose a Button
   * around a React-Router `<Link>` without losing the button
   * variant styling or the focus-ring + disabled handling.
   *
   * Caller must pass exactly one child element (Slot does not
   * support multiple children).  Type system can't enforce this
   * at compile time, but Radix surfaces a clear runtime error
   * if violated.
   *
   * **Mutually exclusive with `leadingIcon` / `trailingIcon`** —
   * combining them would require wrapping the content in a
   * Fragment, which Radix Slot cannot forward props onto.  Embed
   * the icon inside your child element instead.  Violations
   * throw at render time with a clear message rather than
   * silently dropping styles.
   */
  asChild?: boolean;
  /**
   * Optional icon rendered before the text label.  Pass any
   * React node (typically a lucide-react icon).  Styled with
   * a fixed 16px slot so labels stay vertically aligned across
   * leading/trailing combinations.
   */
  leadingIcon?: ReactNode;
  /**
   * Optional icon rendered after the text label.  Same sizing
   * contract as `leadingIcon`.
   */
  trailingIcon?: ReactNode;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  (
    {
      className,
      variant,
      size,
      asChild = false,
      leadingIcon,
      trailingIcon,
      children,
      ...props
    },
    ref,
  ) => {
    // `asChild` and `leadingIcon`/`trailingIcon` are mutually
    // exclusive by design.  Radix Slot clones its single child
    // element to merge styles, refs, and handlers in; a
    // Fragment-wrapped child (which is what we'd need to render
    // icons + a custom inner element) is NOT a single element and
    // Slot silently drops the merged props on it.  Rather than
    // hide that footgun behind a sometimes-broken render path,
    // we surface the conflict at the API boundary and force the
    // caller to embed the icon inside their `<Link>` / etc. when
    // they want both behaviours.  This matches how shadcn/ui
    // composes Button with NavLink in real apps.
    if (asChild && (leadingIcon || trailingIcon)) {
      throw new Error(
        "<Button>: `asChild` cannot be combined with `leadingIcon` or " +
          "`trailingIcon` — Radix Slot can only forward props onto a " +
          "single child element. Embed the icon inside the child " +
          "element (e.g. inside the <Link> you pass as children) " +
          "instead, or remove `asChild`.",
      );
    }
    const Comp = asChild ? Slot : "button";
    const content =
      leadingIcon || trailingIcon ? (
        <>
          {leadingIcon && (
            <span className="inline-flex h-4 w-4 items-center justify-center">
              {leadingIcon}
            </span>
          )}
          {children}
          {trailingIcon && (
            <span className="inline-flex h-4 w-4 items-center justify-center">
              {trailingIcon}
            </span>
          )}
        </>
      ) : (
        children
      );
    return (
      <Comp
        ref={ref}
        className={cn(buttonVariants({ variant, size }), className)}
        {...props}
      >
        {content}
      </Comp>
    );
  },
);
Button.displayName = "Button";

export { buttonVariants };
