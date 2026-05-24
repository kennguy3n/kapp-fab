import { forwardRef, type InputHTMLAttributes, type ReactNode } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

const inputVariants = cva(
  cn(
    "flex w-full rounded-md border bg-bg-elevated text-fg",
    "placeholder:text-fg-subtle",
    "transition-colors",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-1 focus-visible:ring-offset-(--bg)",
    "disabled:cursor-not-allowed disabled:opacity-50 disabled:bg-bg-muted",
    "file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-fg",
  ),
  {
    variants: {
      size: {
        sm: "h-7 px-2 text-xs",
        md: "h-9 px-3 text-sm",
        lg: "h-11 px-4 text-base",
      },
      invalid: {
        true: "border-danger focus-visible:ring-(--danger)",
        false: "border-border",
      },
    },
    defaultVariants: {
      size: "md",
      invalid: false,
    },
  },
);

export interface InputProps
  extends Omit<InputHTMLAttributes<HTMLInputElement>, "size">,
    VariantProps<typeof inputVariants> {
  /**
   * Inline element rendered inside the input on the left (e.g.
   * a search icon).  Renders as a non-interactive decoration —
   * use `<button>` directly if the affix needs to be clickable.
   */
  leadingAddon?: ReactNode;
  /**
   * Inline element rendered inside the input on the right (e.g.
   * a clear / show-password button).  Unlike `leadingAddon`,
   * trailing affixes are commonly clickable, so callers may pass
   * a `<button>` directly.
   */
  trailingAddon?: ReactNode;
}

/**
 * Input is the canonical text-input control.  Wraps the native
 * `<input>` so all browser keyboard, IME, autofill, and
 * accessibility behaviour passes through unchanged — we only style
 * and provide consistent slot conventions.
 *
 * When `leadingAddon` or `trailingAddon` is set, the component
 * renders a positioned wrapper so the addons overlay the input's
 * padding area.  Padding is shifted to make room for the addon
 * without overlapping the typed text.
 */
export const Input = forwardRef<HTMLInputElement, InputProps>(
  (
    { className, size, invalid, leadingAddon, trailingAddon, ...props },
    ref,
  ) => {
    if (!leadingAddon && !trailingAddon) {
      return (
        <input
          ref={ref}
          className={cn(inputVariants({ size, invalid }), className)}
          aria-invalid={invalid || undefined}
          {...props}
        />
      );
    }
    return (
      <div className="relative w-full">
        {leadingAddon && (
          <span className="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-2.5 text-fg-subtle">
            {leadingAddon}
          </span>
        )}
        <input
          ref={ref}
          className={cn(
            inputVariants({ size, invalid }),
            leadingAddon && "pl-9",
            trailingAddon && "pr-9",
            className,
          )}
          aria-invalid={invalid || undefined}
          {...props}
        />
        {trailingAddon && (
          <span className="absolute inset-y-0 right-0 flex items-center pr-2.5 text-fg-subtle">
            {trailingAddon}
          </span>
        )}
      </div>
    );
  },
);
Input.displayName = "Input";

export { inputVariants };
