import type { Meta, StoryObj } from "@storybook/react";
import { Input } from "./Input";

const meta: Meta<typeof Input> = {
  title: "UI/Input",
  component: Input,
  parameters: { layout: "centered" },
  argTypes: {
    size: { control: "select", options: ["sm", "md", "lg"] },
    invalid: { control: "boolean" },
    disabled: { control: "boolean" },
    placeholder: { control: "text" },
  },
};

export default meta;
type Story = StoryObj<typeof Input>;

export const Default: Story = {
  args: { placeholder: "Type something…" },
  render: (args) => (
    <div className="w-72">
      <Input {...args} />
    </div>
  ),
};

export const Invalid: Story = {
  args: { placeholder: "name@example.com", invalid: true, defaultValue: "not an email" },
  render: (args) => (
    <div className="w-72">
      <Input {...args} />
    </div>
  ),
};

export const Disabled: Story = {
  args: { placeholder: "Read only", disabled: true, value: "Disabled value" },
  render: (args) => (
    <div className="w-72">
      <Input {...args} />
    </div>
  ),
};

export const Sizes: Story = {
  render: () => (
    <div className="flex w-72 flex-col gap-2">
      <Input size="sm" placeholder="Small" />
      <Input size="md" placeholder="Medium" />
      <Input size="lg" placeholder="Large" />
    </div>
  ),
};

const SearchIcon = (
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth="2"
    strokeLinecap="round"
    strokeLinejoin="round"
    className="h-4 w-4"
  >
    <circle cx="11" cy="11" r="8" />
    <line x1="21" y1="21" x2="16.65" y2="16.65" />
  </svg>
);

export const WithLeadingAddon: Story = {
  render: () => (
    <div className="w-72">
      <Input leadingAddon={SearchIcon} placeholder="Search records…" />
    </div>
  ),
};

export const WithTrailingAddon: Story = {
  render: () => (
    <div className="w-72">
      <Input
        placeholder="Filter…"
        trailingAddon={
          <span className="text-xs font-medium">⌘K</span>
        }
      />
    </div>
  ),
};
