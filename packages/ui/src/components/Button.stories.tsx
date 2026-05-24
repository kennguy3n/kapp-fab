import type { Meta, StoryObj } from "@storybook/react";
import { Button } from "./Button";

const meta: Meta<typeof Button> = {
  title: "UI/Button",
  component: Button,
  parameters: { layout: "centered" },
  argTypes: {
    variant: {
      control: "select",
      options: ["primary", "secondary", "outline", "ghost", "link", "destructive"],
    },
    size: { control: "select", options: ["sm", "md", "lg", "icon"] },
    disabled: { control: "boolean" },
  },
};

export default meta;
type Story = StoryObj<typeof Button>;

export const Primary: Story = { args: { children: "Save", variant: "primary" } };
export const Secondary: Story = {
  args: { children: "Cancel", variant: "secondary" },
};
export const Outline: Story = {
  args: { children: "Discard", variant: "outline" },
};
export const Ghost: Story = { args: { children: "Toolbar action", variant: "ghost" } };
export const LinkVariant: Story = {
  args: { children: "Forgot password?", variant: "link" },
};
export const Destructive: Story = {
  args: { children: "Delete record", variant: "destructive" },
};

export const Small: Story = { args: { children: "Save", size: "sm" } };
export const Large: Story = { args: { children: "Save", size: "lg" } };

export const Disabled: Story = {
  args: { children: "Save", disabled: true },
};

export const WithLeadingIcon: Story = {
  args: {
    children: "Add row",
    leadingIcon: (
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
        <line x1="12" y1="5" x2="12" y2="19" />
        <line x1="5" y1="12" x2="19" y2="12" />
      </svg>
    ),
  },
};

export const AllVariants: Story = {
  render: () => (
    <div className="flex flex-wrap gap-2">
      <Button variant="primary">Primary</Button>
      <Button variant="secondary">Secondary</Button>
      <Button variant="outline">Outline</Button>
      <Button variant="ghost">Ghost</Button>
      <Button variant="link">Link</Button>
      <Button variant="destructive">Destructive</Button>
    </div>
  ),
};

export const AllSizes: Story = {
  render: () => (
    <div className="flex items-center gap-2">
      <Button size="sm">Small</Button>
      <Button size="md">Medium</Button>
      <Button size="lg">Large</Button>
      <Button size="icon" aria-label="Add">
        +
      </Button>
    </div>
  ),
};
