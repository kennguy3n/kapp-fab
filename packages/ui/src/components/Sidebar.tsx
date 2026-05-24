import {
  createContext,
  forwardRef,
  useCallback,
  useContext,
  useState,
  type AnchorHTMLAttributes,
  type ButtonHTMLAttributes,
  type ForwardedRef,
  type HTMLAttributes,
  type ReactNode,
} from "react";
import { cn } from "../lib/cn";

/**
 * Sidebar is the shell-level navigation primitive — a fixed-width
 * column with a collapsible compact mode, grouped link sections,
 * and active-state highlighting.  The design rationale:
 *
 *   1. **Composable structure**.  Callers compose `<Sidebar>`
 *      around `<SidebarHeader>`, `<SidebarBody>`, `<SidebarGroup>`,
 *      `<SidebarItem>`, and `<SidebarFooter>` so the visual
 *      shell is consistent but content (logo, search, sections,
 *      user menu) lives in app code.  This avoids the failure
 *      mode where the design system tries to own every product
 *      detail and ends up with a 30-prop monolith.
 *
 *   2. **Active state via NavLink-style render prop**.  The
 *      `<SidebarItem>` doesn't import react-router-dom (the
 *      design system stays router-agnostic).  Instead it takes
 *      a render prop or an `active` boolean, so callers pass
 *      the active state from their router's matcher.  See
 *      apps/web/src/components/AppSidebar.tsx for the wiring.
 *
 *   3. **Collapse state via context, not props**.  The compact
 *      mode toggle is owned by Sidebar (Context provider) so
 *      individual items can react to it (hide labels, show
 *      tooltips on hover) without prop-drilling.  Callers can
 *      override with `defaultCollapsed` or fully-controlled
 *      `collapsed` + `onCollapsedChange`.
 *
 *   4. **Layout-only**.  Sidebar deliberately does not render
 *      Link components — it's chrome.  The link/route binding
 *      is the caller's job.
 */

interface SidebarContextValue {
  collapsed: boolean;
  setCollapsed: (next: boolean) => void;
}

const SidebarContext = createContext<SidebarContextValue | null>(null);

function useSidebar() {
  const ctx = useContext(SidebarContext);
  if (!ctx) {
    throw new Error("Sidebar subcomponents must be used inside <Sidebar>");
  }
  return ctx;
}

export interface SidebarProps extends HTMLAttributes<HTMLElement> {
  /** Default collapsed state for uncontrolled usage. */
  defaultCollapsed?: boolean;
  /** Controlled collapsed state — pairs with `onCollapsedChange`. */
  collapsed?: boolean;
  onCollapsedChange?: (next: boolean) => void;
}

export const Sidebar = forwardRef<HTMLElement, SidebarProps>(
  (
    {
      className,
      defaultCollapsed = false,
      collapsed: controlledCollapsed,
      onCollapsedChange,
      children,
      ...props
    },
    ref,
  ) => {
    const [uncontrolled, setUncontrolled] = useState(defaultCollapsed);
    const collapsed = controlledCollapsed ?? uncontrolled;
    const setCollapsed = useCallback(
      (next: boolean) => {
        if (onCollapsedChange) onCollapsedChange(next);
        if (controlledCollapsed === undefined) setUncontrolled(next);
      },
      [controlledCollapsed, onCollapsedChange],
    );

    return (
      <SidebarContext.Provider value={{ collapsed, setCollapsed }}>
        <aside
          ref={ref}
          data-collapsed={collapsed || undefined}
          className={cn(
            // border-e is the inline-end logical border: it
            // renders as border-right in LTR and border-left in
            // RTL automatically when <html dir="rtl">. The flex
            // row containing this aside also flips visually in
            // RTL — the sidebar moves to the right edge of the
            // viewport and the main content fills the left,
            // mirroring the LTR layout. PR-6 adds a Playwright
            // test that pins this exact flip.
            "flex h-screen flex-col border-e border-border bg-bg-subtle text-fg",
            "transition-[width] duration-200",
            collapsed ? "w-14" : "w-60",
            className,
          )}
          {...props}
        >
          {children}
        </aside>
      </SidebarContext.Provider>
    );
  },
);
Sidebar.displayName = "Sidebar";

