import type { Meta, StoryObj } from "@storybook/react";
import { Textarea } from "./Textarea";

const meta: Meta<typeof Textarea> = {
  title: "UI/Textarea",
  component: Textarea,
  parameters: { layout: "centered" },
  argTypes: {
    invalid: { control: "boolean" },
    disabled: { control: "boolean" },
  },
  args: {
    placeholder: "Add a note…",
    rows: 4,
  },
  render: (args) => (
    <div className="w-80">
      <Textarea {...args} />
    </div>
  ),
};

export default meta;
type Story = StoryObj<typeof Textarea>;

export const Default: Story = {};

export const Invalid: Story = { args: { invalid: true } };

export const Disabled: Story = {
  args: { disabled: true, defaultValue: "Read-only content" },
};
