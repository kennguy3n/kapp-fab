import {
  forwardRef,
  type ComponentPropsWithoutRef,
  type ElementRef,
  type HTMLAttributes,
} from "react";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import { cn } from "../lib/cn";

/**
 * Modal is a styled wrapper over Radix Dialog.  Radix gives us
 * the accessibility hard-parts for free:
 *
 *   - focus trap inside the dialog while open;
 *   - return focus to the trigger on close;
 *   - aria-modal / aria-labelledby / aria-describedby plumbing;
 *   - Escape-to-close and outside-click-to-close (configurable);
 *   - Portal rendering so z-index can't be shadowed by ancestors.
 *
 * We expose Modal / ModalTrigger / ModalContent / ModalHeader /
 * ModalTitle / ModalDescription / ModalFooter / ModalClose
 * subcomponents so the structure matches Card's API surface for
 * consistency.  ModalContent renders the overlay + content in one
 * portal so callers don't need to manage Portal/Overlay manually.
 *
 * Backward-compat: previous @kapp/ui exported a single `<Modal
 * open onClose>` controlled component.  We keep that signature
 * available via the `ControlledModal` named export for the few
 * callers (apps/web/src/components/insights/ShareModal.tsx, etc.)
 * that aren't ready to refactor to the composable API.  New code
 * should use the composable Modal pattern below.
 */

export const Modal = DialogPrimitive.Root;
export const ModalTrigger = DialogPrimitive.Trigger;
export const ModalPortal = DialogPrimitive.Portal;
export const ModalClose = DialogPrimitive.Close;

export const ModalOverlay = forwardRef<
  ElementRef<typeof DialogPrimitive.Overlay>,
  ComponentPropsWithoutRef<typeof DialogPrimitive.Overlay>
>(({ className, ...props }, ref) => (
  <DialogPrimitive.Overlay
    ref={ref}
    className={cn(
      // Full-viewport scrim; semi-transparent so the calling page
      // is still partially visible (modal feels like a layer, not
      // a context switch).  Animation classes target Radix's
      // data-state attribute so the overlay fades with the
      // content rather than popping in.
      "fixed inset-0 z-50 bg-black/40 backdrop-blur-sm",
      "data-[state=open]:animate-in data-[state=open]:fade-in-0",
      "data-[state=closed]:animate-out data-[state=closed]:fade-out-0",
      className,
    )}
    {...props}
  />
));
ModalOverlay.displayName = DialogPrimitive.Overlay.displayName;

export const ModalContent = forwardRef<
  ElementRef<typeof DialogPrimitive.Content>,
  ComponentPropsWithoutRef<typeof DialogPrimitive.Content>
>(({ className, children, ...props }, ref) => (
  <ModalPortal>
    <ModalOverlay />
    <DialogPrimitive.Content
      ref={ref}
      className={cn(
        // Centered card with max-width so it doesn't sprawl on
        // wide viewports.  Inner overflow-y is handled by content
        // — we cap height at 85vh so the modal never extends
        // beyond the viewport and the close button stays
        // reachable.
        "fixed left-[50%] top-[50%] z-50",
        "w-full max-w-lg translate-x-[-50%] translate-y-[-50%]",
        "max-h-[85vh] overflow-y-auto",
        "rounded-lg border border-border bg-bg-elevated text-fg shadow-lg",
        "p-6",
        "data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=open]:zoom-in-95",
        "data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95",
        className,
      )}
      {...props}
    >
      {children}
    </DialogPrimitive.Content>
  </ModalPortal>
));
ModalContent.displayName = DialogPrimitive.Content.displayName;

export const ModalHeader = ({
  className,
  ...props
}: HTMLAttributes<HTMLDivElement>) => (
  <div
    className={cn("flex flex-col gap-1.5 mb-4", className)}
    {...props}
  />
);
ModalHeader.displayName = "ModalHeader";

export const ModalFooter = ({
  className,
  ...props
}: HTMLAttributes<HTMLDivElement>) => (
  <div
    className={cn(
      "mt-6 flex flex-col-reverse gap-2 sm:flex-row sm:justify-end",
      className,
    )}
    {...props}
  />
);
ModalFooter.displayName = "ModalFooter";

export const ModalTitle = forwardRef<
  ElementRef<typeof DialogPrimitive.Title>,
  ComponentPropsWithoutRef<typeof DialogPrimitive.Title>
>(({ className, ...props }, ref) => (
  <DialogPrimitive.Title
    ref={ref}
    className={cn(
      "text-lg font-semibold leading-tight tracking-tight",
      className,
    )}
    {...props}
  />
));
ModalTitle.displayName = DialogPrimitive.Title.displayName;

export const ModalDescription = forwardRef<
  ElementRef<typeof DialogPrimitive.Description>,
  ComponentPropsWithoutRef<typeof DialogPrimitive.Description>
>(({ className, ...props }, ref) => (
  <DialogPrimitive.Description
    ref={ref}
    className={cn("text-sm text-fg-muted", className)}
    {...props}
  />
));
ModalDescription.displayName = DialogPrimitive.Description.displayName;

/**
 * ControlledModal preserves the old open-prop API for callers that
 * haven't migrated.  Internally it wraps the composable API.  Do
 * not add new features here — extend ModalContent instead.
 */
export interface ControlledModalProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "title"> {
  open: boolean;
  onClose: () => void;
  title?: string;
}

export function ControlledModal({
  open,
  onClose,
  title,
  children,
  className,
  ...rest
}: ControlledModalProps) {
  return (
    <Modal
      open={open}
      onOpenChange={(next) => {
        if (!next) onClose();
      }}
    >
      <ModalContent className={className} {...rest}>
        {title && (
          <ModalHeader>
            <ModalTitle>{title}</ModalTitle>
          </ModalHeader>
        )}
        {children}
      </ModalContent>
    </Modal>
  );
}
