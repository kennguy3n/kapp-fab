import type { Meta, StoryObj } from "@storybook/react";
import { Inbox, SearchX } from "lucide-react";
import { EmptyState } from "./EmptyState";
import { Button } from "./Button";

const meta: Meta<typeof EmptyState> = {
  title: "UI/EmptyState",
  component: EmptyState,
  parameters: { layout: "centered" },
};

export default meta;
type Story = StoryObj<typeof EmptyState>;

export const NoRecords: Story = {
  render: () => (
    <div className="w-96 rounded-lg border border-border">
      <EmptyState
        icon={<Inbox />}
        title="No invoices yet"
        description="Create your first invoice to start tracking receivables."
        action={<Button>New invoice</Button>}
      />
    </div>
  ),
};

export const NoResults: Story = {
  render: () => (
    <div className="w-96 rounded-lg border border-border">
      <EmptyState
        icon={<SearchX />}
        title="No results"
        description="No records match your filters. Try broadening your search."
      />
    </div>
  ),
};
