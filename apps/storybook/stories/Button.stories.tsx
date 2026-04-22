import type { Meta, StoryObj } from "@storybook/react";
import { Button } from "@kapp/ui";

const meta: Meta<typeof Button> = {
  title: "UI/Button",
  component: Button,
};

export default meta;
type Story = StoryObj<typeof Button>;

export const Primary: Story = { args: { children: "Save", variant: "primary" } };
export const Secondary: Story = { args: { children: "Cancel", variant: "secondary" } };
export const Ghost: Story = { args: { children: "Discard", variant: "ghost" } };
