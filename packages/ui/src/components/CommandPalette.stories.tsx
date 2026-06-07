import type { Meta, StoryObj } from "@storybook/react";
import { useState } from "react";
import {
  LayoutDashboard,
  Users,
  FileText,
  Settings,
  Plus,
} from "lucide-react";
import { CommandPalette } from "./CommandPalette";
import { Button } from "./Button";

const meta: Meta = {
  title: "UI/CommandPalette",
};

export default meta;
type Story = StoryObj;

export const Default: Story = {
  render: () => {
    const [open, setOpen] = useState(false);
    const noop = () => setOpen(false);
    return (
      <div>
        <Button onClick={() => setOpen(true)}>Open (⌘K)</Button>
        <CommandPalette
          open={open}
          onOpenChange={setOpen}
          groups={[
            {
              heading: "Navigation",
              items: [
                {
                  id: "dashboard",
                  label: "Dashboard",
                  icon: <LayoutDashboard />,
                  hint: "Overview",
                  onSelect: noop,
                },
                {
                  id: "contacts",
                  label: "Contacts",
                  icon: <Users />,
                  hint: "CRM",
                  onSelect: noop,
                },
                {
                  id: "invoices",
                  label: "Invoices",
                  icon: <FileText />,
                  hint: "Finance",
                  onSelect: noop,
                },
              ],
            },
            {
              heading: "Actions",
              items: [
                {
                  id: "new-contact",
                  label: "Create new contact",
                  icon: <Plus />,
                  keywords: ["add", "new"],
                  onSelect: noop,
                },
                {
                  id: "settings",
                  label: "Go to settings",
                  icon: <Settings />,
                  onSelect: noop,
                },
              ],
            },
          ]}
        />
      </div>
    );
  },
};
