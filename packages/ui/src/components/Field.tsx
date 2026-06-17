import {
  cloneElement,
  forwardRef,
  useId,
  type HTMLAttributes,
  type ReactElement,
  type ReactNode,
} from "react";
import { cn } from "../lib/cn";

/**
 * Field is the single labelled form-row wrapper every record form
 * builds on: it renders a label, the control, an optional help line,
 * and an error message, and wires the accessibility relationships
 * between them so call sites don't repeat that boilerplate.
 *
 * The control is passed as the single child element (Input / Select /
 * Textarea).  Field clones it to inject:
 *   - `id` (so the `<label htmlFor>` targets it) — unless the child
 *     already sets one,
 *   - `aria-describedby` pointing at the help/error text,
 *   - `aria-required` when `required`,
 *   - the `invalid` state + `aria-invalid` when `error` is set.
 *
 * Controls keep their own `rounded-md` chrome — Field never makes a
 * control a pill.  Error text replaces help text when both are set.
 */
export interface FieldProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "children"> {
  /** Visible label text (or node). */
  label: ReactNode;
  /** The form control element — Input, Select, Textarea, etc. */
  children: ReactElement<Record<string, unknown>>;
  /** Override the generated control id (rarely needed). */
  htmlFor?: string;
  /** Show the required marker and set `aria-required` on the control. */
  required?: boolean;
  /** Validation message — when set, the control renders its invalid state. */
  error?: ReactNode;
  /** Supplementary hint shown below the control when there's no error. */
  help?: ReactNode;
  /** Visually hide the label while keeping it for assistive tech. */
  hideLabel?: boolean;
}

export const Field = forwardRef<HTMLDivElement, FieldProps>(
  (
    { className, label, children, htmlFor, required, error, help, hideLabel, ...props },
    ref,
  ) => {
    const reactId = useId();
    const childProps = children.props;
    const childId = typeof childProps.id === "string" ? childProps.id : undefined;
    const childDescribedBy =
      typeof childProps["aria-describedby"] === "string"
        ? childProps["aria-describedby"]
        : undefined;

    const controlId = htmlFor ?? childId ?? `field-${reactId}`;
    const messageId = error
      ? `${controlId}-error`
      : help
        ? `${controlId}-help`
        : undefined;
    const describedBy =
      [childDescribedBy, messageId].filter(Boolean).join(" ") || undefined;

    const injected: Record<string, unknown> = {
      id: controlId,
      "aria-describedby": describedBy,
    };
    if (required) injected["aria-required"] = true;
    if (error) {
      injected.invalid = true;
      injected["aria-invalid"] = true;
    }
    const control = cloneElement(children, injected);

    return (
      <div ref={ref} className={cn("flex flex-col gap-1.5", className)} {...props}>
        <label
          htmlFor={controlId}
          className={cn(
            "text-sm font-medium text-fg",
            hideLabel && "sr-only",
          )}
        >
          {label}
          {required && (
            <span aria-hidden="true" className="ms-0.5 text-danger">
              *
            </span>
          )}
        </label>
        {control}
        {error ? (
          <p id={messageId} className="text-xs text-danger">
            {error}
          </p>
        ) : help ? (
          <p id={messageId} className="text-xs text-fg-muted">
            {help}
          </p>
        ) : null}
      </div>
    );
  },
);
Field.displayName = "Field";
