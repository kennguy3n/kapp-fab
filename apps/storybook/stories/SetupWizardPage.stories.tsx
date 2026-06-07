import type { ReactNode } from "react";
import type { Meta, StoryObj } from "@storybook/react";
import {
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  Input,
  Select,
  Stepper,
} from "@kapp/ui";
import { CheckCircle2 } from "lucide-react";

/**
 * Composed page layout for design review — a static mirror of
 * `apps/web/src/pages/SetupWizardPage.tsx`.  It assembles the
 * Stepper progress indicator with the per-step Card (company
 * details, CoA template, users) and the success completion screen.
 * Each story pins the wizard to one step with fixed mock data so
 * the layout can be reviewed without the tenant-bootstrap / API
 * plumbing the real page needs.
 */
const meta: Meta = {
  title: "Pages/Setup Wizard",
  parameters: { layout: "padded" },
};

export default meta;
type Story = StoryObj;

const steps = [
  { label: "Company" },
  { label: "Chart of accounts" },
  { label: "Users" },
  { label: "Done" },
];

function Shell({
  current,
  children,
}: {
  current: number;
  children: ReactNode;
}) {
  return (
    <div className="mx-auto flex max-w-2xl flex-col gap-6">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">
        Set up your workspace
      </h1>
      <Stepper current={current} steps={steps} />
      {children}
    </div>
  );
}

export const StepCompany: Story = {
  render: () => (
    <Shell current={0}>
      <Card>
        <CardHeader>
          <CardTitle>Company details</CardTitle>
          <CardDescription>
            Tell us who you are. These details seed the company profile and
            drive the country-specific defaults on the next step.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <label className="flex flex-col gap-1.5">
            <span className="text-sm font-medium text-fg">Company name</span>
            <Input defaultValue="Acme Corp" />
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-sm font-medium text-fg">Industry</span>
            <Input placeholder="e.g. Software, Retail" />
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-sm font-medium text-fg">Country</span>
            <Input placeholder="ISO country code or name" defaultValue="US" />
          </label>
          <label className="flex flex-col gap-1.5">
            <span className="text-sm font-medium text-fg">Language</span>
            <Select defaultValue="en-US" aria-label="Language">
              <option value="en-US">English (United States)</option>
              <option value="pt-BR">Português (Brasil)</option>
              <option value="fr-CA">Français (Canada)</option>
            </Select>
          </label>
          <div className="flex justify-end">
            <Button>Next</Button>
          </div>
        </CardContent>
      </Card>
    </Shell>
  ),
};

const coaTemplates = [
  { id: "us-gaap", name: "United States — US GAAP", region: "Americas" },
  { id: "ca", name: "Canada — ASPE", region: "Americas" },
  { id: "uk", name: "United Kingdom — FRS 102", region: "Europe" },
  { id: "de", name: "Germany — HGB (SKR03)", region: "Europe" },
  { id: "ae", name: "United Arab Emirates — IFRS", region: "GCC" },
];

export const StepChartOfAccounts: Story = {
  render: () => (
    <Shell current={1}>
      <Card>
        <CardHeader>
          <CardTitle>Chart of accounts</CardTitle>
          <CardDescription>
            Pick a localized template to seed your ledger. You can customize
            individual accounts later.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <Input placeholder="Search templates…" type="search" />
          <fieldset className="flex flex-col gap-2">
            {coaTemplates.map((tpl, i) => (
              <label
                key={tpl.id}
                className="flex items-center gap-3 rounded-md border border-border px-3 py-2 text-sm has-[:checked]:border-accent has-[:checked]:bg-bg-subtle"
              >
                <input
                  type="radio"
                  name="coa"
                  defaultChecked={i === 0}
                  className="accent-(--accent)"
                />
                <span className="font-medium text-fg">{tpl.name}</span>
                <span className="ml-auto text-xs text-fg-subtle">
                  {tpl.region}
                </span>
              </label>
            ))}
          </fieldset>
          <div className="flex justify-between">
            <Button type="button" variant="outline">
              Back
            </Button>
            <Button type="button">Next</Button>
          </div>
        </CardContent>
      </Card>
    </Shell>
  ),
};

export const Completion: Story = {
  render: () => (
    <Shell current={3}>
      <Card>
        <CardContent className="flex flex-col items-center gap-4 py-10 text-center">
          <span className="text-success">
            <CheckCircle2 className="h-14 w-14" aria-hidden />
          </span>
          <div className="flex flex-col gap-1">
            <h2 className="text-xl font-semibold text-fg">You're all set</h2>
            <p className="text-sm text-fg-muted">
              Acme Corp is ready. Your chart of accounts is seeded and your
              team has been invited.
            </p>
          </div>
          <Button>Go to dashboard</Button>
        </CardContent>
      </Card>
    </Shell>
  ),
};
