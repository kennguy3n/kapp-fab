import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { MobileNav } from "./MobileNav";

function renderNav(
  props: Partial<React.ComponentProps<typeof MobileNav>> = {},
  initialEntries: string[] = ["/"],
) {
  const onNotificationsClick = props.onNotificationsClick ?? vi.fn();
  const onMoreClick = props.onMoreClick ?? vi.fn();
  const utils = render(
    <MemoryRouter initialEntries={initialEntries}>
      <MobileNav
        chatHref="kchat://team"
        onNotificationsClick={onNotificationsClick}
        onMoreClick={onMoreClick}
        {...props}
      />
    </MemoryRouter>,
  );
  return { ...utils, onNotificationsClick, onMoreClick };
}

describe("MobileNav", () => {
  it("renders the five primary tabs", () => {
    renderNav();
    for (const label of [
      "Dashboard",
      "Records",
      "Chat",
      "Notifications",
      "More",
    ]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it("renders Chat as an external deep link to the host KChat client", () => {
    renderNav({ chatHref: "kchat://team" });
    const chat = screen.getByRole("link", { name: /Chat/i });
    expect(chat).toHaveAttribute("href", "kchat://team");
  });

  it("marks the Dashboard tab active on the index route", () => {
    renderNav({}, ["/"]);
    const dashboard = screen.getByRole("link", { name: /Dashboard/i });
    expect(dashboard).toHaveAttribute("aria-current", "page");
    const records = screen.getByRole("link", { name: /Records/i });
    expect(records).not.toHaveAttribute("aria-current");
  });

  it("marks the Records tab active on a records route", () => {
    renderNav({}, ["/records/crm.lead"]);
    const records = screen.getByRole("link", { name: /Records/i });
    expect(records).toHaveAttribute("aria-current", "page");
    const dashboard = screen.getByRole("link", { name: /Dashboard/i });
    expect(dashboard).not.toHaveAttribute("aria-current");
  });

  it("invokes the Notifications and More callbacks on tap", async () => {
    const user = userEvent.setup();
    const { onNotificationsClick, onMoreClick } = renderNav();

    await user.click(screen.getByRole("button", { name: /Notifications/i }));
    expect(onNotificationsClick).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: /More/i }));
    expect(onMoreClick).toHaveBeenCalledTimes(1);
  });

  it("reflects the open sheet via aria-pressed", () => {
    renderNav({ activeSheet: "more" });
    expect(screen.getByRole("button", { name: /More/i })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(
      screen.getByRole("button", { name: /Notifications/i }),
    ).toHaveAttribute("aria-pressed", "false");
  });
});
