import type { Meta, StoryObj } from "@storybook/react";
import { Badge, type BadgeProps } from "./Badge";

const meta: Meta<typeof Badge> = {
  title: "UI/Badge",
  component: Badge,
  parameters: { layout: "centered" },
  argTypes: {
    variant: {
      control: "select",
      options: [
        "default",
        "neutral",
        "accent",
        "success",
        "warning",
        "danger",
        "info",
        "outline",
      ],
    },
    size: { control: "select", options: ["xs", "sm", "md"] },
  },
};

export default meta;
type Story = StoryObj<typeof Badge>;

export const Default: Story = { args: { children: "Draft" } };
export const Accent: Story = { args: { children: "Featured", variant: "accent" } };
export const Success: Story = { args: { children: "Paid", variant: "success" } };
export const Warning: Story = {
  args: { children: "Pending", variant: "warning" },
};
export const Danger: Story = { args: { children: "Overdue", variant: "danger" } };
export const Info: Story = { args: { children: "New", variant: "info" } };
export const Outline: Story = {
  args: { children: "Tag", variant: "outline" },
};

export const Neutral: Story = {
  args: { children: "Archived", variant: "neutral" },
};

export const AllVariants: Story = {
  render: () => (
    <div className="flex flex-wrap items-center gap-2">
      <Badge>Default</Badge>
      <Badge variant="neutral">Neutral</Badge>
      <Badge variant="accent">Accent</Badge>
      <Badge variant="success">Success</Badge>
      <Badge variant="warning">Warning</Badge>
      <Badge variant="danger">Danger</Badge>
      <Badge variant="info">Info</Badge>
      <Badge variant="outline">Outline</Badge>
    </div>
  ),
};

/**
 * Status → variant mapping. Badge is the workhorse for statuses across
 * the app; map a domain status to the semantic variant (never a raw
 * colour) so light/dark and future re-tunes stay consistent. See
 * packages/ui/THEME.md for the full table.
 */
export const StatusMapping: Story = {
  render: () => {
    const rows: { variant: BadgeProps["variant"]; statuses: string[] }[] = [
      { variant: "success", statuses: ["Active", "Paid", "Completed", "Approved"] },
      { variant: "warning", statuses: ["Pending", "Draft", "Low stock"] },
      { variant: "danger", statuses: ["Failed", "Overdue", "Suspended"] },
      { variant: "info", statuses: ["New", "Processing", "Scheduled"] },
      { variant: "accent", statuses: ["Featured"] },
      { variant: "neutral", statuses: ["Archived", "Closed", "N/A"] },
    ];
    return (
      <div className="flex flex-col gap-2">
        {rows.map((row) => (
          <div key={row.variant} className="flex flex-wrap items-center gap-2">
            {row.statuses.map((s) => (
              <Badge key={s} variant={row.variant}>
                {s}
              </Badge>
            ))}
          </div>
        ))}
      </div>
    );
  },
};
