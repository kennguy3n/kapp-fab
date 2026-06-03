import { NavLink } from "react-router-dom";
import { cn } from "@kapp/ui";
import { NotificationInbox } from "./NotificationBell";

/**
 * The minimal nav shape MobileSheet needs to render the "more" menu.
 * Declared locally (rather than imported from App) so this component —
 * and its test — don't pull in the entire App module graph. App's
 * richer `NavSection` (which also carries feature-gate metadata) is
 * structurally assignable to this.
 */
export interface MobileSheetSection {
  title: string;
  links: { to: string; label: string }[];
}

/**
 * MobileSheet is the slide-up panel opened from the bottom MobileNav.
 * Two modes:
 *   - "more": the full navigation menu (the sidebar's content), since
 *     the sidebar itself is hidden on mobile. Tapping any link closes
 *     the sheet (handled by the click bubbling to the list wrapper).
 *   - "notifications": the notification inbox (reuses NotificationInbox
 *     so there's a single source of truth for the inbox UI).
 *
 * Rendered only on mobile (`md:hidden`) — on larger viewports the
 * sidebar/header already expose these surfaces.
 */
export function MobileSheet({
  mode,
  sections,
  onClose,
}: {
  mode: "notifications" | "more";
  sections: MobileSheetSection[];
  onClose: () => void;
}) {
  return (
    <div className="fixed inset-0 z-50 md:hidden" role="dialog" aria-modal="true">
      <button
        type="button"
        aria-label="Close menu"
        onClick={onClose}
        className="absolute inset-0 h-full w-full bg-black/40"
      />
      <div className="absolute inset-x-0 bottom-0 flex max-h-[80vh] flex-col rounded-t-2xl border-t border-border bg-bg-elevated pb-16 shadow-2xl">
        <div className="flex h-12 shrink-0 items-center justify-between border-b border-border px-4">
          <span className="font-semibold">
            {mode === "more" ? "Menu" : "Notifications"}
          </span>
          <button
            type="button"
            onClick={onClose}
            aria-label="Close"
            className="rounded-md p-1 text-fg-muted hover:text-fg"
          >
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
              <line x1="18" y1="6" x2="6" y2="18" />
              <line x1="6" y1="6" x2="18" y2="18" />
            </svg>
          </button>
        </div>
        <div className="flex-1 overflow-y-auto p-3">
          {mode === "notifications" ? (
            // The sheet IS the inbox surface, so embed the inbox list
            // directly. Rendering <NotificationBell /> here would nest a
            // popover-trigger button (which opens its own absolutely-
            // positioned dropdown) inside the sheet — the wrong control
            // for a full-screen panel. `showTitle={false}` drops the
            // inbox's own "Notifications" heading since the sheet header
            // above already provides one (keeps "Mark all read").
            <NotificationInbox showTitle={false} />
          ) : (
            // Tapping any nav link navigates and closes the sheet — the
            // click bubbles up to this wrapper's handler. These are plain
            // NavLinks (not SidebarGroup/SidebarItem): those call
            // useSidebar() and throw outside a <Sidebar> provider, and the
            // sheet is a sibling of the sidebar, not a descendant.
            <div onClick={onClose}>
              {sections.map((section) => (
                <div key={section.title} className="mb-3">
                  <p className="px-2 pb-1 text-xs font-semibold uppercase tracking-wide text-fg-muted">
                    {section.title}
                  </p>
                  {section.links.map((link) => (
                    <NavLink
                      key={link.to}
                      to={link.to}
                      className={({ isActive }) =>
                        cn(
                          "block rounded-md px-2 py-2 text-sm font-medium transition-colors",
                          isActive
                            ? "bg-accent/10 text-accent"
                            : "text-fg hover:bg-bg-muted",
                        )
                      }
                    >
                      {link.label}
                    </NavLink>
                  ))}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
