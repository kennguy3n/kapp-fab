import type { Meta, StoryObj } from "@storybook/react";
import {
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  EmptyState,
  Skeleton,
  StatCard,
} from "@kapp/ui";
import { AlertTriangle } from "lucide-react";

/**
 * Composed page layout for design review — a static mirror of
 * `apps/web/src/pages/DashboardPage.tsx`.  It assembles the
 * design-system primitives (greeting header, Card section, the
 * eight-tile StatCard grid, the skeleton loader, and the error
 * EmptyState) with fixed mock data so the whole page can be
 * reviewed in Storybook without the React Query / router / API
 * plumbing the real page depends on.
 */
const meta: Meta = {
  title: "Pages/Dashboard",
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj;

interface Tile {
  label: string;
  value: string | number;
  sub?: string;
  trend?: { direction: "up" | "down" | "flat"; value: string; intent?: "positive" | "negative" | "neutral" };
}

const tiles: Tile[] = [
  {
    label: "Open deals",
    value: 42,
    sub: "Pipeline $1,284,000",
    trend: { direction: "up", value: "+8%" },
  },
  {
    label: "Outstanding AR",
    value: "$318,420",
    sub: "in USD",
    trend: { direction: "down", value: "3% lower", intent: "positive" },
  },
  { label: "Outstanding AP", value: "$112,900", sub: "in USD" },
  { label: "Low-stock items", value: 7 },
  { label: "Pending approvals", value: 5 },
  {
    label: "Open tickets",
    value: 23,
    sub: "4 overdue",
    trend: { direction: "up", value: "+2", intent: "negative" },
  },
  { label: "Present today", value: 38, sub: "hr.attendance — UTC day" },
  { label: "Pending reviews", value: 6, sub: "submitted + reviewed" },
];

function Greeting() {
  return (
    <header className="flex flex-col gap-1">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Good morning, Acme Corp
      </h1>
      <p className="text-sm text-fg-muted">
        Friday, 6 June 2026 · At-a-glance KPIs. Each tile links to the
        underlying worklist.
      </p>
    </header>
  );
}

export const Loaded: Story = {
  render: () => (
    <section className="flex flex-col gap-6">
      <Greeting />
      <Card>
        <CardHeader>
          <CardTitle>Key metrics</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-[repeat(auto-fill,minmax(220px,1fr))] gap-3">
            {tiles.map((t) => (
              <StatCard
                key={t.label}
                label={t.label}
                value={t.value}
                sub={t.sub}
                trend={t.trend}
                renderContainer={({ className, children }) => (
                  <a href="#" className={className}>
                    {children}
                  </a>
                )}
              />
            ))}
          </div>
        </CardContent>
      </Card>
    </section>
  ),
};

export const Loading: Story = {
  render: () => (
    <section className="flex flex-col gap-6">
      <Greeting />
      <Card>
        <CardHeader>
          <CardTitle>Key metrics</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-[repeat(auto-fill,minmax(220px,1fr))] gap-3">
            {Array.from({ length: 8 }).map((_, i) => (
              <div
                key={i}
                className="rounded-lg border border-border bg-bg-elevated p-4"
              >
                <Skeleton variant="text" className="h-4 w-24" />
                <Skeleton variant="text" className="mt-3 h-7 w-20" />
                <Skeleton variant="text" className="mt-2 h-3 w-28" />
              </div>
            ))}
          </div>
        </CardContent>
      </Card>
    </section>
  ),
};

export const ErrorState: Story = {
  render: () => (
    <section className="flex flex-col gap-6">
      <Greeting />
      <Card>
        <CardHeader>
          <CardTitle>Key metrics</CardTitle>
        </CardHeader>
        <CardContent>
          <EmptyState
            icon={<AlertTriangle />}
            title="Couldn't load the dashboard"
            description="Failed to load dashboard: network error"
            action={<Button variant="secondary">Retry</Button>}
          />
        </CardContent>
      </Card>
    </section>
  ),
};
