import type { Meta, StoryObj } from "@storybook/react";
import { Badge } from "./Badge";

const meta: Meta<typeof Badge> = {
  title: "UI/Badge",
  component: Badge,
  parameters: { layout: "centered" },
  argTypes: {
    variant: {
      control: "select",
      options: ["default", "accent", "success", "warning", "danger", "info", "outline"],
    },
    size: { control: "select", options: ["xs", "sm", "md"] },
  },
};

export default meta;
type Story = StoryObj<typeof Badge>;

export const Default: Story = { args: { children: "Draft" } };
export const Accent: Story = { args: { children: "Featured", variant: "accent" } };
export const Success: Story = { args: { children: "Paid", variant: "success" } };
export const Warning: Story = {
  args: { children: "Pending", variant: "warning" },
};
export const Danger: Story = { args: { children: "Overdue", variant: "danger" } };
export const Info: Story = { args: { children: "New", variant: "info" } };
export const Outline: Story = {
  args: { children: "Tag", variant: "outline" },
};

export const AllVariants: Story = {
  render: () => (
    <div className="flex flex-wrap items-center gap-2">
      <Badge>Default</Badge>
      <Badge variant="accent">Accent</Badge>
      <Badge variant="success">Success</Badge>
      <Badge variant="warning">Warning</Badge>
      <Badge variant="danger">Danger</Badge>
      <Badge variant="info">Info</Badge>
      <Badge variant="outline">Outline</Badge>
    </div>
  ),
};
