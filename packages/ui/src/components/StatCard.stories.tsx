import type { Meta, StoryObj } from "@storybook/react";
import { DollarSign, Users, FileText } from "lucide-react";
import { StatCard } from "./StatCard";

const meta: Meta<typeof StatCard> = {
  title: "UI/StatCard",
  component: StatCard,
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj<typeof StatCard>;

export const Default: Story = {
  render: () => (
    <div className="w-64">
      <StatCard label="Outstanding AR" value="$42,180" icon={<DollarSign />} />
    </div>
  ),
};

export const WithTrends: Story = {
  render: () => (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
      <StatCard
        label="Revenue (MTD)"
        value="$128,400"
        icon={<DollarSign />}
        trend={{ direction: "up", value: "+12%" }}
        sub="vs. last month"
      />
      <StatCard
        label="New contacts"
        value="86"
        icon={<Users />}
        trend={{ direction: "down", value: "-4%" }}
        sub="vs. last month"
      />
      <StatCard
        label="Overdue invoices"
        value="7"
        icon={<FileText />}
        trend={{ direction: "down", value: "3 fewer", intent: "positive" }}
        sub="down is good"
      />
    </div>
  ),
};

export const AsLink: Story = {
  render: () => (
    <div className="w-64">
      <StatCard
        label="Open tickets"
        value="23"
        renderContainer={({ className, children }) => (
          <a href="#" className={className}>
            {children}
          </a>
        )}
      />
    </div>
  ),
};
