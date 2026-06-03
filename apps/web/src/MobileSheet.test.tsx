import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MobileSheet } from "./App";

const sections = [
  { title: "Overview", links: [{ to: "/", label: "Dashboard" }] },
  { title: "CRM", links: [{ to: "/records/crm.lead", label: "Leads" }] },
];

function renderSheet(onClose = vi.fn()) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const utils = render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/"]}>
        <MobileSheet mode="more" sections={sections} onClose={onClose} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...utils, onClose };
}

function renderNotificationsSheet(onClose = vi.fn()) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const utils = render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/"]}>
        <MobileSheet
          mode="notifications"
          sections={sections}
          onClose={onClose}
        />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...utils, onClose };
}

describe("MobileSheet (more mode)", () => {
  // Regression: the menu previously rendered SidebarGroup/SidebarItem,
  // which call useSidebar() and throw outside a <Sidebar> provider —
  // tapping "More" on mobile crashed the app.
  it("renders the nav menu outside any Sidebar context without crashing", () => {
    renderSheet();
    expect(screen.getByText("Overview")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Dashboard" })).toHaveAttribute(
      "href",
      "/",
    );
    expect(screen.getByRole("link", { name: "Leads" })).toHaveAttribute(
      "href",
      "/records/crm.lead",
    );
  });

  it("closes when a nav link is tapped", async () => {
    const user = userEvent.setup();
    const { onClose } = renderSheet();
    await user.click(screen.getByRole("link", { name: "Leads" }));
    expect(onClose).toHaveBeenCalled();
  });
});

describe("MobileSheet (notifications mode)", () => {
  // Regression: the sheet renders its own "Notifications" header, and the
  // embedded inbox used to render a second one — a duplicated heading.
  // The inbox is now passed showTitle={false}, so exactly one
  // "Notifications" heading appears in the sheet.
  it("shows a single 'Notifications' header (no duplicate from the inbox)", async () => {
    renderNotificationsSheet();
    expect(await screen.findAllByText("Notifications")).toHaveLength(1);
    // The inbox content still renders (its "Mark all read" action).
    expect(
      screen.getByRole("button", { name: /Mark all read/i }),
    ).toBeInTheDocument();
  });
});
