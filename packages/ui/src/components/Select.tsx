import { forwardRef, type SelectHTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Select wraps the native `<select>`.  The reason for using native
 * over a Radix-Select primitive is twofold:
 *
 *   1. Native `<select>` has perfect IME / keyboard / mobile picker
 *      behaviour out of the box.  Radix Select is excellent but
 *      it's a popover-driven listbox that doesn't get OS-level
 *      treatment on mobile (where users expect the wheel picker).
 *   2. Native `<select>` participates in HTML form submission and
 *      autofill without any extra wiring — useful inside record
 *      forms where the form payload is read directly from the DOM.
 *
 * Custom dropdowns (search-filtered, multi-select, async loaders)
 * should be built as separate components (Combobox, MultiSelect)
 * — Select is the simple-case primitive only.
 */
const selectVariants = cva(
  cn(
    // Use the same chrome as Input so paired controls look unified.
    "flex w-full appearance-none rounded-md border bg-bg-elevated text-fg",
    "transition-colors",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-1 focus-visible:ring-offset-(--bg)",
    "disabled:cursor-not-allowed disabled:opacity-50 disabled:bg-bg-muted",
    // Caret affordance — pad on the inline-end and overlay a
    // chevron via background-image so the dropdown indicator
    // survives the appearance-none reset.  data: URL keeps it
    // self-contained. RTL: background-position swaps from the
    // physical "right" anchor to "left" so the chevron sits at
    // the inline-end of the control in both writing directions,
    // and the inline-end padding (pe-8) follows the chevron
    // automatically.
    "bg-no-repeat bg-[length:1rem_1rem] bg-[position:right_0.5rem_center] rtl:bg-[position:left_0.5rem_center]",
    "[background-image:url(\"data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20viewBox='0%200%2024%2024'%20fill='none'%20stroke='currentColor'%20stroke-width='2'%20stroke-linecap='round'%20stroke-linejoin='round'%3E%3Cpolyline%20points='6%209%2012%2015%2018%209'%3E%3C/polyline%3E%3C/svg%3E\")]",
    "pe-8",
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

export interface SelectProps
  extends Omit<SelectHTMLAttributes<HTMLSelectElement>, "size">,
    VariantProps<typeof selectVariants> {}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(
  ({ className, size, invalid, children, ...props }, ref) => (
    <select
      ref={ref}
      className={cn(selectVariants({ size, invalid }), className)}
      aria-invalid={invalid || undefined}
      {...props}
    >
      {children}
    </select>
  ),
);
Select.displayName = "Select";

export { selectVariants };