export const SidebarHeader = forwardRef<
  HTMLDivElement,
  HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn(
      "flex h-14 shrink-0 items-center gap-2 border-b border-border px-3",
      className,
    )}
    {...props}
  />
));
SidebarHeader.displayName = "SidebarHeader";

export const SidebarBody = forwardRef<
  HTMLDivElement,
  HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn("flex-1 overflow-y-auto px-2 py-3", className)}
    {...props}
  />
));
SidebarBody.displayName = "SidebarBody";

export const SidebarFooter = forwardRef<
  HTMLDivElement,
  HTMLAttributes<HTMLDivElement>
>(({ className, ...props }, ref) => (
  <div
    ref={ref}
    className={cn(
      "flex shrink-0 items-center gap-2 border-t border-border p-3",
      className,
    )}
    {...props}
  />
));
SidebarFooter.displayName = "SidebarFooter";

export interface SidebarGroupProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "title"> {
  /**
   * Group heading rendered above the items.  Hidden when the
   * sidebar is collapsed (compact items don't need section
   * labels — tooltips on hover convey grouping).  Typed as
   * ReactNode to allow icons / badges in the heading.  We omit
   * the native HTML `title` attribute from the spread because
   * it's a string-only DOM attribute and would shadow our
   * structured prop.
   */
  title: ReactNode;
}

export const SidebarGroup = forwardRef<HTMLDivElement, SidebarGroupProps>(
  ({ className, title, children, ...props }, ref) => {
    const { collapsed } = useSidebar();
    return (
      <div
        ref={ref}
        className={cn("mb-3 flex flex-col gap-0.5", className)}
        {...props}
      >
        {!collapsed && (
          <div className="px-2 pb-1 pt-2 text-[10px] font-medium uppercase tracking-wider text-fg-subtle">
            {title}
          </div>
        )}
        {children}
      </div>
    );
  },
);
SidebarGroup.displayName = "SidebarGroup";

export interface SidebarItemProps
  extends Omit<AnchorHTMLAttributes<HTMLAnchorElement>, "children"> {
  icon?: ReactNode;
  /**
   * When true, applies the active styling (accent background +
   * darker foreground).  Wire from your router (e.g. NavLink
   * isActive callback) or your own matcher logic.
   */
  active?: boolean;
  /** Used as the visible label and aria-label when collapsed. */
  label: string;
  /**
   * Optional badge rendered to the right of the label.  Hidden
   * when the sidebar is collapsed; the tooltip shows the
   * underlying count in that mode.
   */
  badge?: ReactNode;
  /**
   * When provided, this is used as the underlying anchor's href.
   * The component is router-agnostic: callers using react-router
   * should pass the `<NavLink>` href themselves and handle
   * client-side navigation via their own anchor wrapper (we
   * accept any standard anchor handlers via the spread).
   */
  href?: string;
  /**
   * Optional render prop to inject a router-aware anchor (e.g.
   * react-router's `<NavLink>`).  When set, `href` is ignored —
   * the consumer's anchor owns navigation.
   *
   * The callback receives:
   *
   *   - `getClassName(active)` — a function that returns the
   *     correct merged class string for a given active state.
   *     This is exposed (rather than a static `className` string)
   *     so consumers wiring react-router's `<NavLink>` can defer
   *     the active-state decision to NavLink's own
   *     `isActive` callback, and so the resulting class string is
   *     run through tailwind-merge (via `cn`) which resolves
   *     conflicts between the inactive base classes (e.g.
   *     `text-fg-muted hover:text-fg`) and the active overrides
   *     (`text-accent`) deterministically.  String-concatenating
   *     active classes onto an inactive base would leave the
   *     `hover:text-fg` rule live and the foreground colour would
   *     flip on hover — a regression the static `className`
   *     contract makes very easy to write.
   *
   *   - `ref` — the forwarded ref passed to `<SidebarItem>`.  We
   *     expose it so consumers can attach it to their anchor and
   *     honour the `forwardRef` contract.  Refs passed via the
   *     non-render-prop path go onto the internal `<a>` as
   *     before.
   *
   *   - `children` — the resolved icon + label tree.
   */
  renderAnchor?: (args: {
    getClassName: (active?: boolean) => string;
    ref: ForwardedRef<HTMLAnchorElement>;
    children: ReactNode;
  }) => ReactNode;
}

