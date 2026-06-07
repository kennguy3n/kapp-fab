import type { Meta, StoryObj } from "@storybook/react";
import { Skeleton } from "./Skeleton";

const meta: Meta<typeof Skeleton> = {
  title: "UI/Skeleton",
  component: Skeleton,
  parameters: { layout: "centered" },
  argTypes: {
    variant: { control: "select", options: ["rect", "circle", "text"] },
  },
};

export default meta;
type Story = StoryObj<typeof Skeleton>;

export const Rect: Story = {
  args: { variant: "rect", className: "h-24 w-64" },
};

export const Circle: Story = {
  args: { variant: "circle", className: "h-12 w-12" },
};

export const TextLine: Story = {
  args: { variant: "text", className: "w-48" },
};

export const CardPlaceholder: Story = {
  render: () => (
    <div className="w-72 rounded-lg border border-border p-4">
      <div className="flex items-center gap-3">
        <Skeleton variant="circle" className="h-10 w-10" />
        <div className="flex-1 space-y-2">
          <Skeleton variant="text" className="w-3/4" />
          <Skeleton variant="text" className="w-1/2" />
        </div>
      </div>
      <Skeleton variant="rect" className="mt-4 h-24 w-full" />
    </div>
  ),
};
