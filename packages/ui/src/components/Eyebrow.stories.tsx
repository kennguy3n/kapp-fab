import type { Meta, StoryObj } from "@storybook/react";
import { Eyebrow } from "./Eyebrow";

const meta: Meta<typeof Eyebrow> = {
  title: "UI/Eyebrow",
  component: Eyebrow,
  parameters: { layout: "centered" },
  args: { children: "Communities" },
};

export default meta;
type Story = StoryObj<typeof Eyebrow>;

// The KChat brand motif: a small monospace label with a leading
// underscore that categorises the section beneath it. The caller
// passes the bare word ("Communities") — the component owns the `_`.
export const Default: Story = {};

export const AboveHeading: Story = {
  render: () => (
    <div className="flex flex-col gap-2">
      <Eyebrow>Coming soon</Eyebrow>
      <h2 className="text-2xl">A self-service business suite</h2>
    </div>
  ),
};

export const Examples: Story = {
  render: () => (
    <div className="flex flex-col items-start gap-3">
      <Eyebrow>Overview</Eyebrow>
      <Eyebrow>Communities</Eyebrow>
      <Eyebrow>Coming soon</Eyebrow>
    </div>
  ),
};
