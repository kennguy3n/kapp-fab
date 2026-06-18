import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Bell } from "lucide-react";
import { cn } from "@kapp/ui";

interface Notification {
  id: string;
  type: string;
  title: string;
  body: string;
  read: boolean;
  created_at: string;
}

const headers = (): HeadersInit => {
  const h: Record<string, string> = {
    "Content-Type": "application/json",
    "X-Tenant-ID": localStorage.getItem("kapp.tenant") ?? "default",
  };
  const t = localStorage.getItem("kapp.token");
  if (t) h.Authorization = `Bearer ${t}`;
  return h;
};

// In demo mode the mock layer installs a window.fetch shim that serves
// these notification routes from in-memory fixtures. api.ts installs it
// on boot, but the bell mounts on the app shell and fires its first
// fetch immediately — so ensure the (idempotent) shim is in place first,
// otherwise the cold load races the install and 500s through the proxy.
const demoMode = import.meta.env.VITE_DEMO_MODE === "true";
async function ensureDemoFetch(): Promise<void> {
  if (!demoMode) return;
  const { installPortalDemoFetch } = await import("../lib/mock-api");
  installPortalDemoFetch();
}

async function fetchNotifications(): Promise<Notification[]> {
  await ensureDemoFetch();
  const r = await fetch("/api/v1/notifications?limit=20", {
    headers: headers(),
  });
  if (!r.ok) throw new Error(`list notifications: ${r.status}`);
  return r.json();
}

async function markRead(id: string): Promise<void> {
  await ensureDemoFetch();
  const r = await fetch(`/api/v1/notifications/${id}/read`, {
    method: "POST",
    headers: headers(),
  });
  if (!r.ok) throw new Error(`mark read: ${r.status}`);
}

async function markAllRead(): Promise<void> {
  await ensureDemoFetch();
  const r = await fetch(`/api/v1/notifications/read-all`, {
    method: "POST",
    headers: headers(),
  });
  if (!r.ok) throw new Error(`mark all read: ${r.status}`);
}

/**
 * NotificationBell is the header-level inbox dropdown backed by the
 * notifications table (migrations/000014_notifications.sql). The worker
 * persists every notification envelope it sees, so this UI shows
 * everything the user has received even when the outbound transport
 * (KChat, webhook, email) failed.
 *
 * It stays a hand-rolled popover (rather than the shared DropdownMenu)
 * because the per-item "Mark read" / "Mark all read" actions must NOT
 * dismiss the panel — a menu would close on every select.  Styling is
 * driven entirely by design tokens so it tracks light/dark themes.
 */
export function NotificationBell() {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const list = useQuery({
    queryKey: ["notifications"],
    queryFn: fetchNotifications,
    refetchInterval: 30000,
  });
  const readOne = useMutation({
    mutationFn: markRead,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["notifications"] }),
  });
  const readAll = useMutation({
    mutationFn: markAllRead,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["notifications"] }),
  });

  // Dismiss on outside click / Escape so the panel behaves like a menu
  // without being one.
  useEffect(() => {
    if (!open) return;
    const onPointer = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const items = list.data ?? [];
  const unread = items.filter((n) => !n.read).length;

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-label="Notifications"
        aria-haspopup="true"
        aria-expanded={open}
        className={cn(
          "relative inline-flex h-9 w-9 items-center justify-center rounded-pill",
          "text-fg-muted transition-colors hover:bg-bg-subtle hover:text-fg",
          "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)",
        )}
      >
        <Bell className="h-5 w-5" />
        {unread > 0 && (
          <>
            <span
              className="absolute -right-0.5 -top-0.5 flex min-w-4 items-center justify-center rounded-pill bg-danger px-1 text-[10px] font-semibold leading-4 text-danger-fg"
              aria-hidden="true"
            >
              {unread > 9 ? "9+" : unread}
            </span>
            <span className="sr-only">({unread})</span>
          </>
        )}
      </button>
      {open && (
        <div
          role="dialog"
          aria-label="Notifications"
          className="absolute end-0 top-[calc(100%+8px)] z-50 max-h-[480px] w-80 overflow-y-auto rounded-lg border border-border bg-bg-elevated text-fg shadow-lg"
        >
          <div className="flex items-center justify-between border-b border-border px-3 py-2.5">
            <span className="text-sm font-medium">Notifications</span>
            <button
              type="button"
              onClick={() => readAll.mutate()}
              disabled={readAll.isPending || unread === 0}
              className="text-xs font-medium text-accent transition-colors hover:text-accent-hover disabled:cursor-not-allowed disabled:opacity-40"
            >
              Mark all read
            </button>
          </div>
          {items.length === 0 && (
            <div className="px-3 py-6 text-center text-sm text-fg-subtle">
              No notifications.
            </div>
          )}
          {items.map((n) => (
            <div
              key={n.id}
              className={cn(
                "border-b border-border px-3 py-2.5 last:border-b-0",
                !n.read && "bg-bg-subtle",
              )}
            >
              <div className="flex items-start justify-between gap-2">
                <span className="text-sm font-medium text-fg">
                  {n.title || n.type}
                </span>
                <span className="shrink-0 text-[11px] text-fg-subtle">
                  {new Date(n.created_at).toLocaleString()}
                </span>
              </div>
              {n.body && <p className="mt-1 text-sm text-fg-muted">{n.body}</p>}
              {!n.read && (
                <button
                  type="button"
                  onClick={() => readOne.mutate(n.id)}
                  disabled={readOne.isPending}
                  className="mt-1.5 text-xs font-medium text-accent transition-colors hover:text-accent-hover disabled:opacity-40"
                >
                  Mark read
                </button>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
