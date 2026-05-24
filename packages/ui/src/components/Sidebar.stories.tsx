import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import {
  Sidebar,
  SidebarBody,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarItem,
  SidebarToggle,
} from "./Sidebar";
import { Avatar, AvatarFallback } from "./Avatar";
import { Badge } from "./Badge";

const meta: Meta<typeof Sidebar> = {
  title: "UI/Sidebar",
  component: Sidebar,
  parameters: { layout: "fullscreen" },
};

export default meta;
type Story = StoryObj<typeof Sidebar>;

const HomeIcon = (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <path d="M3 9l9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
    <polyline points="9 22 9 12 15 12 15 22" />
  </svg>
);
const FolderIcon = (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z" />
  </svg>
);
const ChartIcon = (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
  >
    <line x1="18" y1="20" x2="18" y2="10" />
    <line x1="12" y1="20" x2="12" y2="4" />
    <line x1="6" y1="20" x2="6" y2="14" />
  </svg>
);

export const Default: Story = {
  render: () => (
    <div className="flex h-screen">
      <Sidebar>
        <SidebarHeader>
          <div className="flex h-7 w-7 items-center justify-center rounded bg-accent text-accent-fg font-bold">
            K
          </div>
          <span className="font-semibold">Kapp</span>
          <div className="ml-auto">
            <SidebarToggle />
          </div>
        </SidebarHeader>
        <SidebarBody>
          <SidebarGroup title="Overview">
            <SidebarItem icon={HomeIcon} label="Dashboard" active href="#" />
            <SidebarItem icon={ChartIcon} label="Insights" href="#" />
          </SidebarGroup>
          <SidebarGroup title="Work">
            <SidebarItem icon={FolderIcon} label="Records" href="#" />
            <SidebarItem
              icon={FolderIcon}
              label="Approvals"
              href="#"
              badge={<Badge variant="accent" size="xs">3</Badge>}
            />
          </SidebarGroup>
        </SidebarBody>
        <SidebarFooter>
          <Avatar size="sm">
            <AvatarFallback>KN</AvatarFallback>
          </Avatar>
          <span className="text-sm truncate flex-1">Ken Nguyen</span>
        </SidebarFooter>
      </Sidebar>
      <main className="flex-1 p-6">
        <h1 className="text-lg font-semibold">Page content</h1>
        <p className="text-sm text-fg-muted mt-1">
          The sidebar collapses via the toggle in the header.
        </p>
      </main>
    </div>
  ),
};

export const Collapsed: Story = {
  render: () => {
    const [collapsed, setCollapsed] = useState(true);
    return (
      <div className="flex h-screen">
        <Sidebar collapsed={collapsed} onCollapsedChange={setCollapsed}>
          <SidebarHeader>
            <div className="flex h-7 w-7 items-center justify-center rounded bg-accent text-accent-fg font-bold">
              K
            </div>
            {!collapsed && <span className="font-semibold">Kapp</span>}
            <div className={collapsed ? "" : "ml-auto"}>
              <SidebarToggle />
            </div>
          </SidebarHeader>
          <SidebarBody>
            <SidebarGroup title="Overview">
              <SidebarItem icon={HomeIcon} label="Dashboard" active href="#" />
              <SidebarItem icon={ChartIcon} label="Insights" href="#" />
              <SidebarItem icon={FolderIcon} label="Records" href="#" />
            </SidebarGroup>
          </SidebarBody>
        </Sidebar>
        <main className="flex-1 p-6">
          <h1 className="text-lg font-semibold">Collapsed view</h1>
        </main>
      </div>
    );
  },
};
