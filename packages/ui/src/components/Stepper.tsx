import { forwardRef, type HTMLAttributes, type ReactNode } from "react";
import { cn } from "../lib/cn";

/**
 * Stepper is the horizontal multi-step progress indicator for
 * wizards and other linear flows (e.g. the tenant SetupWizard).
 * Each step renders a numbered (or icon) marker plus a label, and
 * a connector line to the next step.  The three states —
 * completed, active, upcoming — are derived from `current` so
 * callers only track a single index.
 *
 * Accessibility: the list is an ordered `<ol>`; the active step
 * carries `aria-current="step"`; completed markers render a
 * checkmark.  Connector lines are decorative (`aria-hidden`).
 */
export interface StepperStep {
  /** Visible label beneath / beside the marker. */
  label: ReactNode;
  /** Optional secondary description line. */
  description?: ReactNode;
  /** Optional icon shown inside the marker instead of the number. */
  icon?: ReactNode;
}

export interface StepperProps extends HTMLAttributes<HTMLOListElement> {
  steps: StepperStep[];
  /** Zero-based index of the active step. */
  current: number;
}

const CheckIcon = () => (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="3"
    strokeLinecap="round"
    strokeLinejoin="round"
    className="h-4 w-4"
    aria-hidden="true"
  >
    <polyline points="20 6 9 17 4 12" />
  </svg>
);

export const Stepper = forwardRef<HTMLOListElement, StepperProps>(
  ({ className, steps, current, ...props }, ref) => (
    <ol
      ref={ref}
      className={cn("flex w-full items-start", className)}
      {...props}
    >
      {steps.map((step, i) => {
        const completed = i < current;
        const active = i === current;
        const isLast = i === steps.length - 1;
        return (
          <li
            key={i}
            className={cn(
              "flex items-start gap-0",
              !isLast && "flex-1",
            )}
            aria-current={active ? "step" : undefined}
          >
            <div className="flex flex-col items-center">
              <span
                className={cn(
                  "flex h-8 w-8 shrink-0 items-center justify-center rounded-full border text-sm font-medium transition-colors",
                  completed && "border-accent bg-accent text-accent-fg",
                  active &&
                    "border-accent bg-bg-elevated text-accent ring-2 ring-(--focus-ring)",
                  !completed &&
                    !active &&
                    "border-border bg-bg-elevated text-fg-subtle",
                )}
              >
                {completed ? (
                  <CheckIcon />
                ) : step.icon ? (
                  <span className="[&_svg]:h-4 [&_svg]:w-4">{step.icon}</span>
                ) : (
                  i + 1
                )}
              </span>
              <span className="mt-1.5 flex flex-col items-center text-center">
                <span
                  className={cn(
                    "text-xs font-medium",
                    active ? "text-fg" : "text-fg-muted",
                  )}
                >
                  {step.label}
                </span>
                {step.description && (
                  <span className="text-[11px] text-fg-subtle">
                    {step.description}
                  </span>
                )}
              </span>
            </div>
            {!isLast && (
              <span
                aria-hidden="true"
                className={cn(
                  "mt-4 h-0.5 flex-1 rounded-full transition-colors",
                  completed ? "bg-accent" : "bg-border",
                )}
              />
            )}
          </li>
        );
      })}
    </ol>
  ),
);
Stepper.displayName = "Stepper";