export const SidebarItem = forwardRef<HTMLAnchorElement, SidebarItemProps>(
  (
    { className, icon, active, label, badge, href, renderAnchor, ...props },
    ref,
  ) => {
    const { collapsed } = useSidebar();
    /**
     * Builds the resolved className for the item given an active
     * state.  Pulled out into a function (rather than computed
     * once at the top of the render) because the render-prop path
     * needs to defer the active decision to react-router's
     * `NavLink isActive` callback — the parent component can't
     * know whether the route matches without consulting the
     * router.  Defining it inline keeps SidebarItem the single
     * owner of the class composition so callers can't drift the
     * active styling away from the inactive base.
     */
    const getClassName = useCallback(
      (isActive?: boolean): string =>
        cn(
          "group flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm",
          "transition-colors",
          "hover:bg-bg-muted focus-visible:outline-none focus-visible:bg-bg-muted",
          isActive
            ? "bg-accent/15 text-accent font-medium hover:text-accent"
            : "text-fg-muted hover:text-fg",
          collapsed && "justify-center px-0",
          className,
        ),
      [collapsed, className],
    );

    const contentChildren = (
      <>
        {icon && (
          <span className="flex h-4 w-4 shrink-0 items-center justify-center">
            {icon}
          </span>
        )}
        {!collapsed && <span className="truncate flex-1">{label}</span>}
        {!collapsed && badge && (
          <span className="shrink-0 text-xs">{badge}</span>
        )}
      </>
    );

    if (renderAnchor) {
      // Honour the forwardRef contract on the render-prop path:
      // pass the ref through so consumers can attach it to their
      // anchor (e.g. react-router NavLink ref) instead of
      // discarding it.  Without this, parents calling
      // `useRef<HTMLAnchorElement>()` on SidebarItem would see
      // their ref silently stay null.
      return (
        <>
          {renderAnchor({
            getClassName,
            ref,
            children: contentChildren,
          })}
        </>
      );
    }

    return (
      <a
        ref={ref}
        href={href}
        title={collapsed ? label : undefined}
        aria-label={collapsed ? label : undefined}
        className={getClassName(active)}
        {...props}
      >
        {contentChildren}
      </a>
    );
  },
);
SidebarItem.displayName = "SidebarItem";

// SidebarToggle is a real <button>, so the prop bag must accept the
// button-specific attributes a consumer is likely to pass (disabled,
// form, formAction, name, value, etc.).  HTMLAttributes covers only the
// generic-element set and would type-error on those.  We hard-code
// type="button" inside the component so callers can't override it to
// "submit" by accident and submit the parent form when expanding the
// sidebar.
export interface SidebarToggleProps
  extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, "type"> {}

export const SidebarToggle = forwardRef<HTMLButtonElement, SidebarToggleProps>(
  ({ className, ...props }, ref) => {
    const { collapsed, setCollapsed } = useSidebar();
    return (
      <button
        ref={ref}
        type="button"
        aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
        onClick={() => setCollapsed(!collapsed)}
        className={cn(
          "inline-flex h-8 w-8 items-center justify-center rounded-md",
          "text-fg-muted hover:bg-bg-muted hover:text-fg",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
          className,
        )}
        {...props}
      >
        <svg
          aria-hidden="true"
          className="h-4 w-4"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          {collapsed ? (
            <polyline points="9 18 15 12 9 6" />
          ) : (
            <polyline points="15 18 9 12 15 6" />
          )}
        </svg>
      </button>
    );
  },
);
SidebarToggle.displayName = "SidebarToggle";
