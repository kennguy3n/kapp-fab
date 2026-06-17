import { forwardRef, type TextareaHTMLAttributes } from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Textarea wraps the native `<textarea>` with the same chrome as
 * Input/Select so multi-line controls sit consistently inside record
 * forms.  Like the other form controls it stays `rounded-md` (NOT a
 * pill — pills are for buttons/badges) and exposes the shared
 * `invalid` variant so a Field can flag validation errors.
 */
const textareaVariants = cva(
  cn(
    "flex w-full rounded-md border bg-bg-elevated text-fg",
    "px-3 py-2 text-sm",
    "placeholder:text-fg-subtle",
    "transition-colors",
    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-1 focus-visible:ring-offset-(--bg)",
    "disabled:cursor-not-allowed disabled:opacity-50 disabled:bg-bg-muted",
  ),
  {
    variants: {
      invalid: {
        true: "border-danger focus-visible:ring-(--danger)",
        false: "border-border",
      },
    },
    defaultVariants: {
      invalid: false,
    },
  },
);

export interface TextareaProps
  extends TextareaHTMLAttributes<HTMLTextAreaElement>,
    VariantProps<typeof textareaVariants> {}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
  ({ className, invalid, ...props }, ref) => (
    <textarea
      ref={ref}
      className={cn(textareaVariants({ invalid }), className)}
      aria-invalid={invalid || undefined}
      {...props}
    />
  ),
);
Textarea.displayName = "Textarea";

export { textareaVariants };
