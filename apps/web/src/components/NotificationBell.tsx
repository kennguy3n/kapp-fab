import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

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

async function fetchNotifications(): Promise<Notification[]> {
  const r = await fetch("/api/v1/notifications?limit=20", {
    headers: headers(),
  });
  if (!r.ok) throw new Error(`list notifications: ${r.status}`);
  return r.json();
}

async function markRead(id: string): Promise<void> {
  const r = await fetch(`/api/v1/notifications/${id}/read`, {
    method: "POST",
    headers: headers(),
  });
  if (!r.ok) throw new Error(`mark read: ${r.status}`);
}

async function markAllRead(): Promise<void> {
  const r = await fetch(`/api/v1/notifications/read-all`, {
    method: "POST",
    headers: headers(),
  });
  if (!r.ok) throw new Error(`mark all read: ${r.status}`);
}

/**
 * Shared notifications query. Both the header bell (badge) and the inbox
 * panel read from one React Query cache entry (`["notifications"]`), so
 * there's a single network subscription and one source of truth for read
 * state no matter how many consumers are mounted. Kept free of mutations
 * so the badge-only consumer (the bell) doesn't allocate useMutation
 * instances it never invokes.
 */
function useNotificationsQuery() {
  const list = useQuery({
    queryKey: ["notifications"],
    queryFn: fetchNotifications,
    refetchInterval: 30000,
  });
  const items = list.data ?? [];
  const unread = items.filter((n) => !n.read).length;
  return { items, unread };
}

/**
 * NotificationInbox renders just the inbox contents (optional header +
 * list) as a plain in-flow block, with no popover positioning of its
 * own. It's the single source of truth for the inbox UI, embedded both
 * inside the header bell's dropdown and directly in the mobile
 * notifications sheet (where a popover-trigger button would be the wrong
 * surface — the sheet IS the inbox).
 *
 * `showTitle` (default true) controls the "Notifications" heading. The
 * desktop dropdown has no title of its own so it keeps the heading; the
 * mobile sheet already renders a "Notifications" header, so it passes
 * `false` to avoid a duplicated title (the "Mark all read" action stays
 * in the row regardless).
 */
export function NotificationInbox({
  showTitle = true,
}: {
  showTitle?: boolean;
}) {
  const qc = useQueryClient();
  const { items, unread } = useNotificationsQuery();
  const readOne = useMutation({
    mutationFn: markRead,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["notifications"] }),
  });
  const readAll = useMutation({
    mutationFn: markAllRead,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["notifications"] }),
  });

  return (
    <div>
      <div
        style={{
          padding: 10,
          display: "flex",
          justifyContent: showTitle ? "space-between" : "flex-end",
          borderBottom: "1px solid #e5e7eb",
        }}
      >
        {showTitle && <strong>Notifications</strong>}
        <button
          onClick={() => readAll.mutate()}
          disabled={readAll.isPending || unread === 0}
          style={{ fontSize: 12 }}
        >
          Mark all read
        </button>
      </div>
      {items.length === 0 && (
        <div style={{ padding: 12, color: "#9ca3af", fontStyle: "italic" }}>
          No notifications.
        </div>
      )}
      {items.map((n) => (
        <div
          key={n.id}
          style={{
            padding: 10,
            borderBottom: "1px solid #f3f4f6",
            background: n.read ? "white" : "#f9fafb",
          }}
        >
          <div
            style={{
              display: "flex",
              justifyContent: "space-between",
              gap: 8,
            }}
          >
            <strong style={{ fontSize: 13 }}>{n.title || n.type}</strong>
            <span style={{ fontSize: 11, color: "#6b7280" }}>
              {new Date(n.created_at).toLocaleString()}
            </span>
          </div>
          {n.body && <p style={{ margin: "4px 0", fontSize: 13 }}>{n.body}</p>}
          {!n.read && (
            <button
              onClick={() => readOne.mutate(n.id)}
              style={{ fontSize: 11 }}
            >
              Mark read
            </button>
          )}
        </div>
      ))}
    </div>
  );
}

/**
 * NotificationBell is the header-level inbox dropdown backed by the
 * notifications table (migrations/000014_notifications.sql). The worker
 * persists every notification envelope it sees, so this UI shows
 * everything the user has received even when the outbound transport
 * (KChat, webhook, email) failed. The dropdown body reuses
 * {@link NotificationInbox} so the header and the mobile sheet stay in
 * sync.
 */
export function NotificationBell() {
  const [open, setOpen] = useState(false);
  const { unread } = useNotificationsQuery();

  return (
    <div style={{ position: "relative" }}>
      <button
        onClick={() => setOpen((v) => !v)}
        style={{
          background: "transparent",
          border: "1px solid #e5e7eb",
          borderRadius: 6,
          padding: "4px 10px",
          cursor: "pointer",
        }}
        aria-label="Notifications"
      >
        Bell {unread > 0 && <strong>({unread})</strong>}
      </button>
      {open && (
        <div
          style={{
            position: "absolute",
            right: 0,
            top: "calc(100% + 4px)",
            width: 360,
            background: "white",
            border: "1px solid #e5e7eb",
            borderRadius: 6,
            boxShadow: "0 4px 12px rgba(0,0,0,0.08)",
            zIndex: 50,
            maxHeight: 480,
            overflowY: "auto",
          }}
        >
          <NotificationInbox />
        </div>
      )}
    </div>
  );
}
