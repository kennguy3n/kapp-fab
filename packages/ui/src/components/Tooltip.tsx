import {
  forwardRef,
  type ComponentPropsWithoutRef,
  type ElementRef,
} from "react";
import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import { cn } from "../lib/cn";

/**
 * Tooltip wraps Radix Tooltip.  Radix gives us:
 *
 *   - hover + focus open semantics (so keyboard users see
 *     tooltips when tabbing through buttons, not just mouse
 *     users);
 *   - delay-group behaviour: hovering one tooltip "primes" the
 *     group so subsequent tooltips in the same provider open
 *     immediately (good for icon-button toolbars);
 *   - aria-describedby plumbing so screen readers announce
 *     the tooltip content when the trigger is focused.
 *
 * Usage pattern:
 *
 *   <TooltipProvider>
 *     <Tooltip>
 *       <TooltipTrigger asChild>
 *         <Button size="icon"><EditIcon/></Button>
 *       </TooltipTrigger>
 *       <TooltipContent>Edit record</TooltipContent>
 *     </Tooltip>
 *   </TooltipProvider>
 *
 * `TooltipProvider` should be mounted once at the app root so
 * the delay-group state is shared across all tooltips.
 */
export const TooltipProvider = TooltipPrimitive.Provider;
export const Tooltip = TooltipPrimitive.Root;
export const TooltipTrigger = TooltipPrimitive.Trigger;

export const TooltipContent = forwardRef<
  ElementRef<typeof TooltipPrimitive.Content>,
  ComponentPropsWithoutRef<typeof TooltipPrimitive.Content>
>(({ className, sideOffset = 4, ...props }, ref) => (
  <TooltipPrimitive.Portal>
    <TooltipPrimitive.Content
      ref={ref}
      sideOffset={sideOffset}
      className={cn(
        "z-50 overflow-hidden rounded-md border border-border bg-bg-elevated px-2.5 py-1.5",
        "text-xs text-fg shadow-md",
        "animate-in fade-in-0 zoom-in-95",
        "data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95",
        "data-[side=bottom]:slide-in-from-top-2",
        "data-[side=left]:slide-in-from-right-2",
        "data-[side=right]:slide-in-from-left-2",
        "data-[side=top]:slide-in-from-bottom-2",
        className,
      )}
      {...props}
    />
  </TooltipPrimitive.Portal>
));
TooltipContent.displayName = TooltipPrimitive.Content.displayName;
