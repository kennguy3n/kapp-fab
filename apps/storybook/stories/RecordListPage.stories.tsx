import type { Meta, StoryObj } from "@storybook/react";
import {
  Badge,
  Button,
  EmptyState,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { Inbox, Plus } from "lucide-react";

/**
 * Composed page layout for design review — a static mirror of
 * `apps/web/src/pages/RecordListPage.tsx`.  It assembles the list
 * header (saved-view Select + action Buttons), the records Table,
 * the floating bulk-action toolbar, and the empty states with
 * fixed mock data, so the layout can be reviewed without the
 * router / React Query / saved-view plumbing the real page needs.
 */
const meta: Meta = {
  title: "Pages/Record List",
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj;

interface Lead {
  name: string;
  company: string;
  stage: string;
  owner: string;
  value: string;
}

const leads: Lead[] = [
  { name: "Renewal — Globex", company: "Globex", stage: "Negotiation", owner: "J. Rivera", value: "$48,000" },
  { name: "Net-new — Initech", company: "Initech", stage: "Qualified", owner: "A. Chen", value: "$12,500" },
  { name: "Upsell — Hooli", company: "Hooli", stage: "Proposal", owner: "M. Okafor", value: "$96,200" },
  { name: "Pilot — Stark Ind.", company: "Stark Industries", stage: "Discovery", owner: "L. Park", value: "$8,000" },
  { name: "Expansion — Wayne", company: "Wayne Enterprises", stage: "Won", owner: "J. Rivera", value: "$210,000" },
];

const stageVariant = (stage: string) =>
  stage === "Won" ? "success" : stage === "Negotiation" ? "warning" : "outline";

function Header() {
  return (
    <header className="flex flex-wrap items-center justify-between gap-3">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">Leads</h1>
      <div className="flex flex-wrap items-center gap-2">
        <label className="flex items-center gap-2 text-sm text-fg-muted">
          View:
          <Select size="sm" aria-label="Saved view" className="w-auto" defaultValue="all">
            <option value="all">All records</option>
            <option value="mine">My open leads (default)</option>
            <option value="team">Team pipeline — shared</option>
          </Select>
        </label>
        <Button size="sm" variant="secondary">
          Save view
        </Button>
        <Button size="sm" leadingIcon={<Plus className="h-4 w-4" />}>
          New
        </Button>
      </div>
    </header>
  );
}

export const Loaded: Story = {
  render: () => (
    <section className="min-w-0 flex-1">
      <Header />
      <div className="mt-4">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Company</TableHead>
              <TableHead>Stage</TableHead>
              <TableHead>Owner</TableHead>
              <TableHead className="text-right">Value</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {leads.map((l) => (
              <TableRow key={l.name}>
                <TableCell className="font-medium text-fg">{l.name}</TableCell>
                <TableCell>{l.company}</TableCell>
                <TableCell>
                  <Badge variant={stageVariant(l.stage)}>{l.stage}</Badge>
                </TableCell>
                <TableCell>{l.owner}</TableCell>
                <TableCell className="text-right font-tabular">{l.value}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <div
        role="toolbar"
        aria-label="Bulk actions"
        className="sticky bottom-4 z-10 mt-3 flex items-center gap-2 rounded-lg border border-border bg-bg-elevated px-3 py-2 shadow-lg"
      >
        <span className="text-sm font-medium text-fg">2 selected</span>
        <Button size="sm" variant="secondary">
          Change Status
        </Button>
        <Button size="sm" variant="destructive">
          Delete
        </Button>
        <Button size="sm" variant="secondary">
          Export CSV
        </Button>
        <Button size="sm" variant="ghost">
          Clear
        </Button>
      </div>
    </section>
  ),
};

export const EmptyKType: Story = {
  render: () => (
    <section className="min-w-0 flex-1">
      <Header />
      <div className="mt-4">
        <EmptyState
          icon={<Inbox />}
          title="No Leads records yet"
          description="Create your first one to get started."
          action={
            <Button leadingIcon={<Plus className="h-4 w-4" />}>New Lead</Button>
          }
        />
      </div>
    </section>
  ),
};

export const FilteredEmpty: Story = {
  render: () => (
    <section className="min-w-0 flex-1">
      <Header />
      <div className="mt-4">
        <EmptyState
          icon={<Inbox />}
          title="No matching Leads records"
          description="No records match this view's filters."
          action={
            <Button leadingIcon={<Plus className="h-4 w-4" />}>New Lead</Button>
          }
        />
      </div>
    </section>
  ),
};
