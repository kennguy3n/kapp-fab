import type { Meta, StoryObj } from "@storybook/react";
import { Select } from "./Select";

const meta: Meta<typeof Select> = {
  title: "UI/Select",
  component: Select,
  parameters: { layout: "centered" },
  argTypes: {
    size: { control: "select", options: ["sm", "md", "lg"] },
    invalid: { control: "boolean" },
    disabled: { control: "boolean" },
  },
};

export default meta;
type Story = StoryObj<typeof Select>;

export const Default: Story = {
  render: (args) => (
    <div className="w-60">
      <Select {...args}>
        <option value="">Pick a country…</option>
        <option value="ch">Switzerland</option>
        <option value="de">Germany</option>
        <option value="sg">Singapore</option>
        <option value="ph">Philippines</option>
      </Select>
    </div>
  ),
};

export const Disabled: Story = {
  args: { disabled: true, value: "ch" },
  render: (args) => (
    <div className="w-60">
      <Select {...args}>
        <option value="ch">Switzerland</option>
      </Select>
    </div>
  ),
};

export const Invalid: Story = {
  args: { invalid: true },
  render: (args) => (
    <div className="w-60">
      <Select {...args}>
        <option value="">Pick a country…</option>
        <option value="ch">Switzerland</option>
      </Select>
    </div>
  ),
};
