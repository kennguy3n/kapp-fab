import {
  createContext,
  forwardRef,
  useCallback,
  useContext,
  useState,
  type AnchorHTMLAttributes,
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
            "flex h-screen flex-col border-r border-border bg-bg-subtle text-fg",
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
   * react-router's `<NavLink>`).  Receives the resolved
   * className and the icon+label children to render.  When set,
   * `href` is ignored — the consumer's anchor owns navigation.
   */
  renderAnchor?: (args: {
    className: string;
    children: ReactNode;
  }) => ReactNode;
}

export const SidebarItem = forwardRef<HTMLAnchorElement, SidebarItemProps>(
  (
    { className, icon, active, label, badge, href, renderAnchor, ...props },
    ref,
  ) => {
    const { collapsed } = useSidebar();
    const resolvedClass = cn(
      "group flex items-center gap-2.5 rounded-md px-2 py-1.5 text-sm",
      "transition-colors",
      "hover:bg-bg-muted focus-visible:outline-none focus-visible:bg-bg-muted",
      active && "bg-accent/15 text-accent font-medium",
      !active && "text-fg-muted hover:text-fg",
      collapsed && "justify-center px-0",
      className,
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
      return <>{renderAnchor({ className: resolvedClass, children: contentChildren })}</>;
    }

    return (
      <a
        ref={ref}
        href={href}
        title={collapsed ? label : undefined}
        aria-label={collapsed ? label : undefined}
        className={resolvedClass}
        {...props}
      >
        {contentChildren}
      </a>
    );
  },
);
SidebarItem.displayName = "SidebarItem";

export interface SidebarToggleProps
  extends HTMLAttributes<HTMLButtonElement> {}

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
