import { describe, it, expect, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import { server } from "../test/msw/server";
import { NotificationBell } from "./NotificationBell";

// NotificationBell fetches /api/v1/notifications directly (raw fetch),
// so the component is exercised end-to-end through MSW. The default
// handler returns an empty list; tests override it per-case.

const API = "/api/v1";

interface Notif {
  id: string;
  type: string;
  title: string;
  body: string;
  read: boolean;
  created_at: string;
}

function notif(over: Partial<Notif> = {}): Notif {
  return {
    id: "n1",
    type: "mention",
    title: "You were mentioned",
    body: "Acme deal needs review",
    read: false,
    created_at: "2024-01-01T00:00:00Z",
    ...over,
  };
}

function renderBell() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <NotificationBell />
    </QueryClientProvider>,
  );
}

describe("NotificationBell", () => {
  beforeEach(() => {
    localStorage.clear();
    localStorage.setItem("kapp.tenant", "acme");
    localStorage.setItem("kapp.token", "tok");
  });

  it("shows the unread count badge derived from the list", async () => {
    server.use(
      http.get(`${API}/notifications`, () =>
        HttpResponse.json([
          notif({ id: "n1", read: false }),
          notif({ id: "n2", read: false, title: "Second" }),
          notif({ id: "n3", read: true, title: "Old" }),
        ]),
      ),
    );
    renderBell();
    // Two of three are unread.
    await waitFor(() =>
      expect(screen.getByText("(2)")).toBeInTheDocument(),
    );
  });

  it("opens the dropdown and lists notification titles + bodies", async () => {
    server.use(
      http.get(`${API}/notifications`, () =>
        HttpResponse.json([notif({ title: "Approval needed", body: "PO-42" })]),
      ),
    );
    const user = userEvent.setup();
    renderBell();
    await waitFor(() => expect(screen.getByText("(1)")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: /Notifications/i }));
    expect(await screen.findByText("Approval needed")).toBeInTheDocument();
    expect(screen.getByText("PO-42")).toBeInTheDocument();
  });

  it("renders the empty-state copy when there are no notifications", async () => {
    // Default handler returns [].
    const user = userEvent.setup();
    renderBell();
    await user.click(screen.getByRole("button", { name: /Notifications/i }));
    expect(await screen.findByText(/No notifications\./i)).toBeInTheDocument();
  });

  it("marks a single notification read and refetches the list", async () => {
    let read = false;
    server.use(
      http.get(`${API}/notifications`, () =>
        HttpResponse.json([notif({ id: "n1", read })]),
      ),
      http.post(`${API}/notifications/n1/read`, () => {
        read = true;
        // 204 No Content carries no body — match the default handler
        // in test/msw/handlers.ts (bare HttpResponse, not .json(null)).
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderBell();
    await waitFor(() => expect(screen.getByText("(1)")).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: /Notifications/i }));
    // Exact "Mark read" (not "Mark all read") targets the single
    // per-notification action.
    await user.click(await screen.findByRole("button", { name: /^Mark read$/i }));

    // After the mutation invalidates the query, the refetch returns the
    // now-read notification, so the unread badge disappears.
    await waitFor(() => expect(screen.queryByText("(1)")).not.toBeInTheDocument());
  });

  it("stays functional (no badge) when the list request errors", async () => {
    server.use(
      http.get(`${API}/notifications`, () =>
        HttpResponse.json({ error: "boom" }, { status: 500 }),
      ),
    );
    renderBell();
    // The bell button always renders; no unread badge is shown on error.
    expect(
      await screen.findByRole("button", { name: /Notifications/i }),
    ).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByText(/^\(\d+\)$/)).not.toBeInTheDocument();
    });
  });

  it("sends the tenant + auth headers on the list request", async () => {
    let seen: Record<string, string | null> = {};
    server.use(
      http.get(`${API}/notifications`, ({ request }) => {
        seen = {
          tenant: request.headers.get("X-Tenant-ID"),
          auth: request.headers.get("Authorization"),
        };
        return HttpResponse.json([]);
      }),
    );
    renderBell();
    await waitFor(() => expect(seen.tenant).toBe("acme"));
    expect(seen.auth).toBe("Bearer tok");
  });
});
