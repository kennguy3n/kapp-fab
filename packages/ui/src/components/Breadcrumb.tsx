import {
  forwardRef,
  type AnchorHTMLAttributes,
  type HTMLAttributes,
  type LiHTMLAttributes,
  type ReactNode,
} from "react";
import { Slot } from "@radix-ui/react-slot";
import { cn } from "../lib/cn";

/**
 * Breadcrumb is a composable, router-agnostic trail primitive — the
 * same design philosophy as Sidebar: the design system owns the
 * chrome (separators, muted vs current styling, ARIA) while the
 * host app supplies navigation via `asChild` so a react-router
 * `<Link>` (or any anchor) drives the actual navigation.
 *
 * Structure:
 *   <Breadcrumb>                     // <nav aria-label="Breadcrumb">
 *     <BreadcrumbList>               // <ol>
 *       <BreadcrumbItem>             // <li>
 *         <BreadcrumbLink asChild>   // <a> or Slot'd <Link>
 *       <BreadcrumbSeparator />      // <li aria-hidden>
 *       <BreadcrumbItem>
 *         <BreadcrumbPage>           // current page, aria-current
 */
export const Breadcrumb = forwardRef<
  HTMLElement,
  HTMLAttributes<HTMLElement> & { separator?: ReactNode }
>(({ className, ...props }, ref) => (
  <nav
    ref={ref}
    aria-label="Breadcrumb"
    className={cn("text-sm", className)}
    {...props}
  />
));
Breadcrumb.displayName = "Breadcrumb";

export const BreadcrumbList = forwardRef<
  HTMLOListElement,
  HTMLAttributes<HTMLOListElement>
>(({ className, ...props }, ref) => (
  <ol
    ref={ref}
    className={cn(
      "flex flex-wrap items-center gap-1.5 break-words text-fg-muted",
      className,
    )}
    {...props}
  />
));
BreadcrumbList.displayName = "BreadcrumbList";

export const BreadcrumbItem = forwardRef<
  HTMLLIElement,
  LiHTMLAttributes<HTMLLIElement>
>(({ className, ...props }, ref) => (
  <li
    ref={ref}
    className={cn("inline-flex items-center gap-1.5", className)}
    {...props}
  />
));
BreadcrumbItem.displayName = "BreadcrumbItem";

export interface BreadcrumbLinkProps
  extends AnchorHTMLAttributes<HTMLAnchorElement> {
  asChild?: boolean;
}

export const BreadcrumbLink = forwardRef<
  HTMLAnchorElement,
  BreadcrumbLinkProps
>(({ className, asChild = false, ...props }, ref) => {
  const Comp = asChild ? Slot : "a";
  return (
    <Comp
      ref={ref}
      className={cn(
        "rounded-sm transition-colors hover:text-fg",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
        className,
      )}
      {...props}
    />
  );
});
BreadcrumbLink.displayName = "BreadcrumbLink";

export const BreadcrumbPage = forwardRef<
  HTMLSpanElement,
  HTMLAttributes<HTMLSpanElement>
>(({ className, ...props }, ref) => (
  <span
    ref={ref}
    role="link"
    aria-disabled="true"
    aria-current="page"
    className={cn("font-medium text-fg", className)}
    {...props}
  />
));
BreadcrumbPage.displayName = "BreadcrumbPage";

/**
 * BreadcrumbSeparator defaults to a chevron but accepts children to
 * override (e.g. a slash).  It's `aria-hidden` and presentational so
 * screen readers announce only the items, not the glyphs between.
 */
export const BreadcrumbSeparator = ({
  children,
  className,
  ...props
}: LiHTMLAttributes<HTMLLIElement>) => (
  <li
    role="presentation"
    aria-hidden="true"
    className={cn("[&>svg]:h-3.5 [&>svg]:w-3.5 text-fg-subtle", className)}
    {...props}
  >
    {children ?? (
      <svg
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        className="rtl:rotate-180"
      >
        <polyline points="9 18 15 12 9 6" />
      </svg>
    )}
  </li>
);
BreadcrumbSeparator.displayName = "BreadcrumbSeparator";
