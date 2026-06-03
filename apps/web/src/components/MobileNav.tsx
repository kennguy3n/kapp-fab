import type { ReactNode } from "react";
import { Link, NavLink, useLocation } from "react-router-dom";
import { cn } from "@kapp/ui";

// The Records tab is a quick link to the primary record type, but it
// represents the whole Records section, so it stays highlighted on any
// `/records/*` route — not just this default type.
const RECORDS_DEFAULT = "/records/crm.lead";

/**
 * MobileNav is the bottom tab bar shown only on small viewports
 * (`md:hidden`) — the touch-friendly counterpart to the desktop
 * sidebar, which is hidden below the `md` breakpoint by AppShell.
 *
 * It exposes the five primary destinations from the responsive spec:
 * Dashboard, Records, Chat (a KChat deep link), Notifications, and
 * More. Dashboard is a router `NavLink` (active on the index route).
 * Records links to the primary record type but is highlighted for the
 * whole `/records/*` section. Chat is an external deep link into the
 * host KChat client. Notifications and
 * More are actions wired by AppShell to open a bottom sheet (the
 * notifications inbox and the full navigation menu respectively),
 * because neither has a dedicated route.
 */
export interface MobileNavProps {
  /** Deep-link target for the Chat tab (the host KChat client). */
  chatHref: string;
  /** Tapped the Notifications tab. */
  onNotificationsClick: () => void;
  /** Tapped the More tab. */
  onMoreClick: () => void;
  /** Which action sheet is currently open, for active styling. */
  activeSheet?: "notifications" | "more" | null;
}

const tabBase =
  "flex flex-1 flex-col items-center justify-center gap-0.5 py-1 text-[10px] font-medium transition-colors";
const tabInactive = "text-fg-muted hover:text-fg";
const tabActive = "text-accent";

function TabIcon({ children }: { children: ReactNode }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="h-5 w-5"
      aria-hidden="true"
    >
      {children}
    </svg>
  );
}

export function MobileNav({
  chatHref,
  onNotificationsClick,
  onMoreClick,
  activeSheet = null,
}: MobileNavProps) {
  const { pathname } = useLocation();
  // Highlight Records for the whole section, not just the default type.
  const recordsActive = pathname.startsWith("/records");
  return (
    <nav
      aria-label="Primary"
      className="fixed inset-x-0 bottom-0 z-40 flex h-16 border-t border-border bg-bg-elevated md:hidden"
    >
      <NavLink
        to="/"
        end
        className={({ isActive }) =>
          cn(tabBase, isActive ? tabActive : tabInactive)
        }
      >
        <TabIcon>
          <path d="M3 9.5 12 3l9 6.5" />
          <path d="M5 10v10h14V10" />
        </TabIcon>
        <span>Dashboard</span>
      </NavLink>

      {/* A plain Link (not NavLink): NavLink only marks itself active on
          a prefix match of its own `to`, but Records should highlight for
          the entire `/records/*` section regardless of the active type. */}
      <Link
        to={RECORDS_DEFAULT}
        aria-current={recordsActive ? "page" : undefined}
        className={cn(tabBase, recordsActive ? tabActive : tabInactive)}
      >
        <TabIcon>
          <rect x="4" y="3" width="16" height="18" rx="2" />
          <path d="M8 8h8M8 12h8M8 16h5" />
        </TabIcon>
        <span>Records</span>
      </Link>

      {/* Chat is an external deep link into the host KChat client, so
          it's a plain anchor (not a router NavLink) and never carries
          an in-app active state. */}
      <a href={chatHref} className={cn(tabBase, tabInactive)}>
        <TabIcon>
          <path d="M21 11.5a8.38 8.38 0 0 1-8.5 8.5 8.5 8.5 0 0 1-3.8-.9L3 21l1.9-5.7a8.5 8.5 0 0 1-.9-3.8 8.38 8.38 0 0 1 8.5-8.5 8.38 8.38 0 0 1 8.5 8.5z" />
        </TabIcon>
        <span>Chat</span>
      </a>

      <button
        type="button"
        onClick={onNotificationsClick}
        aria-pressed={activeSheet === "notifications"}
        className={cn(
          tabBase,
          activeSheet === "notifications" ? tabActive : tabInactive,
        )}
      >
        <TabIcon>
          <path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9" />
          <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        </TabIcon>
        <span>Notifications</span>
      </button>

      <button
        type="button"
        onClick={onMoreClick}
        aria-pressed={activeSheet === "more"}
        className={cn(
          tabBase,
          activeSheet === "more" ? tabActive : tabInactive,
        )}
      >
        <TabIcon>
          <circle cx="5" cy="12" r="1.5" />
          <circle cx="12" cy="12" r="1.5" />
          <circle cx="19" cy="12" r="1.5" />
        </TabIcon>
        <span>More</span>
      </button>
    </nav>
  );
}
