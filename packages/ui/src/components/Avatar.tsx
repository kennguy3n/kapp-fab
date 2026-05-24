import {
  forwardRef,
  type ComponentPropsWithoutRef,
  type ElementRef,
} from "react";
import * as AvatarPrimitive from "@radix-ui/react-avatar";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "../lib/cn";

/**
 * Avatar wraps Radix Avatar — the Image / Fallback split is the
 * key feature: if the image fails to load (404, blocked tracker,
 * offline), the Fallback renders instead.  We use this for user
 * avatars where we can't guarantee image availability and want
 * a graceful degradation to initials.
 *
 * Composition:
 *
 *   <Avatar size="md">
 *     <AvatarImage src={user.avatarUrl} alt={user.name} />
 *     <AvatarFallback>{initials(user.name)}</AvatarFallback>
 *   </Avatar>
 *
 * AvatarFallback only renders after the image fails OR a
 * configurable delay passes — so flash-of-fallback during
 * normal image load is suppressed.
 */
const avatarVariants = cva(
  cn(
    "relative flex shrink-0 overflow-hidden rounded-full",
    "bg-bg-muted text-fg-muted",
  ),
  {
    variants: {
      size: {
        xs: "h-5 w-5 text-[10px]",
        sm: "h-7 w-7 text-xs",
        md: "h-9 w-9 text-sm",
        lg: "h-11 w-11 text-base",
        xl: "h-14 w-14 text-lg",
      },
    },
    defaultVariants: { size: "md" },
  },
);

export interface AvatarProps
  extends ComponentPropsWithoutRef<typeof AvatarPrimitive.Root>,
    VariantProps<typeof avatarVariants> {}

export const Avatar = forwardRef<
  ElementRef<typeof AvatarPrimitive.Root>,
  AvatarProps
>(({ className, size, ...props }, ref) => (
  <AvatarPrimitive.Root
    ref={ref}
    className={cn(avatarVariants({ size }), className)}
    {...props}
  />
));
Avatar.displayName = AvatarPrimitive.Root.displayName;

export const AvatarImage = forwardRef<
  ElementRef<typeof AvatarPrimitive.Image>,
  ComponentPropsWithoutRef<typeof AvatarPrimitive.Image>
>(({ className, ...props }, ref) => (
  <AvatarPrimitive.Image
    ref={ref}
    className={cn("aspect-square h-full w-full object-cover", className)}
    {...props}
  />
));
AvatarImage.displayName = AvatarPrimitive.Image.displayName;

export const AvatarFallback = forwardRef<
  ElementRef<typeof AvatarPrimitive.Fallback>,
  ComponentPropsWithoutRef<typeof AvatarPrimitive.Fallback>
>(({ className, ...props }, ref) => (
  <AvatarPrimitive.Fallback
    ref={ref}
    className={cn(
      "flex h-full w-full items-center justify-center font-medium uppercase",
      className,
    )}
    {...props}
  />
));
AvatarFallback.displayName = AvatarPrimitive.Fallback.displayName;

/**
 * `initials(name)` is a utility to derive a 1-2 char fallback
 * label from a person's display name.  Used as the default
 * `<AvatarFallback>` content when the caller hasn't passed
 * explicit text.
 *
 * Algorithm:
 *   - "John Smith"     -> "JS"
 *   - "Alice"          -> "A"
 *   - ""               -> "?"
 *   - "Mary Ann Doe"   -> "MD" (first + last initial only)
 */
export function initials(name: string | null | undefined): string {
  if (!name) return "?";
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0]!.charAt(0).toUpperCase();
  return (
    parts[0]!.charAt(0).toUpperCase() +
    parts[parts.length - 1]!.charAt(0).toUpperCase()
  );
}

export { avatarVariants };
