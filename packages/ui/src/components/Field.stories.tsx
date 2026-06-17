import type { Meta, StoryObj } from "@storybook/react";
import { Field } from "./Field";
import { Input } from "./Input";
import { Select } from "./Select";
import { Textarea } from "./Textarea";

const meta: Meta<typeof Field> = {
  title: "UI/Field",
  component: Field,
  parameters: { layout: "centered" },
  decorators: [
    (Story) => (
      <div className="w-80">
        <Story />
      </div>
    ),
  ],
};

export default meta;
type Story = StoryObj<typeof Field>;

// Field wraps a single control (Input/Select/Textarea), rendering the
// label, required marker, and help/error line, and wiring the
// accessibility relationships (htmlFor, aria-describedby, aria-invalid).
export const WithInput: Story = {
  args: {
    label: "Company name",
    help: "Shown on invoices and emails.",
    children: <Input placeholder="Acme Corp" />,
  },
};

export const Required: Story = {
  args: {
    label: "Email",
    required: true,
    children: <Input type="email" placeholder="you@example.com" />,
  },
};

export const WithError: Story = {
  args: {
    label: "Email",
    required: true,
    error: "Enter a valid email address.",
    children: <Input type="email" defaultValue="not-an-email" />,
  },
};

export const WithSelect: Story = {
  args: {
    label: "Currency",
    help: "Base currency for this tenant.",
    children: (
      <Select defaultValue="USD">
        <option value="USD">USD — US Dollar</option>
        <option value="EUR">EUR — Euro</option>
        <option value="GBP">GBP — Pound Sterling</option>
      </Select>
    ),
  },
};

export const WithTextarea: Story = {
  args: {
    label: "Notes",
    children: <Textarea rows={4} placeholder="Add context…" />,
  },
};

export const HiddenLabel: Story = {
  args: {
    label: "Search",
    hideLabel: true,
    children: <Input type="search" placeholder="Search…" />,
  },
};
