import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { MobileSheet } from "./App";

const sections = [
  { title: "Overview", links: [{ to: "/", label: "Dashboard" }] },
  { title: "CRM", links: [{ to: "/records/crm.lead", label: "Leads" }] },
];

function renderSheet(onClose = vi.fn()) {
  const utils = render(
    <MemoryRouter initialEntries={["/"]}>
      <MobileSheet mode="more" sections={sections} onClose={onClose} />
    </MemoryRouter>,
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
