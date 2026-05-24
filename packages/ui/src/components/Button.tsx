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
    const Comp = asChild ? Slot : "button";
    // When asChild is set the child becomes the button — but we
    // still want the icons to render around the child's text.  Slot
    // expects exactly one child, so wrap the icon-aware content in
    // a Fragment when icons are present; the resulting "single
    // child" is the icon-wrapping span (or just the text if no
    // icons).  Without this branch, passing `asChild` plus
    // `leadingIcon` would trigger Radix's multi-child warning.
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
